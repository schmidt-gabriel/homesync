package sync

import (
	"context"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"
)

// settleDelay is how long the tree must stay quiet before a cycle runs.
//
// One save from an editor produces a burst of events, and syncing on each one
// would hash and upload the same file several times. Waiting for quiet turns a
// burst into a single cycle.
const settleDelay = 700 * time.Millisecond

// Watcher reports that something under the root changed.
//
// inotify is not recursive, so every directory is watched individually and new
// ones are added as they appear. It also drops events under load and knows
// nothing about what happened while this process was down, which is why the
// engine polls as well: this is for immediacy, the poll is for correctness.
type Watcher struct {
	root   string
	rules  *Ignore
	fs     *fsnotify.Watcher
	logger *slog.Logger
}

func NewWatcher(root string, rules *Ignore, logger *slog.Logger) (*Watcher, error) {
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	return &Watcher{root: root, rules: rules, fs: fsw, logger: logger}, nil
}

func (w *Watcher) Close() error { return w.fs.Close() }

// Run sends on changed whenever the tree has changed and then gone quiet.
func (w *Watcher) Run(ctx context.Context, changed chan<- struct{}) error {
	if err := w.watchTree(w.root); err != nil {
		return err
	}

	// Stopped, not nil, so the first event starts it. A timer that is reset on
	// every event only fires once the burst is over.
	timer := time.NewTimer(time.Hour)
	if !timer.Stop() {
		<-timer.C
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()

		case event, ok := <-w.fs.Events:
			if !ok {
				return nil
			}
			// A new directory needs its own watch, and it may already have
			// files in it by the time we get here — which the cycle picks up
			// regardless, since it scans rather than trusting the event.
			if event.Has(fsnotify.Create) {
				if info, err := os.Lstat(event.Name); err == nil && info.IsDir() {
					if err := w.watchTree(event.Name); err != nil {
						w.logger.Warn("cannot watch new directory", "path", event.Name, "err", err)
					}
				}
			}
			timer.Reset(settleDelay)

		case err, ok := <-w.fs.Errors:
			if !ok {
				return nil
			}
			// Losing an event never loses data: the next cycle asks the server
			// for everything since our revision and rescans the disk.
			w.logger.Warn("watcher error", "err", err)

		case <-timer.C:
			select {
			case changed <- struct{}{}:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
}

// watchTree adds a watch on dir and everything below it.
func (w *Watcher) watchTree(dir string) error {
	return filepath.WalkDir(dir, func(abs string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if !d.IsDir() {
			return nil
		}

		// No point watching a directory whose contents can never sync, and on
		// a tree with a large ignored build directory it is the difference
		// between a handful of watches and tens of thousands.
		if rel, err := filepath.Rel(w.root, abs); err == nil && rel != "." {
			if w.rules.Excludes(filepath.ToSlash(rel), true) {
				return fs.SkipDir
			}
		}

		if err := w.fs.Add(abs); err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		return nil
	})
}
