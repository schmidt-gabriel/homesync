package api

import (
	"net/http"
	"strconv"

	"github.com/schmidt-gabriel/homesync/server/internal/index"
)

// defaultChangesLimit caps one page. A client that has been away for a long
// time pages through with the rev of the last entry it received.
const (
	defaultChangesLimit = 1000
	maxChangesLimit     = 10000
)

type changesResponse struct {
	Changes []index.Entry `json:"changes"`
	// CurrentRev is the newest revision the server holds. A client that has
	// consumed every page stores this and asks again from here.
	CurrentRev int64 `json:"current_rev"`
	// More is true when the page was truncated: call again with since set to
	// the rev of the last entry.
	More bool `json:"more"`
}

// handleChanges is the heart of the protocol. A client remembers one number —
// the last revision it saw — and asks what happened after it. Deletions come
// back as tombstones, so a machine that was offline learns about them too.
func (s *Server) handleChanges(w http.ResponseWriter, r *http.Request) {
	device, _ := DeviceFrom(r.Context())

	since := int64(0)
	if raw := r.URL.Query().Get("since"); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || parsed < 0 {
			writeError(w, http.StatusBadRequest, "invalid_since", "since must be a non-negative integer")
			return
		}
		since = parsed
	}

	limit := defaultChangesLimit
	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 {
			writeError(w, http.StatusBadRequest, "invalid_limit", "limit must be a positive integer")
			return
		}
		limit = min(parsed, maxChangesLimit)
	}

	// Ask for one extra row to learn whether another page exists without a
	// second count query.
	entries, currentRev, err := s.index.Changes(r.Context(), device.Scope, since, limit+1)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "cannot read changes")
		return
	}

	more := len(entries) > limit
	if more {
		entries = entries[:limit]
	}

	// Paths go out relative to the device's scope, so a client never learns
	// that it is not sitting at the root of the data directory.
	writeJSON(w, http.StatusOK, changesResponse{
		Changes: device.stripEntries(entries), CurrentRev: currentRev, More: more,
	})
}
