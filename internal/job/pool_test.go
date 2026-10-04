package job

import (
	"context"
	"sync"
	"testing"
	"time"
)

// TestRunningSurvivesIdleWorkers is the regression test for a batch being
// declared finished the instant its queue drained.
//
// Workers that draw no items leave the range loop as soon as the feeder closes
// the channel. When each worker judged completion for itself, an idle worker
// would clear the running flag — and broadcast "batch finished" — while its
// siblings were still decrypting, so the UI dropped back to idle mid-run.
func TestRunningSurvivesIdleWorkers(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{}, 8)

	pool := New(4, func(ctx context.Context, it *Item, report Reporter) (Result, error) {
		started <- struct{}{}
		select {
		case <-release:
			return Result{Output: "/out/" + it.Name}, nil
		case <-ctx.Done():
			return Result{}, ctx.Err()
		}
	})

	pool.Start([]*Item{{ID: "a", Path: "/a.ncm", Name: "a.ncm"}})

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the single item was never picked up")
	}

	// Give the three idle workers ample time to notice the closed queue and
	// (before the fix) wrongly close out the run.
	time.Sleep(250 * time.Millisecond)

	if !pool.Running() {
		t.Error("pool reported idle while an item was still being processed")
	}
	if got := pool.Snapshot()[0].State; got != Running {
		t.Errorf("item state = %q, want %q", got, Running)
	}

	close(release)
	waitFor(t, pool, func(p *Pool) bool { return !p.Running() })
	if got := pool.Snapshot()[0].State; got != Done {
		t.Errorf("final item state = %q, want %q", got, Done)
	}
}

func TestCancelMarksPendingAndRunningAsSkipped(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once

	pool := New(1, func(ctx context.Context, it *Item, report Reporter) (Result, error) {
		once.Do(func() { close(release) })
		<-ctx.Done()
		return Result{}, ctx.Err()
	})

	pool.Start([]*Item{
		{ID: "a", Path: "/a.ncm", Name: "a.ncm"},
		{ID: "b", Path: "/b.ncm", Name: "b.ncm"},
	})

	select {
	case <-release:
	case <-time.After(5 * time.Second):
		t.Fatal("the first item was never picked up")
	}

	pool.Cancel()

	// Cancel flips the running flag at once; the item states catch up as each
	// worker notices the cancelled context, so wait for the states rather than
	// for the flag.
	waitFor(t, pool, func(p *Pool) bool {
		for _, it := range p.Snapshot() {
			if it.State == Running || it.State == Pending {
				return false
			}
		}
		return true
	})

	for _, it := range pool.Snapshot() {
		// A cancelled item was stopped by the user; calling it failed would be
		// a lie the UI then shows as a red error.
		if it.State != Skipped {
			t.Errorf("%s state = %q, want %q", it.Name, it.State, Skipped)
		}
	}
}

func TestStartReplacesThePreviousBatch(t *testing.T) {
	pool := New(2, func(ctx context.Context, it *Item, report Reporter) (Result, error) {
		return Result{Output: "/out/" + it.Name}, nil
	})

	pool.Start([]*Item{{ID: "a", Path: "/a.ncm", Name: "a.ncm"}})
	pool.Start([]*Item{{ID: "b", Path: "/b.ncm", Name: "b.ncm"}})
	waitFor(t, pool, func(p *Pool) bool { return !p.Running() })

	snap := pool.Snapshot()
	if len(snap) != 1 || snap[0].ID != "b" {
		t.Fatalf("snapshot = %+v, want only the second batch", snap)
	}
	if snap[0].State != Done {
		t.Errorf("state = %q, want %q", snap[0].State, Done)
	}
}

func TestSubscribersReceiveItemUpdates(t *testing.T) {
	pool := New(1, func(ctx context.Context, it *Item, report Reporter) (Result, error) {
		report("decrypt", 5, 10)
		return Result{Output: "/out/a.flac", LRC: "/out/a.lrc", Lyrics: "ok"}, nil
	})

	events, unsubscribe := pool.Subscribe()
	defer unsubscribe()

	pool.Start([]*Item{{ID: "a", Path: "/a.ncm", Name: "a.ncm"}})

	var sawStart, sawProgress, sawDone bool
	deadline := time.After(5 * time.Second)
	for !sawStart || !sawProgress || !sawDone {
		select {
		case evt := <-events:
			if evt.Kind == "batch" && evt.Run && !sawStart {
				if !sawProgress && !sawDone {
					sawStart = true
					continue
				}
				t.Error("the batch-start event arrived after item events")
				sawStart = true
				continue
			}
			if evt.Kind != "item" || evt.Item == nil {
				continue
			}
			// Test the terminal state first: a finished item keeps whatever
			// stage and byte counts it had when the processor returned.
			switch {
			case evt.Item.State == Done:
				sawDone = true
				if evt.Item.LRC != "/out/a.lrc" {
					t.Errorf("lrc = %q, want /out/a.lrc", evt.Item.LRC)
				}
				if !evt.Run {
					t.Error("a done item was reported with run=false mid-batch")
				}
			case evt.Item.Stage == "decrypt" && evt.Item.Total == 10:
				sawProgress = true
			}
		case <-deadline:
			t.Fatalf("timed out (start=%v progress=%v done=%v)", sawStart, sawProgress, sawDone)
		}
	}
}

func waitFor(t *testing.T, pool *Pool, cond func(*Pool) bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond(pool) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition never became true")
}

// TestProcessorCanParkAnItemForReview covers the terminal state that is not
// "done" and not "failed": a backfill that will not write a guess stops the
// item where the user can answer it, and keeps what it found so the answer can
// be offered without searching again.
func TestProcessorCanParkAnItemForReview(t *testing.T) {
	pool := New(1, func(ctx context.Context, it *Item, report Reporter) (Result, error) {
		return Result{
			State:   Review,
			Reason:  "version suffix does not match",
			MusicID: 2008994719,
			Candidates: []Match{
				{MusicID: 2008994719, Name: "ひめごと*クライシスターズ(もみじver.)", Score: 8},
				{MusicID: 2008993850, Name: "ひめごと*クライシスターズ", Score: 5},
			},
		}, nil
	})

	pool.Start([]*Item{{ID: "a", Path: "/a.flac", Name: "a.flac"}})
	waitFor(t, pool, func(p *Pool) bool { return !p.Running() })

	got := pool.Snapshot()[0]
	if got.State != Review {
		t.Fatalf("state = %q, want %q", got.State, Review)
	}
	if got.Error != "" {
		t.Errorf("error = %q; waiting on a person is not a failure", got.Error)
	}
	if got.Reason == "" {
		t.Error("a review item gave the user nothing to act on")
	}
	if len(got.Candidates) != 2 || got.Candidates[0].MusicID != 2008994719 {
		t.Errorf("candidates = %+v, want both, best first", got.Candidates)
	}
}

// TestStartKeepsPerFileInputs pins which fields survive a re-run. The mode
// override is a property of the file the user chose, not of the run, so a
// second batch over the same row must not silently fall back to the default.
func TestStartKeepsPerFileInputs(t *testing.T) {
	mode := 3
	pool := New(1, func(ctx context.Context, it *Item, report Reporter) (Result, error) {
		return Result{State: Review, Reason: "first pass"}, nil
	})

	it := &Item{ID: "a", Path: "/a.flac", Name: "a.flac", LyricsMode: &mode, HasLyrics: true}
	pool.Start([]*Item{it})
	waitFor(t, pool, func(p *Pool) bool { return !p.Running() })

	if got := pool.Snapshot()[0]; got.LyricsMode == nil || *got.LyricsMode != 3 {
		t.Errorf("the per-file mode override did not survive the run: %+v", got.LyricsMode)
	}
	if !pool.Snapshot()[0].HasLyrics {
		t.Error("the scan's \"already has lyrics\" flag was cleared by running")
	}

	// The previous run's answer, though, must not leak into the next one.
	pool.Start([]*Item{it})
	waitFor(t, pool, func(p *Pool) bool { return !p.Running() })
	got := pool.Snapshot()[0]
	if got.MusicID != 0 || len(got.Candidates) != 0 {
		t.Errorf("a re-run kept the last run's match: id=%d candidates=%+v", got.MusicID, got.Candidates)
	}
	if got.Reason != "first pass" {
		t.Errorf("reason = %q, want the new run's own", got.Reason)
	}
	if got.LyricsMode == nil || *got.LyricsMode != 3 {
		t.Error("the mode override was lost by the second run")
	}
}

// TestMutateIsVisibleToSnapshotAndSubscribers is what makes confirming a match
// inside a running batch safe: the decision is recorded in the pool rather than
// in the page that made it, so a reload and every other open tab see it.
func TestMutateIsVisibleToSnapshotAndSubscribers(t *testing.T) {
	release := make(chan struct{})
	pool := New(1, func(ctx context.Context, it *Item, report Reporter) (Result, error) {
		<-release
		return Result{State: Review, Reason: "needs a person"}, nil
	})

	events, unsubscribe := pool.Subscribe()
	defer unsubscribe()

	pool.Start([]*Item{{ID: "a", Path: "/a.flac", Name: "a.flac"}})
	waitFor(t, pool, func(p *Pool) bool { return len(p.Snapshot()) == 1 && p.Snapshot()[0].State == Running })

	if !pool.Mutate("a", func(i *Item) {
		i.MusicID = 2008994719
		i.Reason = "confirmed by hand"
	}) {
		t.Fatal("Mutate did not find an item that is in the pool")
	}

	got := pool.Snapshot()[0]
	if got.MusicID != 2008994719 || got.Reason != "confirmed by hand" {
		t.Errorf("snapshot = %+v, want the mutation", got)
	}

	// And it has to reach open pages, or a second tab shows a stale row.
	sawConfirmed := false
	for !sawConfirmed {
		select {
		case evt := <-events:
			if evt.Kind == "item" && evt.Item != nil && evt.Item.Reason == "confirmed by hand" {
				sawConfirmed = true
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the mutation was never broadcast")
		}
	}

	close(release)
	waitFor(t, pool, func(p *Pool) bool { return !p.Running() })
}

// TestMutateIgnoresAnUnknownItem: the caller is holding a path from a batch
// that has been replaced. Inserting it would add a row the current run never
// queued.
func TestMutateIgnoresAnUnknownItem(t *testing.T) {
	pool := New(1, func(ctx context.Context, it *Item, report Reporter) (Result, error) {
		return Result{}, nil
	})
	pool.Start([]*Item{{ID: "a"}})
	waitFor(t, pool, func(p *Pool) bool { return !p.Running() })

	if pool.Mutate("no-such-id", func(i *Item) { i.Reason = "should not appear" }) {
		t.Error("Mutate claimed to find an item that was never queued")
	}
	if len(pool.Snapshot()) != 1 {
		t.Errorf("snapshot grew to %d items", len(pool.Snapshot()))
	}
}
