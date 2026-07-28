package index

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/schmidt-gabriel/homesync/server/internal/crypt"
)

// settleDelay is how long a path must stay quiet before we index it. A single
// save from an editor produces a burst of write events, and hashing on each one
// would be pure waste.
const settleDelay = 400 * time.Millisecond

// Watcher keeps the index current when files are changed directly in the data
// directory rather than through the API — a network mount, a script, docker cp.
//
// It is an optimisation for latency, never a source of truth: fsnotify drops
// events under load and knows nothing about what happened while the process was
// down. The periodic full Scan is what actually guarantees correctness.
type Watcher struct {
	ix   *Index
	root string
	skip SkipFunc
	key  *crypt.Key

	fs *fsnotify.Watcher

	mu     sync.Mutex
	timers map[string]*time.Timer
}

func NewWatcher(ix *Index, root string, skip SkipFunc, key *crypt.Key) (*Watcher, error) {
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	return &Watcher{
		ix:     ix,
		root:   root,
		skip:   skip,
		key:    key,
		fs:     fsw,
		timers: make(map[string]*time.Timer),
	}, nil
}

// Run watches until the context is cancelled.
func (w *Watcher) Run(ctx context.Context) error {
	defer w.fs.Close()

	if err := w.addTree(w.root); err != nil {
		return err
	}

	for {
		select {
		case <-ctx.Done():
			w.stopTimers()
			return nil

		case event, ok := <-w.fs.Events:
			if !ok {
				return nil
			}
			w.handle(ctx, event)

		case err, ok := <-w.fs.Errors:
			if !ok {
				return nil
			}
			// Losing the watch is survivable — the next full scan repairs it.
			slog.Warn("watcher error", "err", err)
		}
	}
}

// addTree registers a directory and everything under it. fsnotify is not
// recursive, so every subdirectory needs its own watch.
func (w *Watcher) addTree(dir string) error {
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
		if rel, relErr := w.rel(abs); relErr == nil && rel != "" && w.skip != nil && w.skip(rel) {
			return fs.SkipDir
		}
		if err := w.fs.Add(abs); err != nil && !os.IsNotExist(err) {
			slog.Warn("cannot watch directory", "dir", abs, "err", err)
		}
		return nil
	})
}

func (w *Watcher) rel(abs string) (string, error) {
	rel, err := filepath.Rel(w.root, abs)
	if err != nil {
		return "", err
	}
	if rel == "." {
		return "", nil
	}
	return CleanPath(filepath.ToSlash(rel))
}

func (w *Watcher) handle(ctx context.Context, event fsnotify.Event) {
	rel, err := w.rel(event.Name)
	if err != nil || rel == "" {
		return
	}
	if w.skip != nil && w.skip(rel) {
		return
	}

	// A new directory needs its own watch, and it may already contain files
	// that were moved in wholesale — those never generate individual events.
	if event.Has(fsnotify.Create) {
		if info, err := os.Lstat(event.Name); err == nil && info.IsDir() {
			if err := w.addTree(event.Name); err != nil {
				slog.Warn("cannot watch new directory", "dir", event.Name, "err", err)
			}
			w.scheduleTree(ctx, event.Name)
			return
		}
	}

	w.schedule(ctx, rel, event.Name)
}

// scheduleTree queues every path under a newly appeared directory.
func (w *Watcher) scheduleTree(ctx context.Context, dir string) {
	filepath.WalkDir(dir, func(abs string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, relErr := w.rel(abs)
		if relErr != nil || rel == "" {
			return nil
		}
		if w.skip != nil && w.skip(rel) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		w.schedule(ctx, rel, abs)
		return nil
	})
}

// schedule debounces work for one path: repeated events reset the timer, so a
// file being written continuously is indexed once it stops moving.
func (w *Watcher) schedule(ctx context.Context, rel, abs string) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if timer, exists := w.timers[rel]; exists {
		timer.Reset(settleDelay)
		return
	}

	w.timers[rel] = time.AfterFunc(settleDelay, func() {
		w.mu.Lock()
		delete(w.timers, rel)
		w.mu.Unlock()

		if ctx.Err() != nil {
			return
		}
		if err := w.apply(ctx, rel, abs); err != nil {
			slog.Warn("cannot apply filesystem change", "path", rel, "err", err)
		}
	})
}

func (w *Watcher) stopTimers() {
	w.mu.Lock()
	defer w.mu.Unlock()
	for rel, timer := range w.timers {
		timer.Stop()
		delete(w.timers, rel)
	}
}

// apply reconciles one path between disk and index.
func (w *Watcher) apply(ctx context.Context, rel, abs string) error {
	info, err := os.Lstat(abs)
	if errors.Is(err, os.ErrNotExist) {
		prev, found, lookupErr := w.ix.Lookup(ctx, rel)
		if lookupErr != nil || !found || prev.Deleted {
			return lookupErr
		}
		_, err := w.ix.MarkDeleted(ctx, rel, time.Now().UnixMilli())
		return err
	}
	if err != nil {
		return err
	}

	// Symlinks are out of scope for v1, same as in the scanner.
	if info.Mode()&os.ModeSymlink != 0 {
		return nil
	}

	if info.IsDir() {
		prev, found, err := w.ix.Lookup(ctx, rel)
		if err != nil {
			return err
		}
		if found && !prev.Deleted && prev.Type == TypeDir {
			return nil
		}
		_, err = w.ix.Upsert(ctx, Entry{Path: rel, Type: TypeDir, MTime: info.ModTime().UnixMilli()})
		return err
	}

	if !info.Mode().IsRegular() {
		return nil
	}

	prev, found, err := w.ix.Lookup(ctx, rel)
	if err != nil {
		return err
	}
	size, err := PlainSize(abs, info, w.key)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}

	if found && !prev.Deleted && prev.Size == size && prev.MTime == info.ModTime().UnixMilli() {
		return nil
	}

	sum, err := crypt.HashFile(abs, w.key)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}

	// Content unchanged despite a touched mtime: do not burn a revision, or
	// every `touch` would wake every connected machine for nothing.
	if found && !prev.Deleted && prev.SHA256 == sum && prev.Size == size {
		return nil
	}

	_, err = w.ix.Upsert(ctx, Entry{
		Path: rel, Type: TypeFile,
		Size: size, MTime: info.ModTime().UnixMilli(), SHA256: sum,
	})
	return err
}
