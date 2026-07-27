// Package api exposes the sync protocol over HTTP. The contract it implements
// is documented in docs/PROTOCOL.md, which is the source of truth for any
// client — this code is one implementation of that document, not the spec.
package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/schmidt-gabriel/homesync/server/internal/index"
	"github.com/schmidt-gabriel/homesync/server/internal/store"
	"github.com/schmidt-gabriel/homesync/server/internal/trash"
)

// Server wires the index, the data store and the trash to HTTP.
type Server struct {
	index  *index.Index
	store  *store.Store
	trash  *trash.Trash
	events *broadcaster
	mux    *http.ServeMux

	// admin is nil unless EnableAdmin was called; the management routes are
	// not registered at all on a server without an admin password.
	admin *admin
}

// New builds the router and starts forwarding index changes to SSE clients.
func New(ix *index.Index, st *store.Store, tr *trash.Trash) *Server {
	s := &Server{
		index:  ix,
		store:  st,
		trash:  tr,
		events: newBroadcaster(),
		mux:    http.NewServeMux(),
	}
	s.routes()

	// Every committed mutation — whether it came from a client or from
	// fsnotify spotting an out-of-band write — wakes the connected machines.
	ix.OnChange(s.events.publish)

	return s
}

func (s *Server) routes() {
	// Unauthenticated: the container healthcheck must work without a token.
	s.mux.HandleFunc("GET /healthz", s.handleHealth)

	s.mux.HandleFunc("GET /v1/changes", s.requireAuth(s.handleChanges))
	s.mux.HandleFunc("GET /v1/events", s.requireAuth(s.handleEvents))

	s.mux.HandleFunc("GET /v1/files/{path...}", s.requireAuth(s.handleGetFile))
	s.mux.HandleFunc("HEAD /v1/files/{path...}", s.requireAuth(s.handleGetFile))
	s.mux.HandleFunc("PUT /v1/files/{path...}", s.requireAuth(s.handlePutFile))
	s.mux.HandleFunc("DELETE /v1/files/{path...}", s.requireAuth(s.handleDeleteFile))

	s.mux.HandleFunc("PUT /v1/dirs/{path...}", s.requireAuth(s.handlePutDir))
	// Same handler as files: it already branches on the entry type. Exposed
	// here too so directories are removed through the endpoint that creates
	// them, rather than the contract having an asymmetry to explain.
	s.mux.HandleFunc("DELETE /v1/dirs/{path...}", s.requireAuth(s.handleDeleteFile))

	s.mux.HandleFunc("GET /v1/ignore", s.requireAuth(s.handleGetIgnore))
	s.mux.HandleFunc("PUT /v1/ignore", s.requireAuth(s.handlePutIgnore))

	s.mux.HandleFunc("GET /v1/trash", s.requireAuth(s.handleListTrash))
	s.mux.HandleFunc("POST /v1/trash/restore", s.requireAuth(s.handleRestoreTrash))
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

	// ServeMux would answer a path containing ".." with a 307 to the cleaned
	// location. That is safe, but a redirect is a poor contract for a PUT and
	// an odd thing for a client author to have to handle. Reject outright.
	if hasDotSegment(r.URL.EscapedPath()) {
		writeError(rec, http.StatusBadRequest, "invalid_path", "path may not contain . or .. segments")
		return
	}

	s.mux.ServeHTTP(rec, r)

	slog.Debug("request",
		"method", r.Method, "path", r.URL.Path,
		"status", rec.status, "ms", time.Since(start).Milliseconds())
}

// hasDotSegment reports whether any segment of an escaped path is "." or "..".
// Percent-encoded forms are handled downstream by index.CleanPath, which
// decodes before validating.
func hasDotSegment(escapedPath string) bool {
	for _, segment := range strings.Split(escapedPath, "/") {
		if segment == "." || segment == ".." {
			return true
		}
	}
	return false
}

type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if r.wroteHeader {
		return
	}
	r.status = code
	r.wroteHeader = true
	r.ResponseWriter.WriteHeader(code)
}

// Flush and Unwrap keep SSE working through the wrapper.
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	rev, err := s.index.CurrentRev(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "index unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "rev": rev})
}

// errorResponse is the single error shape every endpoint returns, so a client
// can parse failures without special-casing each route.
type errorResponse struct {
	Error   string `json:"error"`   // stable machine-readable code
	Message string `json:"message"` // human-readable detail
	// Conflict carries the generated filename when Error is "conflict".
	Conflict string `json:"conflict,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		slog.Error("cannot encode response", "err", err)
	}
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, errorResponse{Error: code, Message: message})
}
