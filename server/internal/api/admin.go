package api

import (
	"crypto/subtle"
	"embed"
	"encoding/json"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/schmidt-gabriel/homesync/server/internal/index"
)

//go:embed ui
var uiFiles embed.FS

// sessionTTL is how long an admin login lasts before it must be repeated.
const sessionTTL = 12 * time.Hour

const sessionCookie = "homesync_admin"

// admin holds the state for the management UI. It is entirely separate from
// device tokens: a device token syncs files and can do nothing else, and the
// admin password manages the server and cannot sync.
type admin struct {
	password string

	mu       sync.Mutex
	sessions map[string]time.Time // token -> expiry
}

// EnableAdmin turns on the web UI. Without a password the routes are never
// registered at all, so an unconfigured server exposes no management surface.
func (s *Server) EnableAdmin(password string) {
	s.admin = &admin{password: password, sessions: make(map[string]time.Time)}
	s.adminRoutes()
}

func (a *admin) newSession() (string, error) {
	token, err := NewToken()
	if err != nil {
		return "", err
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	// Opportunistic cleanup — the map only grows on login, so this is enough.
	now := time.Now()
	for existing, expiry := range a.sessions {
		if expiry.Before(now) {
			delete(a.sessions, existing)
		}
	}
	a.sessions[token] = now.Add(sessionTTL)
	return token, nil
}

func (a *admin) valid(token string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()

	expiry, ok := a.sessions[token]
	if !ok {
		return false
	}
	if expiry.Before(time.Now()) {
		delete(a.sessions, token)
		return false
	}
	return true
}

func (a *admin) revoke(token string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.sessions, token)
}

func (s *Server) adminRoutes() {
	s.mux.HandleFunc("POST /admin/api/login", s.handleAdminLogin)
	s.mux.HandleFunc("POST /admin/api/logout", s.handleAdminLogout)
	s.mux.HandleFunc("GET /admin/api/session", s.handleAdminSession)

	s.mux.HandleFunc("GET /admin/api/overview", s.requireAdmin(s.handleAdminOverview))
	s.mux.HandleFunc("GET /admin/api/devices", s.requireAdmin(s.handleAdminListDevices))
	s.mux.HandleFunc("POST /admin/api/devices", s.requireAdmin(s.handleAdminCreateDevice))
	s.mux.HandleFunc("DELETE /admin/api/devices/{name}", s.requireAdmin(s.handleAdminDeleteDevice))
	s.mux.HandleFunc("PUT /admin/api/devices/{name}/scope", s.requireAdmin(s.handleAdminSetScope))
	s.mux.HandleFunc("GET /admin/api/files", s.requireAdmin(s.handleAdminBrowse))
	s.mux.HandleFunc("GET /admin/api/trash", s.requireAdmin(s.handleListTrash))
	s.mux.HandleFunc("POST /admin/api/trash/restore", s.requireAdmin(s.handleRestoreTrash))
	// Admin only, and not exposed to devices: emptying the trash is the one
	// thing here that cannot be undone.
	s.mux.HandleFunc("DELETE /admin/api/trash", s.requireAdmin(s.handleAdminEmptyTrash))
	s.mux.HandleFunc("GET /admin/api/ignore", s.requireAdmin(s.handleGetIgnore))
	s.mux.HandleFunc("PUT /admin/api/ignore", s.requireAdmin(s.handlePutIgnore))

	ui, err := fs.Sub(uiFiles, "ui")
	if err != nil {
		panic("embedded ui missing: " + err.Error())
	}

	// The page is compiled into the binary, so it changes exactly when the
	// server is upgraded — and without this, a browser happily keeps showing
	// the previous version afterwards, which looks like the upgrade did not
	// take. It is a few kilobytes served over a local network; there is
	// nothing to gain by caching it.
	s.mux.Handle("GET /", noCache(http.FileServerFS(ui)))
}

func noCache(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
		w.Header().Set("Pragma", "no-cache")
		w.Header().Set("Expires", "0")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(sessionCookie)
		if err != nil || !s.admin.valid(cookie.Value) {
			writeError(w, http.StatusUnauthorized, "unauthorized", "admin session required")
			return
		}
		next(w, r)
	}
}

type loginRequest struct {
	Password string `json:"password"`
}

func (s *Server) handleAdminLogin(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", "expected {\"password\": \"...\"}")
		return
	}

	// Constant-time so a wrong password cannot be found one character at a time.
	if subtle.ConstantTimeCompare([]byte(req.Password), []byte(s.admin.password)) != 1 {
		// A deliberate pause blunts online guessing without needing rate-limit
		// state; the admin logs in rarely enough not to notice.
		time.Sleep(500 * time.Millisecond)
		writeError(w, http.StatusUnauthorized, "unauthorized", "wrong password")
		return
	}

	token, err := s.admin.newSession()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "cannot create session")
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   r.TLS != nil,
		MaxAge:   int(sessionTTL.Seconds()),
	})
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

func (s *Server) handleAdminLogout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(sessionCookie); err == nil {
		s.admin.revoke(cookie.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: "", Path: "/", HttpOnly: true, MaxAge: -1,
	})
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

// handleAdminSession lets the page decide whether to show the login form
// without having to provoke a 401 first.
func (s *Server) handleAdminSession(w http.ResponseWriter, r *http.Request) {
	cookie, err := r.Cookie(sessionCookie)
	authenticated := err == nil && s.admin.valid(cookie.Value)
	writeJSON(w, http.StatusOK, map[string]any{"authenticated": authenticated})
}

type overviewResponse struct {
	Stats        index.Stats `json:"stats"`
	Devices      int         `json:"devices"`
	TrashItems   int         `json:"trash_items"`
	TrashSize    int64       `json:"trash_size"`
	LastScanAt   int64       `json:"last_scan_at"`
	LastScanInfo string      `json:"last_scan_info"`
	DataDir      string      `json:"data_dir"`
	Secure       bool        `json:"secure"`
}

func (s *Server) handleAdminOverview(w http.ResponseWriter, r *http.Request) {
	stats, err := s.index.Stats(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "cannot read stats")
		return
	}

	devices, err := ListDevices(r.Context(), s.index.DB())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "cannot list devices")
		return
	}

	items, err := s.trash.List()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "cannot read trash")
		return
	}
	var trashSize int64
	for _, item := range items {
		trashSize += item.Size
	}

	rawScanAt, _ := s.index.GetMeta(r.Context(), index.MetaLastScanAt, "0")
	scanAt, _ := strconv.ParseInt(rawScanAt, 10, 64)
	scanInfo, _ := s.index.GetMeta(r.Context(), index.MetaLastScanStats, "")

	writeJSON(w, http.StatusOK, overviewResponse{
		Stats:        stats,
		Devices:      len(devices),
		TrashItems:   len(items),
		TrashSize:    trashSize,
		LastScanAt:   scanAt,
		LastScanInfo: scanInfo,
		DataDir:      s.store.Root(),
		Secure:       r.TLS != nil,
	})
}

type adminDevice struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Scope    string `json:"scope"`
	LastSeen int64  `json:"last_seen"`
	Created  int64  `json:"created_at"`
}

func (s *Server) handleAdminListDevices(w http.ResponseWriter, r *http.Request) {
	rows, err := s.index.DB().QueryContext(r.Context(),
		`SELECT id, name, scope, created_at, COALESCE(last_seen, 0)
         FROM devices ORDER BY created_at DESC`)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "cannot list devices")
		return
	}
	defer rows.Close()

	devices := []adminDevice{}
	for rows.Next() {
		var d adminDevice
		if err := rows.Scan(&d.ID, &d.Name, &d.Scope, &d.Created, &d.LastSeen); err != nil {
			writeError(w, http.StatusInternalServerError, "internal", "cannot read devices")
			return
		}
		devices = append(devices, d)
	}

	writeJSON(w, http.StatusOK, map[string]any{"devices": devices})
}

type createDeviceRequest struct {
	Name string `json:"name"`
	// Optional. Defaults to a directory named after the device.
	Scope *string `json:"scope"`
}

func (s *Server) handleAdminCreateDevice(w http.ResponseWriter, r *http.Request) {
	var req createDeviceRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil || req.Name == "" {
		writeError(w, http.StatusBadRequest, "invalid_body", "expected {\"name\": \"...\"}")
		return
	}

	scope := DefaultScope(req.Name)
	if req.Scope != nil {
		validated, err := ValidateScope(*req.Scope)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_scope", err.Error())
			return
		}
		scope = validated
	}

	token, err := AddDevice(r.Context(), s.index.DB(), req.Name, scope)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "cannot create device")
		return
	}

	// Created up front so the folder is visible on the server straight away,
	// rather than appearing only once the device happens to upload something.
	if scope != "" {
		if err := s.store.Mkdir(scope); err != nil {
			slog.Warn("cannot create scope directory", "scope", scope, "err", err)
		}
	}

	// The only time the plaintext token ever leaves the server. Only its hash
	// is stored, so this response cannot be reproduced later.
	writeJSON(w, http.StatusCreated, map[string]any{
		"name": req.Name, "token": token, "scope": scope})
}

func (s *Server) handleAdminDeleteDevice(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	removed, err := RemoveDevice(r.Context(), s.index.DB(), name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "cannot remove device")
		return
	}
	if removed == 0 {
		writeError(w, http.StatusNotFound, "not_found", "no such device")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"removed": removed})
}

type scopeRequest struct {
	Scope string `json:"scope"`
}

func (s *Server) handleAdminSetScope(w http.ResponseWriter, r *http.Request) {
	var req scopeRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", "expected {\"scope\": \"...\"}")
		return
	}

	scope, err := ValidateScope(req.Scope)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_scope", err.Error())
		return
	}

	updated, err := SetScope(r.Context(), s.index.DB(), r.PathValue("name"), scope)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "cannot update the scope")
		return
	}
	if updated == 0 {
		writeError(w, http.StatusNotFound, "not_found", "no such device")
		return
	}

	if scope != "" {
		if err := s.store.Mkdir(scope); err != nil {
			slog.Warn("cannot create scope directory", "scope", scope, "err", err)
		}
	}

	// The device's view of the tree just changed underneath it, so it has to
	// reconcile from scratch. Its own state database still points at the old
	// scope's revisions, which is why this is worth stating in the response.
	writeJSON(w, http.StatusOK, map[string]any{
		"scope":  scope,
		"notice": "the device will resync from scratch the next time it connects",
	})
}

func (s *Server) handleAdminEmptyTrash(w http.ResponseWriter, r *http.Request) {
	removed, err := s.trash.Empty()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "cannot empty the trash")
		return
	}

	slog.Info("trash emptied from the admin page", "items", removed)
	writeJSON(w, http.StatusOK, map[string]any{"removed": removed})
}

func (s *Server) handleAdminBrowse(w http.ResponseWriter, r *http.Request) {
	prefix := r.URL.Query().Get("prefix")
	if prefix != "" {
		cleaned, err := index.CleanPath(prefix)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_path", "bad prefix")
			return
		}
		prefix = cleaned
	}

	entries, err := s.index.Browse(r.Context(), prefix, 500)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "cannot browse index")
		return
	}

	// Flag entries the index knows about but that are missing from disk — the
	// clearest early signal that something is wrong with the volume.
	type browseEntry struct {
		index.Entry
		OnDisk bool `json:"on_disk"`
	}
	result := make([]browseEntry, 0, len(entries))
	for _, e := range entries {
		_, statErr := s.store.Stat(e.Path)
		result = append(result, browseEntry{Entry: e, OnDisk: !os.IsNotExist(statErr)})
	}

	writeJSON(w, http.StatusOK, map[string]any{"prefix": prefix, "entries": result})
}
