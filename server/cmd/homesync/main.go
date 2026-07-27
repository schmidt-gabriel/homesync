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
	"github.com/schmidt-gabriel/homesync/server/internal/index"
	"github.com/schmidt-gabriel/homesync/server/internal/store"
	"github.com/schmidt-gabriel/homesync/server/internal/trash"
)

const usage = `homesync — file sync server

Usage:
  homesync serve                 Run the server (default)
  homesync device add <name>     Register a device, printing its token once
  homesync device list           List registered devices
  homesync device remove <name>  Revoke a device

Environment:
  DATA_DIR          Directory to sync            (default /data)
  CONFIG_DIR        Where the index lives        (default /config)
  LISTEN_ADDR       Address to bind              (default :8080)
  TRASH_DAYS        Days to keep deleted content (default 30)
  RESCAN_INTERVAL   Full reconciliation period   (default 15m)
  TOMBSTONE_DAYS    Days to keep delete markers  (default 90)
  TLS_CERT/TLS_KEY  Enable HTTPS when both set
  ADMIN_PASSWORD    Enables the web admin UI at /
  LOG_LEVEL         debug|info|warn|error        (default info)
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
	adminPassword  string
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
		adminPassword:  os.Getenv("ADMIN_PASSWORD"),
	}
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
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
	ix, err := openIndex(cfg)
	if err != nil {
		return err
	}
	defer ix.Close()

	st, err := store.New(cfg.dataDir)
	if err != nil {
		return err
	}
	tr, err := trash.New(st.Root())
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Reconcile before serving: whatever happened while we were down is
	// reflected in the index before any client can ask about it.
	slog.Info("scanning data directory", "dir", st.Root())
	stats, err := ix.Scan(ctx, st.Root(), index.DefaultSkip)
	if err != nil {
		return fmt.Errorf("initial scan: %w", err)
	}
	slog.Info("initial scan complete", "stats", stats.String())

	if err := warnIfNoDevices(ctx, ix); err != nil {
		return err
	}

	watcher, err := index.NewWatcher(ix, st.Root(), index.DefaultSkip)
	if err != nil {
		return err
	}
	go func() {
		if err := watcher.Run(ctx); err != nil {
			slog.Error("watcher stopped", "err", err)
		}
	}()

	go runJanitor(ctx, ix, st, tr, cfg)

	handler := api.New(ix, st, tr)
	if cfg.adminPassword != "" {
		handler.EnableAdmin(cfg.adminPassword)
		slog.Info("admin UI enabled at /")
	} else {
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
			slog.Warn("serving plain HTTP — bearer tokens travel in the clear; " +
				"set TLS_CERT/TLS_KEY or keep this on a trusted network")
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
func runJanitor(ctx context.Context, ix *index.Index, st *store.Store, tr *trash.Trash, cfg config) {
	ticker := time.NewTicker(cfg.rescanInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		stats, err := ix.Scan(ctx, st.Root(), index.DefaultSkip)
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
		token, err := api.AddDevice(ctx, ix.DB(), name)
		if err != nil {
			return err
		}
		// Printed once and never stored — only its hash goes to the database.
		fmt.Printf("Device %q registered.\n\nToken (shown once, store it now):\n\n  %s\n\n", name, token)
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
			fmt.Printf("%s  %s\n", d.ID, d.Name)
		}
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
