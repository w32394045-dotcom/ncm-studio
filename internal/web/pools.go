package web

import (
	"net/http"
	"path/filepath"

	"ncm-studio/internal/job"
)

// poolState is the shape every batch page reads on load or after a
// reconnection: whether a run is going, what it holds, and how many workers it
// uses.
//
// It is deliberately the same shape for all four pools, because the pages are
// the same page with a different processor behind them: one list, one progress
// bar, one cancel button.
type poolState struct {
	Running bool        `json:"running"`
	Items   []*job.Item `json:"items"`
	Workers int         `json:"workers"`
}

func stateOf(pool *job.Pool) poolState {
	return poolState{
		Running: pool.Running(),
		Items:   pool.Snapshot(),
		Workers: pool.Workers(),
	}
}

// queuePaths turns the paths a page sent into items, dropping anything that
// cannot be addressed. The id is the absolute path, so the same file queued
// twice in one batch is one row rather than two racing rewrites of it.
func queuePaths(paths []string) []*job.Item {
	seen := make(map[string]bool, len(paths))
	items := make([]*job.Item, 0, len(paths))
	for _, p := range paths {
		abs, err := filepath.Abs(p)
		if err != nil || seen[abs] {
			continue
		}
		seen[abs] = true
		items = append(items, &job.Item{ID: abs, Path: abs, Name: filepath.Base(abs)})
	}
	return items
}

// handlePoolState answers a page that has just opened, or one that reconnected
// after the phone slept through part of the run.
func handlePoolState(pool *job.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, stateOf(pool))
	}
}
