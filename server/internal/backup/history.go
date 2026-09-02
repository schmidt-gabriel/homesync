package backup

import (
	"context"
	"database/sql"
	"encoding/json"
	"math"
)

// historyLimit is how many runs are kept. One a day, so this is a couple of
// years — enough to see a pattern, small enough to send to the page whole.
const historyLimit = 500

// Stats are the numbers rsync prints under --stats. Sizes are bytes.
type Stats struct {
	FilesTotal       int64   `json:"files_total"`
	FilesCreated     int64   `json:"files_created"`
	FilesDeleted     int64   `json:"files_deleted"`
	FilesTransferred int64   `json:"files_transferred"`
	TotalSize        int64   `json:"total_size"`
	TransferredSize  int64   `json:"transferred_size"`
	LiteralData      int64   `json:"literal_data"`
	BytesSent        int64   `json:"bytes_sent"`
	BytesReceived    int64   `json:"bytes_received"`
	Speedup          float64 `json:"speedup"`
}

// Run statuses.
const (
	StatusSuccess = "success"
	StatusFailed  = "failed"
	// StatusSkipped is a run that declined to start: another was already
	// going. It is not a failure and does not break the success streak.
	StatusSkipped = "skipped"
	// StatusCancelled is a run someone stopped. Also not a failure: nothing
	// went wrong, a person changed their mind, and a success streak that broke
	// because of that would be reporting something that did not happen.
	StatusCancelled = "cancelled"
)

// Run triggers.
const (
	TriggerSchedule = "schedule"
	TriggerManual   = "manual"
)

// Run is one attempt, successful or not.
type Run struct {
	ID         int64  `json:"id"`
	StartedAt  int64  `json:"started_at"`  // unix milliseconds
	FinishedAt int64  `json:"finished_at"` // unix milliseconds
	DurationMS int64  `json:"duration_ms"`
	Status     string `json:"status"`
	Trigger    string `json:"trigger"`
	Snapshot   string `json:"snapshot,omitempty"`
	Error      string `json:"error,omitempty"`
	// Warning is set on a run that finished but has something to say — files
	// that vanished mid-copy, snapshots that could not be pruned.
	Warning string   `json:"warning,omitempty"`
	Pruned  []string `json:"pruned"`
	Stats   *Stats   `json:"stats,omitempty"`
}

func recordRun(ctx context.Context, db *sql.DB, run Run) error {
	stats := ""
	if run.Stats != nil {
		encoded, err := json.Marshal(run.Stats)
		if err != nil {
			return err
		}
		stats = string(encoded)
	}
	pruned, err := json.Marshal(run.Pruned)
	if err != nil {
		return err
	}

	if _, err := db.ExecContext(ctx, `
        INSERT INTO backup_runs
            (started_at, finished_at, duration_ms, status, trigger, snapshot, error, warning, stats, pruned)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		run.StartedAt, run.FinishedAt, run.DurationMS, run.Status, run.Trigger,
		run.Snapshot, run.Error, run.Warning, stats, string(pruned)); err != nil {
		return err
	}

	// Bounded here rather than by a periodic sweep: the table only grows on
	// this path, so this is the one place that can let it run away.
	_, err = db.ExecContext(ctx, `
        DELETE FROM backup_runs WHERE id NOT IN (
            SELECT id FROM backup_runs ORDER BY id DESC LIMIT ?
        )`, historyLimit)
	return err
}

// listRuns returns the most recent runs, newest first.
func listRuns(ctx context.Context, db *sql.DB, limit int) ([]Run, error) {
	rows, err := db.QueryContext(ctx, `
        SELECT id, started_at, finished_at, duration_ms, status, trigger,
               snapshot, error, warning, stats, pruned
        FROM backup_runs ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	runs := []Run{}
	for rows.Next() {
		var run Run
		var stats, pruned string
		if err := rows.Scan(&run.ID, &run.StartedAt, &run.FinishedAt, &run.DurationMS,
			&run.Status, &run.Trigger, &run.Snapshot, &run.Error, &run.Warning,
			&stats, &pruned); err != nil {
			return nil, err
		}
		if stats != "" {
			var parsed Stats
			if json.Unmarshal([]byte(stats), &parsed) == nil {
				run.Stats = &parsed
			}
		}
		run.Pruned = []string{}
		if pruned != "" {
			// A row that will not decode still describes a run that happened;
			// showing it without its pruned list beats hiding it.
			_ = json.Unmarshal([]byte(pruned), &run.Pruned)
		}
		runs = append(runs, run)
	}
	return runs, rows.Err()
}

// clearRuns empties the history and reports how many rows went. The snapshots
// on the disk are untouched: this is the log of what happened, not the backups
// themselves.
func clearRuns(ctx context.Context, db *sql.DB) (int64, error) {
	result, err := db.ExecContext(ctx, `DELETE FROM backup_runs`)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// Metrics summarise the history for the cards at the top of the tab.
type Metrics struct {
	Runs        int  `json:"runs"`
	Successes   int  `json:"successes"`
	Failures    int  `json:"failures"`
	SuccessRate *int `json:"success_rate"` // percent, nil until something has finished
	// Streak is consecutive successes, newest first, ending at the first
	// failure. Skipped runs neither add to it nor break it.
	Streak          int   `json:"streak"`
	AvgDurationMS   int64 `json:"avg_duration_ms"`
	AvgTransferred  int64 `json:"avg_transferred_bytes"`
	LastRun         *Run  `json:"last_run"`
	LastSuccess     *Run  `json:"last_success"`
	LastSuccessSize int64 `json:"last_success_bytes"`
}

// buildMetrics summarises runs, which must be newest first.
func buildMetrics(runs []Run) Metrics {
	var m Metrics
	m.Runs = len(runs)

	// The averages describe recent behaviour, not the whole history: a job
	// that got slower three months ago should show the new number.
	const window = 30
	var durations, transferred []int64

	for i := range runs {
		run := &runs[i]
		switch run.Status {
		case StatusSuccess:
			m.Successes++
			if m.LastSuccess == nil {
				m.LastSuccess = run
				if run.Stats != nil {
					m.LastSuccessSize = run.Stats.TransferredSize
				}
			}
			if len(durations) < window {
				durations = append(durations, run.DurationMS)
				if run.Stats != nil {
					transferred = append(transferred, run.Stats.TransferredSize)
				}
			}
		case StatusFailed:
			m.Failures++
		}
	}
	if len(runs) > 0 {
		m.LastRun = &runs[0]
	}

	for i := range runs {
		if runs[i].Status == StatusFailed {
			break
		}
		if runs[i].Status == StatusSuccess {
			m.Streak++
		}
	}

	if finished := m.Successes + m.Failures; finished > 0 {
		rate := int(math.Round(100 * float64(m.Successes) / float64(finished)))
		m.SuccessRate = &rate
	}
	m.AvgDurationMS = average(durations)
	m.AvgTransferred = average(transferred)
	return m
}

func average(values []int64) int64 {
	if len(values) == 0 {
		return 0
	}
	var total int64
	for _, value := range values {
		total += value
	}
	return total / int64(len(values))
}
