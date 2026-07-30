package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"strconv"
	"time"

	"github.com/schmidt-gabriel/homesync/server/internal/crypt"
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
	// Whatever went into the trash came back unchanged, so on an encrypted
	// volume that is ciphertext. The index only ever describes plaintext.
	sum, err := crypt.HashFile(abs, s.store.Key())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "cannot hash restored file")
		return
	}
	size, err := index.PlainSize(abs, info, s.store.Key())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "cannot size restored file")
		return
	}

	rev, err := s.index.Upsert(r.Context(), index.Entry{
		Path: rel, Type: index.TypeFile,
		Size: size, MTime: info.ModTime().UnixMilli(), SHA256: sum,
	})
	if err != nil {
		s.writeStoreError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, fileResponse{
		Path: responsePath(r, rel), Rev: rev, Size: size, SHA256: sum,
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

type ignoreResponse struct {
	Rules   string `json:"rules"`
	Version int64  `json:"version"`
	// Purged counts what saving these rules took off the server. Present only
	// on a save, and zero when it took nothing.
	Purged int `json:"purged,omitempty"`
}

func (s *Server) handleGetIgnore(w http.ResponseWriter, r *http.Request) {
	rules, version, err := s.ignore.Load(r.Context())
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

	version, err := s.ignore.Save(r.Context(), req.Rules)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "cannot store ignore rules")
		return
	}

	// The rules are saved either way. A purge that fails halfway has still
	// removed what it removed, and the scanner drops the rest, so reporting the
	// count we reached is more use to the caller than an error that would
	// suggest the save itself did not happen.
	purged, err := s.purgeIgnored(r.Context())
	if err != nil {
		slog.Warn("ignore rules saved, but the purge did not finish",
			"purged", purged, "err", err)
	}

	writeJSON(w, http.StatusOK, ignoreResponse{Rules: req.Rules, Version: version, Purged: purged})
}

// purgeIgnored takes out everything the rules now exclude.
//
// A rule has to be retroactive to mean anything: the reason someone adds
// `.git/` is the copy that is already on the server, and leaving it there means
// the pattern they just wrote appears to do nothing. Every client is told by
// the tombstones; the content goes to the trash rather than being destroyed,
// exactly like any other deletion here.
//
// Files that were never indexed are left alone. What is on the volume but not
// in the index is invisible to every client already, and this is not the place
// to go looking for it.
func (s *Server) purgeIgnored(ctx context.Context) (int, error) {
	entries, err := s.index.All(ctx)
	if err != nil {
		return 0, err
	}

	// Deepest first, so a directory's children are already gone by the time we
	// try to remove it.
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path > entries[j].Path })

	purged := 0
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return purged, err
		}
		if !s.ignore.Skip(entry.Path, entry.Type == index.TypeDir) {
			continue
		}

		if entry.Type == index.TypeFile {
			if err := s.moveToTrash(entry.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
				slog.Warn("cannot move an ignored file to the trash", "path", entry.Path, "err", err)
				continue
			}
		} else if err := s.store.Remove(entry.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
			// Something unindexed is still inside it. The row goes anyway: the
			// scanner skips this path from here on, so keeping it would leave a
			// directory no client can see and nothing will ever clean up.
			slog.Warn("cannot remove an ignored directory", "path", entry.Path, "err", err)
		}

		if _, err := s.index.MarkDeleted(ctx, entry.Path, time.Now().UnixMilli()); err != nil {
			slog.Warn("cannot tombstone an ignored path", "path", entry.Path, "err", err)
			continue
		}
		purged++
	}

	if purged > 0 {
		slog.Info("ignore rules purged paths from the index", "paths", purged,
			"note", "file content is recoverable from the trash")
	}
	return purged, nil
}
