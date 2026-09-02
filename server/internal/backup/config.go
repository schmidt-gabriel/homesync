// Package backup takes dated snapshots of a directory onto another disk.
//
// Each run creates BackupDir/YYYY-MM-DD with rsync --link-dest pointed at the
// previous snapshot, so unchanged files become hard links and cost no space:
// every folder reads as a full backup, and only what changed is stored twice.
// Old snapshots are then thinned by a grandfather-father-son rule.
//
// It was a separate project (datakeeper) with its own container, cron and
// shell script. Nothing about the schedule or the retention needed a second
// process, and keeping the history where the admin UI could read it needed a
// shared volume, so it lives here instead — one binary, and the state is in
// the index next to everything else the UI shows.
package backup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/schmidt-gabriel/homesync/server/internal/index"
)

// configKey is where the document lives in the index's meta table. It is the
// source of truth once written: the environment only seeds the first start,
// the same way the shared ignore rules work.
const configKey = "backup_config"

// Config is everything the job needs, and everything the admin UI can change.
type Config struct {
	// Enabled false leaves the schedule stopped. Nothing else is validated
	// away, so a half-finished configuration can be saved and turned on later.
	Enabled bool `json:"enabled"`

	// SourceDir and BackupDir are paths inside the container, so changing
	// either from the UI only makes sense for something already mounted.
	SourceDir string `json:"source_dir"`
	BackupDir string `json:"backup_dir"`

	// Schedule is a five-field cron expression, read in the server's timezone.
	Schedule string `json:"schedule"`

	// Marker is a file that must exist in BackupDir for a run to start. See
	// checkMarker: it is the only reliable way to tell that the backup disk is
	// actually mounted.
	Marker string `json:"marker"`

	// Retention, per tier. See Classify.
	Daily   int `json:"daily_retention"`
	Weekly  int `json:"weekly_retention"`
	Monthly int `json:"monthly_retention"`
}

// DefaultConfig is what an unconfigured server reports. Disabled: a sync
// server that started making copies of a directory nobody nominated would be
// a surprise, and the destination has to be mounted to be worth anything.
func DefaultConfig() Config {
	return Config{
		Enabled:   false,
		SourceDir: "/data",
		BackupDir: "/backup",
		Schedule:  "0 3 * * *",
		Marker:    ".homesync_backup_disk",
		Daily:     7,
		Weekly:    4,
		Monthly:   6,
	}
}

// Validate reports what would stop this configuration from running, with the
// destination checked hardest: rsync is being handed a --delete and a path,
// and the paths are the part a person types.
func (c Config) Validate() error {
	if _, err := ParseSchedule(c.Schedule); err != nil {
		return fmt.Errorf("schedule: %w", err)
	}

	source, err := cleanDir(c.SourceDir, "source directory")
	if err != nil {
		return err
	}
	dest, err := cleanDir(c.BackupDir, "backup directory")
	if err != nil {
		return err
	}

	if source == dest {
		return errors.New("the source and the backup directory are the same path")
	}
	// Either nesting ruins the backup, in opposite ways. A destination under
	// the source makes every run copy the previous runs, forever. A source
	// under the destination means retention deletes the originals.
	if within(dest, source) {
		return errors.New("the backup directory is inside the source; every run would copy the previous ones")
	}
	if within(source, dest) {
		return errors.New("the source is inside the backup directory; retention would delete the originals")
	}

	if strings.TrimSpace(c.Marker) == "" {
		return errors.New("marker: required — it is what proves the backup disk is mounted")
	}
	if strings.ContainsRune(c.Marker, '/') {
		return errors.New("marker: a file name, not a path")
	}

	// Without at least one daily, a run would prune the snapshot it just took
	// on any day that is neither the first of its week nor of its month.
	if c.Daily < 1 {
		return errors.New("daily retention: keep at least 1")
	}
	if c.Weekly < 0 || c.Monthly < 0 {
		return errors.New("retention counts cannot be negative")
	}
	return nil
}

// normalised returns the config with its paths cleaned, which is what the rest
// of the package works with. Only valid configs reach it.
func (c Config) normalised() Config {
	c.SourceDir = filepath.Clean(c.SourceDir)
	c.BackupDir = filepath.Clean(c.BackupDir)
	c.Marker = strings.TrimSpace(c.Marker)
	c.Schedule = strings.TrimSpace(c.Schedule)
	return c
}

func cleanDir(path, what string) (string, error) {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return "", fmt.Errorf("%s: required", what)
	}
	if !filepath.IsAbs(trimmed) {
		return "", fmt.Errorf("%s: must be an absolute path", what)
	}
	clean := filepath.Clean(trimmed)
	if clean == "/" {
		return "", fmt.Errorf("%s: cannot be the filesystem root", what)
	}
	return clean, nil
}

// within reports whether path sits inside dir. Both are already cleaned.
func within(path, dir string) bool {
	return strings.HasPrefix(path, dir+string(filepath.Separator))
}

// LoadConfig reads the stored document, falling back to fallback when nothing
// has been saved yet. A document that no longer parses — a downgrade, a hand
// edit — is reported rather than silently replaced by the defaults, which
// would turn a typo into a differently-configured backup.
func LoadConfig(ctx context.Context, ix *index.Index, fallback Config) (Config, error) {
	raw, err := ix.GetMeta(ctx, configKey, "")
	if err != nil {
		return Config{}, err
	}
	if raw == "" {
		return fallback, nil
	}

	// Decoding onto the fallback means a document written by an older version
	// keeps that version's defaults for fields it does not mention, rather
	// than getting Go's zero values.
	cfg := fallback
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return Config{}, fmt.Errorf("stored backup config is not valid JSON: %w", err)
	}
	return cfg, nil
}

// SaveConfig writes the document. Callers validate first.
func SaveConfig(ctx context.Context, ix *index.Index, cfg Config) error {
	raw, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	return ix.SetMeta(ctx, configKey, string(raw))
}
