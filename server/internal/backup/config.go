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
//
// What it needs is split in two, because the two halves have different owners.
// Paths say where things are: they are decided by the container's volumes and
// are fixed for the life of the process. Config says what the job does: it is
// policy, it is saved in the index, and the admin page is where it is set.
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

// configKey is where the policy document lives in the index's meta table.
const configKey = "backup_config"

// Paths are the locations the job works with. They come from the environment
// and are never editable at runtime, because they are not really settings:
// they are where the volumes are mounted, and the server cannot move a mount.
// A path that could be changed from the admin page could be pointed at
// somewhere nothing is mounted, which is a backup of nothing that looks like a
// backup of something.
//
// The defaults are the whole convention: mount what should be copied at
// /backup-source and the disk it goes to at /backup, and there is nothing to
// configure.
type Paths struct {
	// Source is what gets copied. /backup-source rather than /data because
	// what is worth backing up is rarely only what this server syncs — the
	// synced volume is usually a subtree of it.
	Source string `json:"source_dir"`

	// Dest is where the dated snapshots are written. Another disk, which is
	// the entire point: it has to survive the one the data is on.
	Dest string `json:"backup_dir"`

	// Marker is a file that must exist in Dest for a run to start, and it
	// belongs here rather than in the policy because it describes the disk. It
	// is the only reliable way to tell that the disk is mounted at all: Dest
	// is a bind mount, so it is always a mountpoint inside the container even
	// when the host disk is absent, and the bind then resolves to the empty
	// directory underneath it — on the root partition, which a run would
	// proceed to fill.
	Marker string `json:"marker"`

	// SourceOnHost and DestOnHost are the same two directories as the machine
	// running the container knows them. Display only, and never used to read or
	// write anything: the server has no access to them under those names.
	//
	// They have to be told, because a container genuinely cannot know. What it
	// sees is /backup-source; that a bind mount put /home/app-data there is a
	// fact which exists only outside it. Showing the inside name to someone
	// looking for the folder on their own disk is showing them a path that is
	// not on it. Empty falls back to the container path, which is the honest
	// answer when nobody has said otherwise.
	SourceOnHost string `json:"source_on_host,omitempty"`
	DestOnHost   string `json:"backup_dir_on_host,omitempty"`
}

// DefaultPaths is the convention a container that mounts nothing else follows.
func DefaultPaths() Paths {
	return Paths{
		Source: "/backup-source",
		Dest:   "/backup",
		Marker: ".homesync_backup_disk",
	}
}

// Config is the policy: whether the job runs, when, and how much it keeps.
// This is what the admin page writes and what is stored in the index.
type Config struct {
	// Enabled false leaves the schedule stopped. Backups start off, because a
	// sync server that began copying a directory nobody nominated would be a
	// surprise.
	Enabled bool `json:"enabled"`

	// Schedule is a five-field cron expression, read in the server's timezone.
	Schedule string `json:"schedule"`

	// Retention, per tier. See Classify.
	Daily   int `json:"daily_retention"`
	Weekly  int `json:"weekly_retention"`
	Monthly int `json:"monthly_retention"`
}

// DefaultConfig is what an unconfigured server reports.
func DefaultConfig() Config {
	return Config{
		Enabled:  false,
		Schedule: "0 3 * * *",
		Daily:    7,
		Weekly:   4,
		Monthly:  6,
	}
}

// Validate reports what would stop these paths from working. Checked once at
// startup rather than on every save, and the destination hardest: rsync is
// being handed a --delete and a path.
func (p Paths) Validate() error {
	source, err := cleanDir(p.Source, "source directory")
	if err != nil {
		return err
	}
	dest, err := cleanDir(p.Dest, "backup directory")
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

	if strings.TrimSpace(p.Marker) == "" {
		return errors.New("marker: required — it is what proves the backup disk is mounted")
	}
	if strings.ContainsRune(p.Marker, '/') {
		return errors.New("marker: a file name, not a path")
	}
	return nil
}

// normalised returns the paths cleaned, which is what the rest of the package
// works with. Only validated paths reach it.
func (p Paths) normalised() Paths {
	return Paths{
		Source: filepath.Clean(p.Source),
		Dest:   filepath.Clean(p.Dest),
		Marker: strings.TrimSpace(p.Marker),
	}
}

// Validate reports what would stop this policy from running.
func (c Config) Validate() error {
	if _, err := ParseSchedule(c.Schedule); err != nil {
		return fmt.Errorf("schedule: %w", err)
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

func (c Config) normalised() Config {
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

// within reports whether path sits inside dir. Both are already cleaned. The
// separator matters: /backup-source is not inside /backup.
func within(path, dir string) bool {
	return strings.HasPrefix(path, dir+string(filepath.Separator))
}

// LoadConfig reads the stored policy, falling back to fallback when nothing
// has been saved yet. A document that no longer parses — a downgrade, a hand
// edit — is reported rather than silently replaced by the defaults, which
// would turn a typo into a differently-configured backup.
//
// Documents written before the paths moved out of this struct still carry
// them. They are ignored, which is the intended outcome: where things are is
// the container's business now, not a saved preference.
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

// SaveConfig writes the policy document. Callers validate first.
func SaveConfig(ctx context.Context, ix *index.Index, cfg Config) error {
	raw, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	return ix.SetMeta(ctx, configKey, string(raw))
}
