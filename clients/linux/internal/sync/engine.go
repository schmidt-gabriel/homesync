package sync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path"
	"sort"
	"strings"
	"time"
)

// Config is everything the engine needs to run.
type Config struct {
	Root       string
	DeviceName string

	// MaxDeletesPerPull and MaxDeleteFraction are a safety net, not a tuning
	// knob. A damaged state database, or a server whose volume failed to
	// mount, can produce a change set that deletes everything.
	MaxDeletesPerPull int
	MaxDeleteFraction float64

	// PollInterval is the backstop for the event stream. Real-time updates
	// arrive over SSE; this only matters when that connection is down.
	PollInterval time.Duration
}

func (c *Config) applyDefaults() {
	if c.MaxDeletesPerPull == 0 {
		c.MaxDeletesPerPull = 100
	}
	if c.MaxDeleteFraction == 0 {
		c.MaxDeleteFraction = 0.25
	}
	if c.PollInterval == 0 {
		c.PollInterval = 5 * time.Minute
	}
	if c.DeviceName == "" {
		if host, err := os.Hostname(); err == nil {
			c.DeviceName = host
		} else {
			c.DeviceName = "linux"
		}
	}
}

// Summary is what one sync cycle did.
type Summary struct {
	Downloaded      int
	Uploaded        int
	DeletedLocally  int
	DeletedRemotely int
	DirsCreated     int
	Conflicts       []string
}

func (s Summary) Empty() bool {
	return s.Downloaded == 0 && s.Uploaded == 0 && s.DeletedLocally == 0 &&
		s.DeletedRemotely == 0 && s.DirsCreated == 0 && len(s.Conflicts) == 0
}

func (s Summary) String() string {
	if s.Empty() {
		return "nothing to do"
	}
	var parts []string
	if s.Downloaded > 0 {
		parts = append(parts, fmt.Sprintf("down %d", s.Downloaded))
	}
	if s.Uploaded > 0 {
		parts = append(parts, fmt.Sprintf("up %d", s.Uploaded))
	}
	if s.DeletedLocally > 0 {
		parts = append(parts, fmt.Sprintf("-%d local", s.DeletedLocally))
	}
	if s.DeletedRemotely > 0 {
		parts = append(parts, fmt.Sprintf("-%d remote", s.DeletedRemotely))
	}
	if s.DirsCreated > 0 {
		parts = append(parts, fmt.Sprintf("+%d dirs", s.DirsCreated))
	}
	if len(s.Conflicts) > 0 {
		parts = append(parts, fmt.Sprintf("%d conflicts", len(s.Conflicts)))
	}
	return strings.Join(parts, " ")
}

// PausedError means the engine refused to apply a change set rather than
// carrying it out. It is not retried: the reason has to be dealt with.
type PausedError struct{ Reason string }

func (e *PausedError) Error() string { return e.Reason }

// Engine keeps a local folder and a HomeSync server in step.
type Engine struct {
	cfg    Config
	api    *Client
	store  *Store
	state  *State
	rules  *Ignore
	logger *slog.Logger
}

func NewEngine(cfg Config, api *Client, logger *slog.Logger) (*Engine, error) {
	cfg.applyDefaults()

	store, err := NewStore(cfg.Root)
	if err != nil {
		return nil, err
	}
	// Keyed on the resolved root, so the record follows the folder rather than
	// the spelling of the path used to reach it.
	state, err := OpenState(StatePath(store.Root()))
	if err != nil {
		return nil, err
	}

	return &Engine{
		cfg:    cfg,
		api:    api,
		store:  store,
		state:  state,
		rules:  ParseIgnore(state.IgnoreRules()),
		logger: logger,
	}, nil
}

func (e *Engine) Close() error { return e.state.Close() }

func (e *Engine) Store() *Store { return e.store }

// Rules is the ignore set in force, which the watcher needs so it does not
// place a watch on every directory of an ignored build tree.
func (e *Engine) Rules() *Ignore { return e.rules }

func (e *Engine) PollInterval() time.Duration { return e.cfg.PollInterval }

// RefreshIgnoreRules fetches the shared rules, so a pattern added on one
// machine takes effect everywhere.
func (e *Engine) RefreshIgnoreRules(ctx context.Context) {
	doc, err := e.api.IgnoreRules(ctx)
	if err != nil {
		e.logger.Warn("cannot fetch ignore rules", "err", err)
		return
	}
	if doc.Version == e.state.IgnoreVersion() && e.state.IgnoreRules() != "" {
		return
	}
	if err := e.state.SetIgnore(doc.Rules, doc.Version); err != nil {
		e.logger.Warn("cannot store ignore rules", "err", err)
		return
	}
	e.rules = ParseIgnore(doc.Rules)
}

// SyncOnce pulls, then pushes.
//
// The order matters: pulling first keeps the revisions we send as X-Base-Rev
// fresh, which turns what would have been a conflict into an ordinary update
// whenever the two edits did not really overlap in time.
func (e *Engine) SyncOnce(ctx context.Context) (Summary, error) {
	pulled, err := e.pull(ctx)
	if err != nil {
		return pulled, err
	}

	pushed, err := e.push(ctx)

	total := Summary{
		Downloaded:      pulled.Downloaded + pushed.Downloaded,
		Uploaded:        pulled.Uploaded + pushed.Uploaded,
		DeletedLocally:  pulled.DeletedLocally + pushed.DeletedLocally,
		DeletedRemotely: pulled.DeletedRemotely + pushed.DeletedRemotely,
		DirsCreated:     pulled.DirsCreated + pushed.DirsCreated,
		Conflicts:       append(pulled.Conflicts, pushed.Conflicts...),
	}
	return total, err
}

// ── Pull ─────────────────────────────────────────────────────────────────────

func (e *Engine) pull(ctx context.Context) (Summary, error) {
	var summary Summary

	entries, currentRev, err := e.api.AllChanges(ctx, e.state.LastRev())
	if err != nil {
		return summary, err
	}
	if len(entries) == 0 {
		return summary, e.state.SetLastRev(currentRev)
	}

	relevant := entries[:0:0]
	for _, entry := range entries {
		if !e.rules.Excludes(entry.Path, entry.IsDir()) {
			relevant = append(relevant, entry)
		}
	}

	if err := e.checkDeleteGuard(relevant); err != nil {
		return summary, err
	}

	for _, entry := range relevant {
		if err := ctx.Err(); err != nil {
			return summary, err
		}
		if err := e.apply(ctx, entry, &summary); err != nil {
			// One unreachable path must not stop the rest of the batch, but we
			// must not advance past it either: leaving LastRev where it is
			// means the next cycle retries.
			if IsNotFound(err) {
				continue
			}
			return summary, err
		}
	}

	return summary, e.state.SetLastRev(currentRev)
}

// checkDeleteGuard refuses a pull that would delete an implausible share of
// the tree.
//
// A damaged state database or an unmounted server volume both look, from here,
// exactly like "the user deleted everything". Pausing and saying so is the only
// safe response.
//
// It counts only what this pull would really remove. Judging it on every
// tombstone in the change set instead would sweep in paths holding local
// content the engine already refuses to touch, pausing over deletions that were
// never going to happen.
func (e *Engine) checkDeleteGuard(entries []Entry) error {
	deletions := 0
	for _, entry := range entries {
		if !entry.Deleted {
			continue
		}
		local, ok := e.store.Describe(entry.Path)
		if !ok {
			continue
		}
		if deletable, _ := e.deletable(entry.Path, local); deletable {
			deletions++
		}
	}
	if deletions == 0 {
		return nil
	}

	known, err := e.state.Count()
	if err != nil {
		return err
	}

	// A fraction of nothing rounds to nothing, and a limit of zero would stop
	// the very first pull. With no record to take a proportion of, the absolute
	// cap is the only meaningful bound.
	limit := e.cfg.MaxDeletesPerPull
	if known > 0 {
		limit = min(e.cfg.MaxDeletesPerPull, int(float64(known)*e.cfg.MaxDeleteFraction))
		limit = max(limit, 1)
	}

	if deletions <= limit {
		return nil
	}

	return &PausedError{Reason: fmt.Sprintf(
		"refusing to delete %d local files in one go (limit %d). This usually means "+
			"the server's volume is not mounted or the local state is damaged, not "+
			"that the files were really deleted", deletions, limit)}
}

func (e *Engine) apply(ctx context.Context, entry Entry, summary *Summary) error {
	recorded, known, err := e.state.Get(entry.Path)
	if err != nil {
		return err
	}

	// Our own write, coming back to us.
	//
	// Pushing advances the server's revision past the cursor we pulled from, so
	// the next pull always re-reads what we just uploaded. Treated as a remote
	// change, the client ends up fighting itself: a local edit made after the
	// upload looks like both sides having changed and gets parked as a bogus
	// conflict, and a local deletion gets undone by the file being downloaded
	// again before the push phase ever deletes it.
	//
	// Recognising the echo by revision *and* content is what settles it.
	if !entry.Deleted && known && recorded.Rev == entry.Rev &&
		(entry.SHA256 == "" || recorded.SHA256 == entry.SHA256) {
		return nil
	}

	if entry.Deleted {
		return e.applyDeletion(entry, summary)
	}

	if entry.IsDir() {
		if !e.store.Exists(entry.Path) {
			if err := e.store.Mkdir(entry.Path); err != nil {
				return err
			}
			summary.DirsCreated++
		}
		return e.state.Record(Synced{
			Path: entry.Path, Type: "dir", MTime: entry.MTime, Rev: entry.Rev,
		})
	}

	return e.applyFile(ctx, entry, summary)
}

func (e *Engine) applyDeletion(entry Entry, summary *Summary) error {
	local, ok := e.store.Describe(entry.Path)
	if !ok {
		return e.state.Forget(entry.Path)
	}

	deletable, err := e.deletable(entry.Path, local)
	if err != nil {
		return err
	}
	if !deletable {
		// Keep it, and forget it. The push phase then sees a path on disk the
		// server does not have and uploads it, which is what "this is local
		// content" means in the only vocabulary the two sides share.
		return e.state.Forget(entry.Path)
	}

	if err := e.store.Remove(entry.Path); err != nil {
		return err
	}
	summary.DeletedLocally++
	return e.state.Forget(entry.Path)
}

// deletable reports whether a tombstone may remove what is on disk here.
//
// Only content we recorded as synced, and that has not changed since, may go on
// the server's word alone. Two cases are refused:
//
// A path with no record was never confirmed as ours. It is a file the user put
// there, or one left behind when the state database was lost — and a lost
// database is exactly when the server's tombstones stop describing this
// machine. Deleting on that basis destroys work the server never had.
//
// A path that changed since we last agreed holds an edit the server has not
// seen, which deleting would silently discard.
func (e *Engine) deletable(rel string, local Local) (bool, error) {
	recorded, known, err := e.state.Get(rel)
	if err != nil || !known {
		return false, err
	}

	if local.Type != "file" {
		// Removing a directory takes everything under it, including files that
		// were never recorded, so it has to be empty to be safe. One that still
		// holds something is kept and, like any other local content, offered
		// back to the server on the next push.
		return e.store.IsEmptyDir(rel)
	}

	if local.Size == recorded.Size && local.MTime == recorded.MTime {
		return true, nil
	}
	sum, err := e.store.Hash(rel)
	if err != nil {
		return false, nil
	}
	return sum == recorded.SHA256, nil
}

func (e *Engine) applyFile(ctx context.Context, entry Entry, summary *Summary) error {
	recorded, known, err := e.state.Get(entry.Path)
	if err != nil {
		return err
	}
	local, onDisk := e.store.Describe(entry.Path)

	// Already identical: record where it now stands and move on without
	// spending a request.
	if onDisk && local.Type == "file" && entry.SHA256 != "" {
		if sum, err := e.store.Hash(entry.Path); err == nil && sum == entry.SHA256 {
			return e.state.Record(Synced{
				Path: entry.Path, Type: "file", Size: local.Size,
				MTime: local.MTime, SHA256: entry.SHA256, Rev: entry.Rev,
			})
		}
	}

	// Both sides moved. Park the local version under a name that says where it
	// came from, then take the server's. Nothing is lost: the copy is uploaded
	// on the next push and appears on every machine.
	if onDisk && local.Type == "file" && known {
		sum, err := e.store.Hash(entry.Path)
		if err == nil && sum != recorded.SHA256 {
			parked := ConflictName(entry.Path, e.cfg.DeviceName, time.Now())
			if err := e.store.Move(entry.Path, parked); err != nil {
				return err
			}
			if err := e.state.Forget(entry.Path); err != nil {
				return err
			}
			summary.Conflicts = append(summary.Conflicts, parked)
			e.logger.Warn("conflict: kept the local version alongside the server's",
				"path", entry.Path, "local_copy", parked)
		}
	}

	tmp, rev, sha, err := e.api.Download(ctx, entry.Path, e.store.TempDir())
	if err != nil {
		return err
	}
	if err := e.store.Install(tmp, entry.Path, entry.MTime); err != nil {
		os.Remove(tmp)
		return err
	}

	// Read back what actually landed rather than assuming: if setting the
	// modification time failed, recording the intended value would make every
	// later scan think the file had changed and re-upload it.
	installed, _ := e.store.Describe(entry.Path)
	if entry.SHA256 != "" {
		sha = entry.SHA256
	}
	if entry.Rev != 0 {
		rev = entry.Rev
	}

	summary.Downloaded++
	return e.state.Record(Synced{
		Path: entry.Path, Type: "file", Size: installed.Size,
		MTime: installed.MTime, SHA256: sha, Rev: rev,
	})
}

// ── Push ─────────────────────────────────────────────────────────────────────

func (e *Engine) push(ctx context.Context) (Summary, error) {
	var summary Summary

	onDisk, err := e.store.Scan(e.rules)
	if err != nil {
		return summary, err
	}
	recorded, err := e.state.All()
	if err != nil {
		return summary, err
	}

	// Directories first, and shallowest first, so an empty one survives the
	// round trip. A directory with files in it is created implicitly by its
	// contents.
	var dirs []string
	for rel, file := range onDisk {
		if file.Type == "dir" {
			if _, ok := recorded[rel]; !ok {
				dirs = append(dirs, rel)
			}
		}
	}
	sort.Strings(dirs)
	for _, rel := range dirs {
		if err := e.api.Mkdir(ctx, rel); err != nil {
			return summary, err
		}
		if err := e.state.Record(Synced{
			Path: rel, Type: "dir", MTime: onDisk[rel].MTime,
		}); err != nil {
			return summary, err
		}
		summary.DirsCreated++
	}

	var files []string
	for rel, file := range onDisk {
		if file.Type == "file" {
			files = append(files, rel)
		}
	}
	sort.Strings(files)

	for _, rel := range files {
		if err := ctx.Err(); err != nil {
			return summary, err
		}
		if err := e.pushFile(ctx, rel, onDisk[rel], recorded[rel], &summary); err != nil {
			return summary, err
		}
	}

	return summary, e.pushDeletions(ctx, onDisk, recorded, &summary)
}

func (e *Engine) pushFile(ctx context.Context, rel string, file Local, known Synced, summary *Summary) error {
	// Cheap check first. Hashing every file on every cycle would make the
	// folder unusable at any real size; size and mtime are a good enough filter
	// to decide what is worth hashing.
	if known.Path != "" && known.Size == file.Size && known.MTime == file.MTime {
		return nil
	}

	// Snapshot before doing anything else with it. Everything below describes
	// this copy, which cannot change underneath us.
	snapshot, err := e.store.Snapshot(rel)
	if err != nil {
		// Vanished between the scan and now. The next cycle will see it gone
		// and handle it as a deletion.
		return nil
	}
	defer os.Remove(snapshot)

	sum, err := HashFile(snapshot)
	if err != nil {
		return err
	}

	// Content identical despite a touched mtime. Record the new mtime so we
	// stop re-hashing it, but do not upload: a stray `touch` must not wake
	// every other machine.
	if known.Path != "" && known.SHA256 == sum {
		return e.state.Record(Synced{
			Path: rel, Type: "file", Size: file.Size,
			MTime: file.MTime, SHA256: sum, Rev: known.Rev,
		})
	}

	// The size recorded has to be the snapshot's, not the one the scan saw: if
	// the file grew or shrank in between, the scan's figure describes bytes
	// nobody uploaded.
	size := file.Size
	if info, err := os.Stat(snapshot); err == nil {
		size = info.Size()
	}

	response, err := e.api.Upload(ctx, rel, snapshot, known.Rev, sum)
	if err == nil {
		summary.Uploaded++
		return e.state.Record(Synced{
			Path: rel, Type: "file", Size: size,
			MTime: file.MTime, SHA256: sum, Rev: response.Rev,
		})
	}

	if !IsCode(err, "conflict") {
		return err
	}

	// The server kept its version and stored ours under another name. Take its
	// version now so both exist here too; ours comes back down on the next pull
	// under the new name.
	if conflict := conflictName(err); conflict != "" {
		summary.Conflicts = append(summary.Conflicts, conflict)
		e.logger.Warn("conflict: the server stored our version separately",
			"path", rel, "stored_as", conflict)
	}

	tmp, rev, sha, err := e.api.Download(ctx, rel, e.store.TempDir())
	if err != nil {
		return err
	}
	if err := e.store.Install(tmp, rel, 0); err != nil {
		os.Remove(tmp)
		return err
	}

	installed, _ := e.store.Describe(rel)
	if sha == "" {
		sha, _ = e.store.Hash(rel)
	}
	summary.Downloaded++
	return e.state.Record(Synced{
		Path: rel, Type: "file", Size: installed.Size,
		MTime: installed.MTime, SHA256: sha, Rev: rev,
	})
}

// conflictName is the name the server parked our body under, from the error it
// answered with.
func conflictName(err error) string {
	var se *ServerError
	if errors.As(err, &se) {
		return se.Conflict
	}
	return ""
}

func (e *Engine) pushDeletions(ctx context.Context, onDisk map[string]Local, recorded map[string]Synced, summary *Summary) error {
	// Deepest first, so a directory's children are gone before we try to
	// delete it and the server does not have to answer `not_empty`.
	var gone []string
	for rel := range recorded {
		if _, stillThere := onDisk[rel]; !stillThere {
			gone = append(gone, rel)
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(gone)))

	for _, rel := range gone {
		known := recorded[rel]

		// A path we have never uploaded cannot be deleted remotely; drop it
		// from our record and move on.
		if known.Rev == 0 {
			if err := e.state.Forget(rel); err != nil {
				return err
			}
			continue
		}

		endpoint := "files"
		if known.Type == "dir" {
			endpoint = "dirs"
		}

		err := e.api.Delete(ctx, endpoint, rel, known.Rev)
		switch {
		case err == nil:
			summary.DeletedRemotely++
		case IsNotFound(err):
			// Someone else got there first.
		case IsCode(err, "stale"):
			// It changed on the server after we last saw it. Deleting would
			// discard that edit, so leave it: the next pull brings the newer
			// version back down.
		case IsCode(err, "not_empty"):
			// Its children go first; the next cycle will get it.
			continue
		default:
			return err
		}

		if err := e.state.Forget(rel); err != nil {
			return err
		}
	}
	return nil
}

// ConflictName builds `<stem>.conflict-<device>-<yyyyMMdd-HHmmss><ext>`,
// matching what the server generates, so a copy made here is
// indistinguishable from one made there.
func ConflictName(rel, device string, when time.Time) string {
	dir, base := path.Split(rel)
	ext := path.Ext(base)
	stem := strings.TrimSuffix(base, ext)

	name := fmt.Sprintf("%s.conflict-%s-%s%s",
		stem, sanitiseDevice(device), when.Format("20060102-150405"), ext)
	return dir + name
}

func sanitiseDevice(device string) string {
	var b strings.Builder
	for _, r := range device {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}

	cleaned := strings.Join(strings.FieldsFunc(b.String(), func(r rune) bool {
		return r == '-'
	}), "-")
	if cleaned == "" {
		return "unknown"
	}
	if len(cleaned) > 32 {
		cleaned = cleaned[:32]
	}
	return cleaned
}
