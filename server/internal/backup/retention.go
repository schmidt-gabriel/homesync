package backup

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"
)

// snapshotName is the only directory shape treated as a snapshot. Anything
// else in the backup directory — the marker file, the "latest" symlink, a
// folder someone put there — is left entirely alone, by listing and by
// retention alike.
var snapshotName = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// snapshotLayout is both the directory name and the day it stands for.
const snapshotLayout = "2006-01-02"

// Snapshot is one dated directory on the backup disk.
type Snapshot struct {
	Name  string `json:"name"`
	MTime int64  `json:"mtime"` // unix milliseconds
	// Tiers are the retention rules keeping it: any of daily, weekly, monthly.
	// Empty means the next run deletes it, which is worth showing before it
	// happens rather than explaining afterwards.
	Tiers []string `json:"tiers"`
	Kept  bool     `json:"kept"`
}

// ListSnapshots returns the snapshot directories in dir, newest first. The
// names are ISO dates, so sorting them as strings sorts them by date.
func ListSnapshots(dir string) ([]Snapshot, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}

	snapshots := make([]Snapshot, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() || !snapshotName.MatchString(entry.Name()) {
			continue
		}
		// IsDir on a DirEntry from ReadDir does not follow symlinks, so a link
		// named like a snapshot is skipped rather than descended into.
		snapshot := Snapshot{Name: entry.Name()}
		if info, err := entry.Info(); err == nil {
			snapshot.MTime = info.ModTime().UnixMilli()
		}
		snapshots = append(snapshots, snapshot)
	}

	sort.Slice(snapshots, func(i, j int) bool {
		return snapshots[i].Name > snapshots[j].Name
	})
	return snapshots, nil
}

// Classify tags each snapshot with the retention tiers keeping it alive,
// walking newest to oldest. Grandfather-father-son: the most recent Daily
// snapshots, plus the newest snapshot of each of the last Weekly ISO weeks,
// plus the newest of each of the last Monthly months.
//
// A snapshot can be held by more than one tier, and the counters advance only
// on the snapshot that first claims a week or a month — so "4 weekly" means
// four distinct weeks, not the four newest snapshots that happen to be in one.
//
// Input must be newest first, as ListSnapshots returns it. The slice is
// returned with Tiers and Kept filled in.
func Classify(snapshots []Snapshot, cfg Config) []Snapshot {
	seenWeeks := map[string]bool{}
	seenMonths := map[string]bool{}

	for i := range snapshots {
		snapshot := &snapshots[i]
		snapshot.Tiers = nil

		if i < cfg.Daily {
			snapshot.Tiers = append(snapshot.Tiers, "daily")
		}

		day, err := time.Parse(snapshotLayout, snapshot.Name)
		if err != nil {
			// The name matched the pattern but is not a real date — 2024-02-31.
			// Nothing claims it, so retention will remove it.
			snapshot.Kept = len(snapshot.Tiers) > 0
			continue
		}

		if len(seenWeeks) < cfg.Weekly {
			year, week := day.ISOWeek()
			key := fmt.Sprintf("%04d-W%02d", year, week)
			if !seenWeeks[key] {
				seenWeeks[key] = true
				snapshot.Tiers = append(snapshot.Tiers, "weekly")
			}
		}

		if len(seenMonths) < cfg.Monthly {
			key := snapshot.Name[:7] // YYYY-MM
			if !seenMonths[key] {
				seenMonths[key] = true
				snapshot.Tiers = append(snapshot.Tiers, "monthly")
			}
		}

		snapshot.Kept = len(snapshot.Tiers) > 0
	}
	return snapshots
}

// prune deletes the snapshots no tier claims, oldest first, and returns their
// names. Snapshots share inodes through hard links, so removing one never
// damages another: the content survives as long as any link to it remains.
func prune(dir string, snapshots []Snapshot) ([]string, error) {
	var removed []string
	for i := len(snapshots) - 1; i >= 0; i-- {
		snapshot := snapshots[i]
		if snapshot.Kept {
			continue
		}
		// Rebuilt from the validated directory and a name that matched
		// snapshotName, so this cannot walk anywhere else.
		if err := os.RemoveAll(filepath.Join(dir, snapshot.Name)); err != nil {
			return removed, fmt.Errorf("remove %s: %w", snapshot.Name, err)
		}
		removed = append(removed, snapshot.Name)
	}
	return removed, nil
}
