package api

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/schmidt-gabriel/homesync/server/internal/index"
	"github.com/schmidt-gabriel/homesync/server/internal/store"
)

// baseRevHeader carries the revision the client believes is current for the
// path it is about to change. It is what makes concurrent edits detectable
// rather than silently last-write-wins.
const baseRevHeader = "X-Base-Rev"

// contentHashHeader carries the SHA-256 the client computed for exactly the
// bytes it meant to send. Optional, but a client that sends it gets its upload
// rejected rather than stored if the two disagree.
//
// This is not about the network, which TCP and TLS already checksum. It is
// about the file changing underneath the client while it was being read: an
// editor saving over it produces bytes belonging to no version of the file,
// and without this the server would store them and hand them to every other
// machine as the truth.
const contentHashHeader = "X-Content-SHA256"

// fileResponse is returned by PUT and DELETE.
type fileResponse struct {
	Path   string `json:"path"`
	Rev    int64  `json:"rev"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256,omitempty"`
	MTime  int64  `json:"mtime"`
	Type   string `json:"type"`
}

// requestPath validates the path from the URL and resolves it inside the
// authenticated device's scope. Every handler goes through this, so a device
// cannot name a path outside its own subtree even by accident.
func requestPath(r *http.Request) (string, error) {
	device, _ := DeviceFrom(r.Context())
	return device.resolve(r.PathValue("path"))
}

// responsePath turns an index path back into what the device should see.
func responsePath(r *http.Request, path string) string {
	device, _ := DeviceFrom(r.Context())
	relative, ok := device.strip(path)
	if !ok {
		return path
	}
	return relative
}

// parseBaseRev reads X-Base-Rev. Absent means "I believe this path does not
// exist", which is revision 0 — the same value the index reports for a path it
// has never seen.
func parseBaseRev(r *http.Request) (int64, error) {
	raw := r.Header.Get(baseRevHeader)
	if raw == "" {
		return 0, nil
	}
	rev, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || rev < 0 {
		return 0, fmt.Errorf("invalid %s header", baseRevHeader)
	}
	return rev, nil
}

// liveRev returns the revision a client must present to modify path: the
// entry's revision if it exists, or 0 if it is absent or tombstoned.
func liveRev(e index.Entry, found bool) int64 {
	if !found || e.Deleted {
		return 0
	}
	return e.Rev
}

func (s *Server) handleGetFile(w http.ResponseWriter, r *http.Request) {
	rel, err := requestPath(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_path", err.Error())
		return
	}

	entry, found, err := s.index.Lookup(r.Context(), rel)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "index lookup failed")
		return
	}
	if !found || entry.Deleted {
		writeError(w, http.StatusNotFound, "not_found", "no such path")
		return
	}
	if entry.Type == index.TypeDir {
		writeError(w, http.StatusBadRequest, "is_directory", "path is a directory")
		return
	}

	f, info, err := s.store.Open(rel)
	if err != nil {
		// The index and the disk disagree; a rescan will reconcile it.
		if errors.Is(err, os.ErrNotExist) {
			writeError(w, http.StatusNotFound, "not_found", "content missing on disk")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal", "cannot read file")
		return
	}
	defer f.Close()

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("ETag", `"`+entry.SHA256+`"`)
	w.Header().Set(baseRevHeader, strconv.FormatInt(entry.Rev, 10))

	// ServeContent handles Range requests and conditional GETs, so a client
	// resuming a large download does not start over.
	http.ServeContent(w, r, path.Base(rel), info.ModTime(), f)
}

func (s *Server) handlePutFile(w http.ResponseWriter, r *http.Request) {
	rel, err := requestPath(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_path", err.Error())
		return
	}

	baseRev, err := parseBaseRev(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_base_rev", err.Error())
		return
	}

	entry, found, err := s.index.Lookup(r.Context(), rel)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "index lookup failed")
		return
	}
	if found && entry.Type == index.TypeDir && !entry.Deleted {
		writeError(w, http.StatusConflict, "is_directory", "a directory exists at this path")
		return
	}

	// Before anything reaches the disk. On a case-insensitive volume, writing
	// "NOTES.md" when "notes.md" exists overwrites the original file, so
	// discovering the collision after the write would be discovering it too
	// late — the data would already be gone.
	if err := s.index.CheckCaseCollision(r.Context(), rel); err != nil {
		s.writeStoreError(w, err)
		return
	}

	// Two machines edited from the same base. Rather than pick a winner, keep
	// both: the incoming body is stored beside the original under a generated
	// name, and the client is told what that name is.
	if got := liveRev(entry, found); got != baseRev {
		device, _ := DeviceFrom(r.Context())
		conflictPath := conflictName(rel, device.Name, time.Now())

		result, rev, err := s.storeFile(r, conflictPath, r.Body)
		if err != nil {
			s.writeStoreError(w, err)
			return
		}

		writeJSON(w, http.StatusConflict, struct {
			errorResponse
			fileResponse
		}{
			errorResponse: errorResponse{
				Error: "conflict",
				Message: fmt.Sprintf("path changed since rev %d (now %d); body stored as %q",
					baseRev, got, responsePath(r, conflictPath)),
				Conflict: responsePath(r, conflictPath),
			},
			fileResponse: fileResponse{
				Path: responsePath(r, conflictPath), Rev: rev, Size: result.Size,
				SHA256: result.SHA256, MTime: result.MTime, Type: index.TypeFile,
			},
		})
		return
	}

	// Preserve whatever is being replaced before it is gone.
	if found && !entry.Deleted && entry.Type == index.TypeFile {
		if err := s.moveToTrash(rel); err != nil {
			writeError(w, http.StatusInternalServerError, "internal", "cannot preserve previous version")
			return
		}
	}

	result, rev, err := s.storeFile(r, rel, r.Body)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}

	status := http.StatusOK
	if !found || entry.Deleted {
		status = http.StatusCreated
	}
	writeJSON(w, status, fileResponse{
		Path: responsePath(r, rel), Rev: rev, Size: result.Size,
		SHA256: result.SHA256, MTime: result.MTime, Type: index.TypeFile,
	})
}

func (s *Server) handleDeleteFile(w http.ResponseWriter, r *http.Request) {
	rel, err := requestPath(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_path", err.Error())
		return
	}

	baseRev, err := parseBaseRev(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_base_rev", err.Error())
		return
	}

	entry, found, err := s.index.Lookup(r.Context(), rel)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "index lookup failed")
		return
	}
	if !found || entry.Deleted {
		writeError(w, http.StatusNotFound, "not_found", "no such path")
		return
	}

	// Deleting something you have not seen the latest version of would discard
	// another machine's edit. Refuse and let the client resolve it.
	if got := liveRev(entry, found); got != baseRev {
		writeError(w, http.StatusConflict, "stale",
			fmt.Sprintf("path changed since rev %d (now %d); fetch it before deleting", baseRev, got))
		return
	}

	if entry.Type == index.TypeFile {
		if err := s.moveToTrash(rel); err != nil && !errors.Is(err, os.ErrNotExist) {
			writeError(w, http.StatusInternalServerError, "internal", "cannot move to trash")
			return
		}
	} else if err := s.store.Remove(rel); err != nil && !errors.Is(err, os.ErrNotExist) {
		// A non-empty directory means its children are still indexed; the
		// client should delete them first.
		writeError(w, http.StatusConflict, "not_empty", "directory is not empty")
		return
	}

	rev, err := s.index.MarkDeleted(r.Context(), rel, time.Now().UnixMilli())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "cannot record deletion")
		return
	}

	writeJSON(w, http.StatusOK, fileResponse{
		Path: responsePath(r, rel), Rev: rev, Type: entry.Type})
}

func (s *Server) handlePutDir(w http.ResponseWriter, r *http.Request) {
	rel, err := requestPath(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_path", err.Error())
		return
	}

	entry, found, err := s.index.Lookup(r.Context(), rel)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "index lookup failed")
		return
	}
	if found && !entry.Deleted {
		if entry.Type == index.TypeFile {
			writeError(w, http.StatusConflict, "is_file", "a file exists at this path")
			return
		}
		// Already a directory: creating it again is a no-op, not an error.
		writeJSON(w, http.StatusOK, fileResponse{
			Path: responsePath(r, rel), Rev: entry.Rev, Type: index.TypeDir})
		return
	}

	if err := s.index.CheckCaseCollision(r.Context(), rel); err != nil {
		s.writeStoreError(w, err)
		return
	}

	if err := s.store.Mkdir(rel); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "cannot create directory")
		return
	}

	info, err := s.store.Stat(rel)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "cannot stat directory")
		return
	}

	rev, err := s.index.Upsert(r.Context(), index.Entry{
		Path: rel, Type: index.TypeDir, MTime: info.ModTime().UnixMilli(),
	})
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, fileResponse{
		Path: responsePath(r, rel), Rev: rev, Type: index.TypeDir,
		MTime: info.ModTime().UnixMilli(),
	})
}

// ErrHashMismatch means the body did not hash to what the client said it would.
var ErrHashMismatch = errors.New("content hash mismatch")

// storeFile writes a body to disk and records it, returning the assigned rev.
func (s *Server) storeFile(r *http.Request, rel string, body io.Reader) (store.WriteResult, int64, error) {
	result, err := s.store.Write(rel, body)
	if err != nil {
		return store.WriteResult{}, 0, err
	}

	// Checked after writing rather than before, because the body is a stream
	// and hashing it first would mean holding the whole file in memory. The
	// write went to a temporary file and was renamed into place, so undoing it
	// is a delete.
	if declared := r.Header.Get(contentHashHeader); declared != "" && declared != result.SHA256 {
		if removeErr := s.store.Remove(rel); removeErr != nil {
			slog.Error("cannot remove a file that failed its hash check",
				"path", rel, "err", removeErr)
		}
		return store.WriteResult{}, 0, fmt.Errorf(
			"%w: client declared %s, body hashed to %s", ErrHashMismatch, declared, result.SHA256)
	}

	rev, err := s.index.Upsert(r.Context(), index.Entry{
		Path: rel, Type: index.TypeFile,
		Size: result.Size, MTime: result.MTime, SHA256: result.SHA256,
	})
	if err != nil {
		// The content is on disk but unindexed. Deliberately left in place:
		// the next rescan will pick it up, whereas deleting it here would
		// throw away the only copy of what the client just sent.
		slog.Error("wrote file but could not index it; rescan will reconcile",
			"path", rel, "err", err)
		return store.WriteResult{}, 0, err
	}
	return result, rev, nil
}

// moveToTrash preserves the current content of rel. A path already missing from
// disk is not an error — the index will be reconciled by the next scan.
func (s *Server) moveToTrash(rel string) error {
	abs, err := s.store.Abs(rel)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(abs); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	_, err = s.trash.Put(abs, rel, time.Now())
	return err
}

func (s *Server) writeStoreError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrHashMismatch):
		writeError(w, http.StatusUnprocessableEntity, "hash_mismatch", err.Error())
	case errors.Is(err, index.ErrCaseCollision):
		writeError(w, http.StatusConflict, "case_collision", err.Error())
	case errors.Is(err, store.ErrOutsideRoot), errors.Is(err, index.ErrInvalidPath):
		writeError(w, http.StatusBadRequest, "invalid_path", "path escapes the data root")
	case errors.Is(err, store.ErrNotRegular):
		writeError(w, http.StatusConflict, "not_regular", "target is not a regular file")
	default:
		writeError(w, http.StatusInternalServerError, "internal", "cannot store file")
	}
}

// conflictName builds the filename a losing write is parked under. The device
// and timestamp make it obvious where the copy came from, and keeping the
// original extension means the file still opens in the right application.
func conflictName(rel, device string, when time.Time) string {
	dir, base := path.Split(rel)
	ext := path.Ext(base)
	stem := strings.TrimSuffix(base, ext)

	return dir + fmt.Sprintf("%s.conflict-%s-%s%s",
		stem, sanitiseDevice(device), when.Format("20060102-150405"), ext)
}

// sanitiseDevice strips anything that would change the shape of the path or
// look wrong in a filename.
func sanitiseDevice(name string) string {
	if name == "" {
		return "unknown"
	}
	cleaned := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-':
			return r
		case r == ' ' || r == '_' || r == '.':
			return '-'
		default:
			return -1
		}
	}, name)

	cleaned = strings.Trim(cleaned, "-")
	if cleaned == "" {
		return "unknown"
	}
	if len(cleaned) > 32 {
		cleaned = cleaned[:32]
	}
	return cleaned
}
