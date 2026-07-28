package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/schmidt-gabriel/homesync/server/internal/index"
	"github.com/schmidt-gabriel/homesync/server/internal/trash"
)

// ── Trash ────────────────────────────────────────────────────────────────────

type trashResponse struct {
	Items []trash.Item `json:"items"`
}

func (s *Server) handleListTrash(w http.ResponseWriter, r *http.Request) {
	items, err := s.trash.List()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "cannot read trash")
		return
	}

	// A device sees only what it could have deleted, with paths relative to its
	// own scope. The admin UI reaches this handler with no device attached, so
	// the scope is empty there and it sees everything, which is what it is for.
	device, isDevice := DeviceFrom(r.Context())
	if isDevice && device.Scope != "" {
		visible := make([]trash.Item, 0, len(items))
		for _, item := range items {
			relative, ok := device.strip(item.Path)
			if !ok {
				continue
			}
			item.Path = relative
			visible = append(visible, item)
		}
		items = visible
	}

	writeJSON(w, http.StatusOK, trashResponse{Items: items})
}

type restoreRequest struct {
	ID string `json:"id"`
}

func (s *Server) handleRestoreTrash(w http.ResponseWriter, r *http.Request) {
	var req restoreRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", "expected {\"id\": \"...\"}")
		return
	}

	item, err := s.trash.Lookup(req.ID)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "no such trash item")
		return
	}

	// The id encodes the original full path, so the item is restored where it
	// came from. A device may only restore inside its own scope: without this,
	// a valid id from another device's subtree would be a way to reach it.
	if device, isDevice := DeviceFrom(r.Context()); isDevice && device.Scope != "" {
		if _, ok := device.strip(item.Path); !ok {
			writeError(w, http.StatusNotFound, "not_found", "no such trash item")
			return
		}
	}

	rel, err := index.CleanPath(item.Path)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_path", "item has an unusable original path")
		return
	}

	// Refuse to clobber. Restoring should never be the thing that loses data.
	if s.store.Exists(rel) {
		writeError(w, http.StatusConflict, "occupied",
			"something already exists at the original path; move it first")
		return
	}

	abs, err := s.store.Abs(rel)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_path", "path escapes the data root")
		return
	}
	if err := os.MkdirAll(parentDir(abs), 0o755); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "cannot recreate parent directory")
		return
	}
	if err := os.Rename(s.trash.AbsPath(item.ID), abs); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "cannot restore file")
		return
	}

	info, err := os.Stat(abs)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "cannot stat restored file")
		return
	}
	sum, err := index.HashFile(abs)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "cannot hash restored file")
		return
	}

	rev, err := s.index.Upsert(r.Context(), index.Entry{
		Path: rel, Type: index.TypeFile,
		Size: info.Size(), MTime: info.ModTime().UnixMilli(), SHA256: sum,
	})
	if err != nil {
		s.writeStoreError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, fileResponse{
		Path: responsePath(r, rel), Rev: rev, Size: info.Size(), SHA256: sum,
		MTime: info.ModTime().UnixMilli(), Type: index.TypeFile,
	})
}

func parentDir(abs string) string {
	for i := len(abs) - 1; i >= 0; i-- {
		if abs[i] == os.PathSeparator {
			return abs[:i]
		}
	}
	return "."
}

// ── Shared ignore rules ──────────────────────────────────────────────────────

// The ignore list lives on the server so that every machine filters the same
// way. A rule added on one Mac takes effect everywhere without touching the
// others.
const (
	ignoreKey        = "ignore_rules"
	ignoreVersionKey = "ignore_version"
)

// defaultIgnore is seeded on first start. These are the files macOS scatters
// through every directory, which nobody wants replicated.
const defaultIgnore = `# One pattern per line. Blank lines and # comments are ignored.
# Syntax: gitignore-style globs matched against the path relative to the root.

.DS_Store
._*
Icon?
.Spotlight-V100
.Trashes
.fseventsd
.TemporaryItems
.DocumentRevisions-V100
*.swp
~$*
`

type ignoreResponse struct {
	Rules   string `json:"rules"`
	Version int64  `json:"version"`
}

func (s *Server) handleGetIgnore(w http.ResponseWriter, r *http.Request) {
	rules, version, err := s.readIgnore(r)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "cannot read ignore rules")
		return
	}
	w.Header().Set("ETag", `"`+strconv.FormatInt(version, 10)+`"`)
	writeJSON(w, http.StatusOK, ignoreResponse{Rules: rules, Version: version})
}

type ignoreRequest struct {
	Rules string `json:"rules"`
}

func (s *Server) handlePutIgnore(w http.ResponseWriter, r *http.Request) {
	var req ignoreRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<10)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", "expected {\"rules\": \"...\"}")
		return
	}

	version := time.Now().UnixMilli()
	_, err := s.index.DB().ExecContext(r.Context(),
		`INSERT INTO meta(key, value) VALUES (?, ?), (?, ?)
         ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		ignoreKey, req.Rules, ignoreVersionKey, strconv.FormatInt(version, 10))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "cannot store ignore rules")
		return
	}

	writeJSON(w, http.StatusOK, ignoreResponse{Rules: req.Rules, Version: version})
}

func (s *Server) readIgnore(r *http.Request) (string, int64, error) {
	var rules string
	err := s.index.DB().QueryRowContext(r.Context(),
		`SELECT value FROM meta WHERE key = ?`, ignoreKey).Scan(&rules)
	if errors.Is(err, sql.ErrNoRows) {
		return defaultIgnore, 0, nil
	}
	if err != nil {
		return "", 0, err
	}

	var version int64
	var raw string
	err = s.index.DB().QueryRowContext(r.Context(),
		`SELECT value FROM meta WHERE key = ?`, ignoreVersionKey).Scan(&raw)
	if err == nil {
		version, _ = strconv.ParseInt(raw, 10, 64)
	}

	return rules, version, nil
}
