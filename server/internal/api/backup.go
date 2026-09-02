package api

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/schmidt-gabriel/homesync/server/internal/backup"
)

// The backup job is configured entirely from here: what to copy, where to, how
// often and how much history to keep. It was a second container with its own
// cron and its own environment, which meant the answer to "is it still running
// nightly?" lived somewhere the admin page could not see.

func (s *Server) handleAdminBackupStatus(w http.ResponseWriter, r *http.Request) {
	status, err := s.backups.Status(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "cannot read backup status")
		return
	}
	writeJSON(w, http.StatusOK, status)
}

// readOnlyFields are the ones a request cannot set: they describe where the
// container's volumes are mounted, and no amount of saving moves a mount.
var readOnlyFields = []string{"source_dir", "backup_dir", "marker"}

func (s *Server) handleAdminBackupConfig(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 8<<10))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", "expected a backup configuration object")
		return
	}

	// Refused rather than ignored. Dropping an unknown field silently would
	// answer 200 to a request that changed nothing the caller asked for, and
	// the caller would have no way to tell.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", "expected a backup configuration object")
		return
	}
	for _, field := range readOnlyFields {
		if _, present := raw[field]; present {
			writeError(w, http.StatusBadRequest, "read_only",
				field+" is where the container's volume is mounted, not a setting; "+
					"change it by changing the mount")
			return
		}
	}

	// Decoded onto the current configuration, so the page can send just the
	// field it changed — the enable switch does not have to know every other
	// value to avoid resetting it.
	cfg := s.backups.Config()
	if err := json.Unmarshal(body, &cfg); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", "expected a backup configuration object")
		return
	}

	if err := s.backups.Save(r.Context(), cfg); err != nil {
		// Validation messages are written for the person typing into the form,
		// so they are passed through rather than flattened into "invalid".
		writeError(w, http.StatusBadRequest, "invalid_config", err.Error())
		return
	}

	slog.Info("backup configuration saved", "enabled", cfg.Enabled, "schedule", cfg.Schedule)

	status, err := s.backups.Status(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "saved, but cannot read the new status")
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (s *Server) handleAdminBackupRun(w http.ResponseWriter, r *http.Request) {
	// Checked before starting so the page can say what is wrong, rather than
	// the operator watching for a run that fails a second later. A run is
	// still allowed while the schedule is off: switching it on for the first
	// time usually means wanting to watch one work.
	cfg := s.backups.Config()
	status, err := s.backups.Status(r.Context())
	if err == nil && status.Problem != "" {
		writeError(w, http.StatusConflict, "not_ready", status.Problem)
		return
	}
	if err := cfg.Validate(); err != nil {
		writeError(w, http.StatusConflict, "invalid_config", err.Error())
		return
	}

	if err := s.backups.Trigger(); err != nil {
		if errors.Is(err, backup.ErrRunning) {
			writeError(w, http.StatusConflict, "already_running", "a backup is already running")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal", "cannot start a backup")
		return
	}

	// Accepted, not OK: rsync is still going when this returns.
	writeJSON(w, http.StatusAccepted, map[string]any{"status": "started"})
}
