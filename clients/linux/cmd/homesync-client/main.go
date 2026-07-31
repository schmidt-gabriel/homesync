// Command homesync-client keeps a local folder in step with a HomeSync server.
//
// It is written against docs/PROTOCOL.md rather than against the Mac client,
// which is the point of having written the protocol down: the two agree because
// they both implement the document, not because one copied the other. It proves
// itself with the same conformance suite.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/schmidt-gabriel/homesync/clients/linux/internal/sync"
)

const usage = `homesync-client — keeps a folder in step with a HomeSync server

Usage:
  homesync-client [flags]

Flags:
  -url     Server URL          (or HOMESYNC_URL)
  -token   Device token        (or HOMESYNC_TOKEN)
  -root    Folder to sync      (or HOMESYNC_ROOT, default ~/HomeSync)
  -device  Name used in conflict copies (or HOMESYNC_DEVICE, default hostname)
  -once    Run one cycle and exit, rather than staying up

Send SIGUSR1 to sync now:

  systemctl --user kill -s USR1 homesync   # or: pkill -USR1 homesync-client

The token is minted on the server with "homesync device add <name>" and is
shown once. Put it in HOMESYNC_TOKEN rather than on the command line, where it
would be visible to anyone who can list processes.
`

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		url    = flag.String("url", os.Getenv("HOMESYNC_URL"), "server URL")
		token  = flag.String("token", os.Getenv("HOMESYNC_TOKEN"), "device token")
		root   = flag.String("root", os.Getenv("HOMESYNC_ROOT"), "folder to sync")
		device = flag.String("device", os.Getenv("HOMESYNC_DEVICE"), "name used in conflict copies")
		once   = flag.Bool("once", false, "run one cycle and exit")
	)
	flag.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	flag.Parse()

	setupLogging()

	if *url == "" || *token == "" {
		flag.Usage()
		return errors.New("both a server URL and a device token are required")
	}
	if *root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("no -root given and cannot find the home directory: %w", err)
		}
		*root = filepath.Join(home, "HomeSync")
	}

	api := sync.NewClient(*url, *token)
	if api.Insecure() {
		// The token is a bearer credential: anyone holding it has full read and
		// write access. Said once, at info, because a home network is what this
		// is built for — logging it as a warning on every start would train
		// people to ignore warnings.
		slog.Info("talking to the server over plain HTTP, which is fine on a network you trust; "+
			"use https:// if it is reachable from outside it", "url", *url)
	}

	engine, err := sync.NewEngine(sync.Config{
		Root:              *root,
		DeviceName:        *device,
		MaxDeletesPerPull: envInt("HOMESYNC_MAX_DELETES", 100),
		PollInterval:      envDuration("HOMESYNC_POLL_INTERVAL", 5*time.Minute),
	}, api, slog.Default())
	if err != nil {
		return err
	}
	defer engine.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Fail loudly at startup rather than looking healthy and syncing nothing.
	if err := api.Reachable(ctx); err != nil {
		return fmt.Errorf("cannot reach the server: %w", err)
	}
	engine.RefreshIgnoreRules(ctx)

	slog.Info("syncing", "root", engine.Store().Root(), "server", *url)

	if *once {
		summary, err := engine.SyncOnce(ctx)
		slog.Info("cycle complete", "did", summary.String())
		return err
	}

	return serve(ctx, engine, api)
}

// serve runs until cancelled: watches the folder, listens for server events,
// and polls as a backstop.
func serve(ctx context.Context, engine *sync.Engine, api *sync.Client) error {
	// Buffered by one and coalescing: several requests arriving while a cycle
	// runs become one follow-up, rather than being dropped or queued N deep.
	wake := make(chan struct{}, 1)
	request := func() {
		select {
		case wake <- struct{}{}:
		default:
		}
	}

	// Sync now, on demand. A daemon has no menu to click, and the alternative
	// people reach for is restarting the service, which throws away a healthy
	// event stream and a warm state to ask a question the running process could
	// have answered.
	//
	// SIGUSR1 because it is the signal with no other meaning; without this the
	// daemon simply ignores it, which is indistinguishable from a sync that
	// found nothing to do.
	syncNow := make(chan os.Signal, 1)
	signal.Notify(syncNow, syscall.SIGUSR1)
	defer signal.Stop(syncNow)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-syncNow:
				slog.Info("sync requested")
				request()
			}
		}
	}()

	watcher, err := sync.NewWatcher(engine.Store().Root(), engine.Rules, slog.Default())
	if err != nil {
		return err
	}
	defer watcher.Close()

	local := make(chan struct{}, 1)
	go func() {
		if err := watcher.Run(ctx, local); err != nil && ctx.Err() == nil {
			slog.Warn("watcher stopped", "err", err)
		}
	}()
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-local:
				request()
			}
		}
	}()

	go watchEvents(ctx, api, request)

	// Whatever changed while this client was not running has to be caught too.
	request()

	ticker := time.NewTicker(engine.PollInterval())
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			slog.Info("shutting down")
			return nil
		case <-ticker.C:
			request()
		case <-wake:
			summary, err := engine.SyncOnce(ctx)
			switch {
			case err == nil:
				if !summary.Empty() {
					slog.Info("synced", "did", summary.String())
				}
			case ctx.Err() != nil:
				return nil
			default:
				var paused *sync.PausedError
				if errors.As(err, &paused) {
					// Not retried on a timer: a guard that trips every five
					// minutes is noise, and the reason needs a person.
					slog.Error("paused", "reason", paused.Reason)
					continue
				}
				slog.Warn("sync failed", "err", err, "note", "will retry")
			}
		}
	}
}

// watchEvents turns the server's SSE stream into wake-ups.
//
// Losing the stream never loses data: the next cycle asks for everything since
// our own revision and catches up in one go. So a disconnect is ordinary, and
// reconnecting with backoff is the whole recovery strategy.
func watchEvents(ctx context.Context, api *sync.Client, request func()) {
	backoff := time.Second
	revs := make(chan int64, 8)

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-revs:
				request()
			}
		}
	}()

	for ctx.Err() == nil {
		err := api.Events(ctx, revs)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			slog.Debug("event stream dropped", "err", err)
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, time.Minute)
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
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))
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
