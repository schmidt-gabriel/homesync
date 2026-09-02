package backup

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/schmidt-gabriel/homesync/server/internal/index"
)

// ErrRunning is returned when a second run is asked for while one is going.
var ErrRunning = errors.New("a backup is already running")

// Manager owns the schedule and the one run that may be in flight. There is
// exactly one per server, and it is the only thing that starts a backup.
type Manager struct {
	index *index.Index

	// paths never change after New: they come from the environment, are
	// validated once, and the admin page shows them without offering to edit
	// them. Nothing takes the lock to read them.
	paths Paths

	mu      sync.Mutex
	cfg     Config
	running bool
	// cancelRun stops the run in flight, and is the only way to reach the
	// rsync process: it is started with a context derived from this, so
	// cancelling kills the child. Non-nil only while a run is going.
	cancelRun context.CancelFunc
	// progress is replaced wholesale on every rsync line and read by the
	// status endpoint. nil between runs, so the page cannot show the last
	// run's numbers as though something were happening.
	progress  *Progress
	runStart  time.Time
	runReason string
	baseCtx   context.Context

	// reload wakes the scheduler when the configuration changes, so a new
	// schedule takes effect immediately instead of after the pending timer.
	reload chan struct{}
}

// New reads the stored configuration, seeding it from envDefaults the first
// time the server runs. After that the stored document wins: the admin UI
// writes it, and an environment variable that silently overrode what the page
// shows would make the page a lie.
func New(ctx context.Context, ix *index.Index, paths Paths, envDefaults Config) (*Manager, error) {
	cfg, err := LoadConfig(ctx, ix, envDefaults)
	if err != nil {
		return nil, err
	}
	return &Manager{
		index:  ix,
		paths:  paths,
		cfg:    cfg,
		reload: make(chan struct{}, 1),
	}, nil
}

// Paths reports where this server backs up from and to. Read-only by
// construction: the admin page displays them, and there is no setter.
func (m *Manager) Paths() Paths { return m.paths }

// Config returns the current configuration.
func (m *Manager) Config() Config {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cfg
}

// Save validates and stores a configuration, and wakes the scheduler.
func (m *Manager) Save(ctx context.Context, cfg Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	cfg = cfg.normalised()
	if err := SaveConfig(ctx, m.index, cfg); err != nil {
		return err
	}

	m.mu.Lock()
	m.cfg = cfg
	m.mu.Unlock()

	m.wake()
	return nil
}

func (m *Manager) wake() {
	select {
	case m.reload <- struct{}{}:
	default: // a reload is already pending; one is enough
	}
}

// Run drives the schedule until ctx is cancelled.
func (m *Manager) Run(ctx context.Context) {
	m.mu.Lock()
	m.baseCtx = ctx
	m.mu.Unlock()

	for {
		cfg := m.Config()

		// nil until there is something to wait for: a disabled job, or a
		// schedule that will not parse, blocks on ctx and reload alone.
		var wait <-chan time.Time
		var timer *time.Timer
		if cfg.Enabled {
			schedule, err := ParseSchedule(cfg.Schedule)
			if err != nil {
				// Saved configurations are validated, so this is a document
				// from a downgrade or a hand edit. Say so once and idle rather
				// than firing on a schedule nobody wrote.
				slog.Error("backup schedule will not parse; the job is idle",
					"schedule", cfg.Schedule, "err", err)
			} else if next, ok := schedule.Next(time.Now()); ok {
				timer = time.NewTimer(time.Until(next))
				wait = timer.C
				slog.Info("next backup scheduled", "at", next.Format(time.RFC3339))
			} else {
				slog.Warn("backup schedule never fires", "schedule", cfg.Schedule)
			}
		}

		select {
		case <-ctx.Done():
			stop(timer)
			return
		case <-m.reload:
			// Stopped rather than left to fire: the loop is about to build a
			// new one, and this is inside a loop, so a deferred Stop would
			// pile up one per configuration change.
			stop(timer)
			continue
		case <-wait:
			if _, err := m.runOnce(ctx, TriggerSchedule); err != nil {
				slog.Warn("scheduled backup did not start", "err", err)
			}
		}
	}
}

// Trigger starts a run now, in the background, and returns as soon as it has
// started. The HTTP request that asks for one is long gone before rsync
// finishes, so the run is deliberately not tied to its context.
func (m *Manager) Trigger() error {
	m.mu.Lock()
	ctx := m.baseCtx
	running := m.running
	m.mu.Unlock()

	if running {
		return ErrRunning
	}
	if ctx == nil {
		ctx = context.Background()
	}

	go func() {
		if _, err := m.runOnce(ctx, TriggerManual); err != nil && !errors.Is(err, ErrRunning) {
			slog.Warn("manual backup did not start", "err", err)
		}
	}()
	return nil
}

// runOnce is the only path that starts rsync. The claim on `running` is what
// makes it single-flight — with one process there is no lock file to keep, and
// two rsyncs writing the same snapshot directory would fight over --delete.
func (m *Manager) runOnce(ctx context.Context, trigger string) (Run, error) {
	m.mu.Lock()
	if m.running {
		m.mu.Unlock()
		// Recorded, not just refused: a schedule that keeps colliding with a
		// run that takes longer than its interval is worth seeing in the list.
		skipped := Run{
			StartedAt: time.Now().UnixMilli(), FinishedAt: time.Now().UnixMilli(),
			Status: StatusSkipped, Trigger: trigger, Pruned: []string{},
			Error: "a backup was already running",
		}
		if err := recordRun(ctx, m.index.DB(), skipped); err != nil {
			slog.Warn("cannot record skipped backup", "err", err)
		}
		return skipped, ErrRunning
	}
	cfg := m.cfg
	runCtx, cancel := context.WithCancel(ctx)
	m.running = true
	m.runStart = time.Now()
	m.runReason = trigger
	m.cancelRun = cancel
	m.mu.Unlock()

	defer func() {
		cancel()
		m.mu.Lock()
		m.running = false
		m.progress = nil
		m.cancelRun = nil
		m.mu.Unlock()
	}()

	slog.Info("backup starting", "trigger", trigger, "source", m.paths.Source, "dest", m.paths.Dest)
	run := execute(runCtx, m.paths, cfg, trigger, time.Now(), m.setProgress)

	switch run.Status {
	case StatusSuccess:
		slog.Info("backup finished", "snapshot", run.Snapshot,
			"duration", time.Duration(run.DurationMS)*time.Millisecond,
			"pruned", len(run.Pruned), "warning", run.Warning)
	case StatusCancelled:
		// Not an error: someone asked for this.
		slog.Info("backup stopped", "snapshot", run.Snapshot,
			"duration", time.Duration(run.DurationMS)*time.Millisecond)
	default:
		slog.Error("backup failed", "snapshot", run.Snapshot, "err", run.Error)
	}

	// Recorded with a context of its own: a run cancelled by shutdown still
	// has to leave a trace, and ctx is already done by then.
	recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := recordRun(recordCtx, m.index.DB(), run); err != nil {
		slog.Warn("cannot record backup run", "err", err)
	}
	return run, nil
}

// setProgress is called once per file rsync handles, so it does the least it
// can: take the lock, replace a pointer, release.
func (m *Manager) setProgress(p Progress) {
	m.mu.Lock()
	m.progress = &p
	m.mu.Unlock()
}

// Stop ends the run in flight and reports whether there was one. What rsync
// had written is removed by the run itself: a half-copied directory left in
// the list would read as a complete backup.
func (m *Manager) Stop() bool {
	m.mu.Lock()
	cancel := m.cancelRun
	m.mu.Unlock()

	if cancel == nil {
		return false
	}
	cancel()
	return true
}

// ClearHistory empties the record of past runs, returning how many were
// removed. A run in flight is unaffected: it is recorded when it finishes, so
// clearing during one loses nothing and the run still appears afterwards.
func (m *Manager) ClearHistory(ctx context.Context) (int64, error) {
	return clearRuns(ctx, m.index.DB())
}

// Status is everything the admin page shows on the Backups tab.
type Status struct {
	Config Config `json:"config"`
	// Paths are shown, never offered for editing: they are the container's
	// mounts. The page renders them as facts rather than as fields.
	Paths     Paths  `json:"paths"`
	Health    Health `json:"health"`
	Problem   string `json:"problem,omitempty"`
	Running   bool   `json:"running"`
	RunningAt int64  `json:"running_since,omitempty"`
	// RunningTrigger says whether the run in flight was scheduled or asked
	// for, which is the difference between "it is 03:00" and "someone is
	// waiting for this to finish".
	RunningTrigger string `json:"running_trigger,omitempty"`
	// Progress is present only while a run is in flight, and only once rsync
	// has printed its first line — a run spends its first moments building a
	// file list with nothing to report.
	Progress  *Progress  `json:"progress,omitempty"`
	NextRun   int64      `json:"next_run,omitempty"` // unix milliseconds
	Snapshots []Snapshot `json:"snapshots"`
	History   []Run      `json:"history"`
	Metrics   Metrics    `json:"metrics"`
}

// Status gathers the current picture. It touches the disk, so it is a
// deliberate request rather than something the page polls.
func (m *Manager) Status(ctx context.Context) (Status, error) {
	m.mu.Lock()
	cfg, running, since, reason := m.cfg, m.running, m.runStart, m.runReason
	progress := m.progress
	m.mu.Unlock()

	status := Status{
		Config:    cfg,
		Paths:     m.paths,
		Health:    checkHealth(m.paths),
		Running:   running,
		Snapshots: []Snapshot{},
	}
	status.Problem = status.Health.Problem(m.paths)
	if running {
		status.RunningAt = since.UnixMilli()
		status.RunningTrigger = reason
		status.Progress = progress
	}

	if cfg.Enabled {
		if schedule, err := ParseSchedule(cfg.Schedule); err == nil {
			if next, ok := schedule.Next(time.Now()); ok {
				status.NextRun = next.UnixMilli()
			}
		}
	}

	// A backup directory that cannot be read is already reported by Problem;
	// an empty snapshot list beside that message is the honest answer.
	if snapshots, err := ListSnapshots(m.paths.normalised().Dest); err == nil {
		status.Snapshots = Classify(snapshots, cfg)
	}

	history, err := listRuns(ctx, m.index.DB(), historyLimit)
	if err != nil {
		return Status{}, err
	}
	status.History = history
	status.Metrics = buildMetrics(history)
	return status, nil
}

// stop releases a timer that is no longer being waited on. A nil timer is the
// disabled case and has nothing to release.
func stop(timer *time.Timer) {
	if timer != nil {
		timer.Stop()
	}
}
