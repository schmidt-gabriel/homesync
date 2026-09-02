// Command homesync runs the sync server: an HTTP API over a directory, with a
// global revision counter that lets any number of clients stay in step.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/schmidt-gabriel/homesync/server/internal/api"
	"github.com/schmidt-gabriel/homesync/server/internal/backup"
	"github.com/schmidt-gabriel/homesync/server/internal/crypt"
	"github.com/schmidt-gabriel/homesync/server/internal/ignore"
	"github.com/schmidt-gabriel/homesync/server/internal/index"
	"github.com/schmidt-gabriel/homesync/server/internal/store"
	"github.com/schmidt-gabriel/homesync/server/internal/trash"
)

const usage = `homesync — file sync server

Usage:
  homesync serve                 Run the server (default)
  homesync device add <name> [scope]   Register a device, printing its token once
  homesync device list                 List registered devices
  homesync device scope <name> <path>  Repoint a device at another subtree
  homesync device remove <name>        Revoke a device
  homesync key generate                Print a new encryption key
  homesync key encrypt                 Encrypt every file already on the volume
  homesync key decrypt                 Decrypt every file back to plaintext

Each device syncs one subtree of DATA_DIR, its scope, which defaults to a
folder named after it. Point two devices at the same scope and they share
those files.

Environment:
  DATA_DIR          Directory to sync            (default /data)
  CONFIG_DIR        Where the index lives        (default /config)
  LISTEN_ADDR       Address to bind              (default :8080)
  TRASH_DAYS        Days to keep deleted content (default 30)
  RESCAN_INTERVAL   Full reconciliation period   (default 15m)
  TOMBSTONE_DAYS    Days to keep delete markers  (default 90)
  TLS_CERT/TLS_KEY  Enable HTTPS when both set
  ENCRYPTION_KEY    Encrypt file contents at rest (homesync key generate)
  ADMIN_USER        Admin username               (default admin)
  ADMIN_PASSWORD    Enables the web admin UI at /
  ADMIN_NO_AUTH     Serve the admin UI with no login at all
  LOG_LEVEL         debug|info|warn|error        (default info)

Backups, where things are. Read on every start, and shown on the admin page
without being editable there: these describe the container's volumes, and no
amount of saving moves a mount.

  BACKUP_SOURCE     Directory to snapshot        (default /backup-source)
  BACKUP_DIR        Where snapshots go           (default /backup)
  BACKUP_MARKER     File proving the disk is mounted (default .homesync_backup_disk)

The two directories above are what the container sees. What the host calls them
is something only the host knows, so the admin page shows these when they are
set and falls back to the container path when they are not. Display only:
nothing is read or written under these names.

  BACKUP_SOURCE_HOST  The source as the host knows it, e.g. /home/app-data
  BACKUP_DIR_HOST     The destination as the host knows it, e.g. /mnt/Storage/Backup

Backups, what the job does. These seed the configuration the first time the
server starts; from the first save on the admin page onwards it owns them, and
changing these has no effect.

  BACKUP_ENABLED    Take dated snapshots         (default false)
  BACKUP_SCHEDULE   Five-field cron expression   (default "0 3 * * *")
  BACKUP_DAILY      Daily snapshots to keep      (default 7)
  BACKUP_WEEKLY     ISO weeks to keep one of     (default 4)
  BACKUP_MONTHLY    Months to keep one of        (default 6)

Mount what should be copied at /backup-source and the disk it goes to at
/backup and there is nothing to set. Each run writes BACKUP_DIR/YYYY-MM-DD with
rsync --link-dest, so unchanged files are hard links to the previous snapshot
and cost no space. The marker file is what stops a run when the backup disk is
not mounted: a bind mount is always a mountpoint inside the container, so the
mount itself proves nothing.

Encryption applies to new writes. Turning it on leaves the files already on
the volume as they are, readable either way; "homesync key encrypt" converts
them. The server holds the key, so this protects a stolen disk or a copied
backup, not a compromised server.
`

type config struct {
	dataDir        string
	configDir      string
	listenAddr     string
	trashDays      int
	tombstoneDays  int
	rescanInterval time.Duration
	tlsCert        string
	tlsKey         string
	adminUser      string
	adminPassword  string
	adminNoAuth    bool
	encryptionKey  string
	backupPaths    backup.Paths
	backup         backup.Config
}

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run() error {
	setupLogging()
	cfg := loadConfig()

	args := os.Args[1:]
	command := "serve"
	if len(args) > 0 {
		command = args[0]
		args = args[1:]
	}

	switch command {
	case "serve":
		return serve(cfg)
	case "device":
		return deviceCommand(cfg, args)
	case "key":
		return keyCommand(cfg, args)
	case "help", "-h", "--help":
		fmt.Print(usage)
		return nil
	default:
		fmt.Print(usage)
		return fmt.Errorf("unknown command %q", command)
	}
}

func setupLogging() {
	level := slog.LevelInfo
	switch strings.ToLower(os.Getenv("LOG_LEVEL")) {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level})))
}

func loadConfig() config {
	return config{
		dataDir:        env("DATA_DIR", "/data"),
		configDir:      env("CONFIG_DIR", "/config"),
		listenAddr:     env("LISTEN_ADDR", ":8080"),
		trashDays:      envInt("TRASH_DAYS", 30),
		tombstoneDays:  envInt("TOMBSTONE_DAYS", 90),
		rescanInterval: envDuration("RESCAN_INTERVAL", 15*time.Minute),
		tlsCert:        os.Getenv("TLS_CERT"),
		tlsKey:         os.Getenv("TLS_KEY"),
		adminUser:      env("ADMIN_USER", "admin"),
		adminPassword:  os.Getenv("ADMIN_PASSWORD"),
		adminNoAuth:    envBool("ADMIN_NO_AUTH", false),
		encryptionKey:  os.Getenv("ENCRYPTION_KEY"),
		backupPaths:    backupPaths(),
		backup:         backupConfig(),
	}
}

// backupPaths is where the volumes are, and the environment is the only place
// that can say. Unlike the policy below, this is read on every start rather
// than seeding one: moving a mount has to move the path with it, and a value
// saved months ago would go on naming the old one.
func backupPaths() backup.Paths {
	defaults := backup.DefaultPaths()
	return backup.Paths{
		Source: env("BACKUP_SOURCE", defaults.Source),
		Dest:   env("BACKUP_DIR", defaults.Dest),
		Marker: env("BACKUP_MARKER", defaults.Marker),
		// Display only. The compose file knows the host paths because it wrote
		// the mounts; nothing inside the container can work them out.
		SourceOnHost: os.Getenv("BACKUP_SOURCE_HOST"),
		DestOnHost:   os.Getenv("BACKUP_DIR_HOST"),
	}
}

// backupConfig is only ever the seed for a fresh database. Once the admin UI
// has saved a configuration, that document wins — an environment variable that
// silently overrode what the page shows would make the page a lie.
func backupConfig() backup.Config {
	defaults := backup.DefaultConfig()
	return backup.Config{
		Enabled:  envBool("BACKUP_ENABLED", defaults.Enabled),
		Schedule: env("BACKUP_SCHEDULE", defaults.Schedule),
		Daily:    envInt("BACKUP_DAILY", defaults.Daily),
		Weekly:   envInt("BACKUP_WEEKLY", defaults.Weekly),
		Monthly:  envInt("BACKUP_MONTHLY", defaults.Monthly),
	}
}

// encryptionKey turns the configured value into a key, or nil when there is
// none. A key that will not parse stops the server rather than quietly
// starting without encryption — that would write plaintext onto a volume the
// operator believes is encrypted.
func (c config) key() (*crypt.Key, error) {
	if c.encryptionKey == "" {
		return nil, nil
	}
	key, err := crypt.ParseKey(c.encryptionKey)
	if err != nil {
		return nil, fmt.Errorf("ENCRYPTION_KEY: %w", err)
	}
	return &key, nil
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envBool(key string, fallback bool) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	switch v {
	case "":
		return fallback
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		slog.Warn("ignoring unparseable value", "key", key, "value", v)
		return fallback
	}
}

func envInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil {
			return parsed
		}
		slog.Warn("ignoring unparseable value", "key", key, "value", v)
	}
	return fallback
}

func envDuration(key string, fallback time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if parsed, err := time.ParseDuration(v); err == nil {
			return parsed
		}
		slog.Warn("ignoring unparseable value", "key", key, "value", v)
	}
	return fallback
}

// openIndex prepares the config directory and the database.
func openIndex(cfg config) (*index.Index, error) {
	if err := os.MkdirAll(cfg.configDir, 0o755); err != nil {
		return nil, fmt.Errorf("create config dir: %w", err)
	}
	return index.Open(filepath.Join(cfg.configDir, "homesync.db"))
}

func serve(cfg config) error {
	key, err := cfg.key()
	if err != nil {
		return err
	}

	ix, err := openIndex(cfg)
	if err != nil {
		return err
	}
	defer ix.Close()

	st, err := store.New(cfg.dataDir, key)
	if err != nil {
		return err
	}
	if key != nil {
		slog.Info("encrypting file contents at rest",
			"note", "existing plaintext files stay readable; run `homesync key encrypt` to convert them")
	}
	tr, err := trash.New(st.Root())
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// The shared ignore rules decide what the index holds, not just what the
	// clients upload. One instance, read by the scanner, the watcher and the
	// API alike, so a rule cannot be in force in one of them and not another.
	rules := ignore.NewShared(ix.DB())
	if err := rules.Refresh(ctx); err != nil {
		return fmt.Errorf("read ignore rules: %w", err)
	}

	// Reconcile before serving: whatever happened while we were down is
	// reflected in the index before any client can ask about it.
	slog.Info("scanning data directory", "dir", st.Root())
	stats, err := ix.Scan(ctx, st.Root(), rules.Skip, key)
	if err != nil {
		return fmt.Errorf("initial scan: %w", err)
	}
	slog.Info("initial scan complete", "stats", stats.String())

	if err := warnIfNoDevices(ctx, ix); err != nil {
		return err
	}

	watcher, err := index.NewWatcher(ix, st.Root(), rules.Skip, key)
	if err != nil {
		return err
	}
	go func() {
		if err := watcher.Run(ctx); err != nil {
			slog.Error("watcher stopped", "err", err)
		}
	}()

	go runJanitor(ctx, ix, st, tr, rules, cfg)

	// Validated here rather than at the first run, so a mount that is not
	// where the server thinks it is says so on startup instead of at 03:00.
	// Not fatal: syncing has nothing to do with backups, and refusing to serve
	// files because a backup path is wrong would be a poor trade.
	if err := cfg.backupPaths.Validate(); err != nil {
		slog.Error("backup paths are unusable; the job will refuse to run",
			"source", cfg.backupPaths.Source, "dest", cfg.backupPaths.Dest, "err", err)
	}

	backups, err := backup.New(ctx, ix, cfg.backupPaths, cfg.backup)
	if err != nil {
		return err
	}
	// Started whatever the configuration says: a disabled job idles on its
	// reload channel, so switching it on from the admin UI takes effect
	// without a restart.
	go backups.Run(ctx)
	if active := backups.Config(); active.Enabled {
		slog.Info("backups enabled", "schedule", active.Schedule,
			"source", cfg.backupPaths.Source, "dest", cfg.backupPaths.Dest)
	}

	handler := api.New(ix, st, tr, rules, backups)
	switch {
	case cfg.adminNoAuth:
		handler.EnableAdmin(cfg.adminUser, "", true)
		// Loud, and deliberately not once-in-passing: everything the UI can do
		// is now available to anyone who can reach the port.
		slog.Warn("admin UI enabled at / with NO AUTHENTICATION",
			"exposed", "device tokens, the file tree, and the trash",
			"fix", "unset ADMIN_NO_AUTH and set ADMIN_PASSWORD")
	case cfg.adminPassword != "":
		handler.EnableAdmin(cfg.adminUser, cfg.adminPassword, false)
		slog.Info("admin UI enabled at /", "user", cfg.adminUser)
	default:
		slog.Info("admin UI disabled (set ADMIN_PASSWORD to enable)")
	}

	srv := &http.Server{
		Addr:    cfg.listenAddr,
		Handler: handler,
		// No WriteTimeout: SSE connections are meant to stay open, and a write
		// deadline would sever them on a schedule.
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		useTLS := cfg.tlsCert != "" && cfg.tlsKey != ""
		slog.Info("listening", "addr", cfg.listenAddr, "tls", useTLS)

		var err error
		if useTLS {
			err = srv.ListenAndServeTLS(cfg.tlsCert, cfg.tlsKey)
		} else {
			// A home network is what this is for, so plain HTTP is the normal
			// case and gets stated once at info level. Logging it as a warning
			// on every start trains people to ignore warnings.
			slog.Info("serving plain HTTP, suitable for a trusted local network; " +
				"set TLS_CERT and TLS_KEY if this is reachable from outside it")
			err = srv.ListenAndServe()
		}
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		slog.Info("shutting down")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

// warnIfNoDevices makes a fresh install obvious instead of silently rejecting
// every request with 401.
func warnIfNoDevices(ctx context.Context, ix *index.Index) error {
	devices, err := api.ListDevices(ctx, ix.DB())
	if err != nil {
		return err
	}
	if len(devices) == 0 {
		slog.Warn("no devices registered — every request will return 401",
			"fix", "run: homesync device add <name>")
	}
	return nil
}

// runJanitor does the periodic housekeeping: full rescan, trash purge and
// tombstone pruning.
func runJanitor(ctx context.Context, ix *index.Index, st *store.Store, tr *trash.Trash,
	rules *ignore.Shared, cfg config) {
	ticker := time.NewTicker(cfg.rescanInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		// Re-read first: a device registered from the CLI since the last pass
		// has a scope, and a scope changes how a rule reads.
		if err := rules.Refresh(ctx); err != nil {
			slog.Warn("cannot re-read ignore rules", "err", err)
		}

		stats, err := ix.Scan(ctx, st.Root(), rules.Skip, st.Key())
		if err != nil {
			slog.Warn("rescan failed", "err", err)
		} else if stats.Added+stats.Updated+stats.Deleted > 0 {
			// Only worth a log line when the watcher missed something.
			slog.Info("rescan reconciled changes", "stats", stats.String())
		}

		if removed, err := tr.Purge(time.Now().AddDate(0, 0, -cfg.trashDays)); err != nil {
			slog.Warn("trash purge failed", "err", err)
		} else if removed > 0 {
			slog.Info("purged trash", "items", removed, "older_than_days", cfg.trashDays)
		}

		cutoff := time.Now().AddDate(0, 0, -cfg.tombstoneDays).UnixMilli()
		if pruned, err := ix.PruneTombstones(ctx, cutoff); err != nil {
			slog.Warn("tombstone prune failed", "err", err)
		} else if pruned > 0 {
			slog.Info("pruned tombstones", "rows", pruned, "older_than_days", cfg.tombstoneDays)
		}
	}
}

func deviceCommand(cfg config, args []string) error {
	if len(args) == 0 {
		fmt.Print(usage)
		return errors.New("device: missing subcommand")
	}

	ix, err := openIndex(cfg)
	if err != nil {
		return err
	}
	defer ix.Close()

	ctx := context.Background()

	switch args[0] {
	case "add":
		if len(args) < 2 {
			return errors.New("device add: missing name")
		}
		name := args[1]

		scope := api.DefaultScope(name)
		if len(args) > 2 {
			validated, err := api.ValidateScope(args[2])
			if err != nil {
				return err
			}
			scope = validated
		}

		token, err := api.AddDevice(ctx, ix.DB(), name, scope)
		if err != nil {
			return err
		}

		// Created now so the folder is there to see, rather than appearing
		// only once the device uploads something.
		if scope != "" {
			if err := os.MkdirAll(filepath.Join(cfg.dataDir, scope), 0o755); err != nil {
				return fmt.Errorf("create scope directory: %w", err)
			}
		}

		where := scope
		if where == "" {
			where = "(the whole data directory)"
		}
		// Printed once and never stored — only its hash goes to the database.
		fmt.Printf("Device %q registered, syncing %s.\n\nToken (shown once, store it now):\n\n  %s\n\n",
			name, where, token)
		return nil

	case "list":
		devices, err := api.ListDevices(ctx, ix.DB())
		if err != nil {
			return err
		}
		if len(devices) == 0 {
			fmt.Println("No devices registered.")
			return nil
		}
		for _, d := range devices {
			scope := d.Scope
			if scope == "" {
				scope = "(whole tree)"
			}
			fmt.Printf("%s  %-24s %s\n", d.ID, d.Name, scope)
		}
		return nil

	case "scope":
		if len(args) < 3 {
			return errors.New("device scope: expected <name> <path>")
		}
		scope, err := api.ValidateScope(args[2])
		if err != nil {
			return err
		}
		updated, err := api.SetScope(ctx, ix.DB(), args[1], scope)
		if err != nil {
			return err
		}
		if updated == 0 {
			return fmt.Errorf("no device named %q", args[1])
		}
		if scope != "" {
			if err := os.MkdirAll(filepath.Join(cfg.dataDir, scope), 0o755); err != nil {
				return fmt.Errorf("create scope directory: %w", err)
			}
		}
		fmt.Printf("Device %q now syncs %q.\n", args[1], scope)
		fmt.Println("It will resync from scratch the next time it connects.")
		return nil

	case "remove":
		if len(args) < 2 {
			return errors.New("device remove: missing name")
		}
		removed, err := api.RemoveDevice(ctx, ix.DB(), args[1])
		if err != nil {
			return err
		}
		if removed == 0 {
			return fmt.Errorf("no device named %q", args[1])
		}
		fmt.Printf("Revoked %d device(s) named %q.\n", removed, args[1])
		return nil

	default:
		return fmt.Errorf("device: unknown subcommand %q", args[0])
	}
}
