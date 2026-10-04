// Package job runs decryption work in the background and broadcasts progress.
package job

import (
	"context"
	"sync"
	"sync/atomic"
)

// State is where an item is in its lifecycle.
type State string

const (
	Pending State = "pending"
	Running State = "running"
	Done    State = "done"
	// Review is an item the run deliberately stopped short of finishing
	// because it could not decide on its own — an ambiguous match, or a track
	// that looks like a backing track. It is a terminal state for the run
	// rather than a failure: nothing is wrong with the file, it is waiting on
	// a person, and it stays put until one answers.
	Review  State = "review"
	Skipped State = "skipped"
	Failed  State = "failed"
)

// Match is one candidate a backfill found for a file, kept on the item so a
// page that reloads can still offer the choice. It is deliberately a flat wire
// shape rather than the matcher's own type: this package stays free of
// internal imports so it can be the shared vocabulary for every kind of job.
type Match struct {
	MusicID    int64    `json:"musicId"`
	Name       string   `json:"name"`
	Artists    []string `json:"artists,omitempty"`
	Album      string   `json:"album,omitempty"`
	DurationMS int64    `json:"durationMs,omitempty"`
	Score      int      `json:"score,omitempty"`
}

// Item is one file being processed.
type Item struct {
	ID   string `json:"id"`
	Path string `json:"path"`
	Name string `json:"name"`

	// LyricsMode overrides the batch's default for this one file, as the
	// integer the settings use. It is a pointer so that "no override" stays
	// distinct from "chose the number that happens to be the default today":
	// the list shows those two differently, and changing the default later
	// must move the second group and leave the first alone.
	LyricsMode *int `json:"lyricsMode,omitempty"`

	// Op is which of a pool's operations this item is for, on the pools that
	// have more than one. The .lrc page exports and imports through the same
	// pool because a user starting one of those has not asked to stop the
	// other, and the two share a list, a progress bar and a cancel button. It
	// is empty for the pools whose processor has only one job.
	Op string `json:"op,omitempty"`

	// HasLyrics is what the file already carried when it was scanned, so the
	// list can offer "skip what is already done" without re-reading anything.
	HasLyrics bool `json:"hasLyrics,omitempty"`

	// Force tells a processor to redo work it would otherwise recognise as
	// already done — translating a file that already carries a machine
	// translation, say. It is the difference between a batch that resumes and
	// one that starts over, and the user is the only one who can tell which
	// they meant.
	Force bool `json:"force,omitempty"`

	State  State  `json:"state"`
	Stage  string `json:"stage,omitempty"`
	Done   int64  `json:"done"`
	Total  int64  `json:"total"`
	Output string `json:"output,omitempty"`
	LRC    string `json:"lrc,omitempty"` // sidecar .lrc, when one was written
	Error  string `json:"error,omitempty"`
	Lyrics string `json:"lyrics,omitempty"` // "", "ok", "none", "failed"
	Warn   string `json:"warn,omitempty"`
	// Removed says the source file was deleted as part of the run, which the
	// settings can ask for once its output is written. The row says so rather
	// than silently outliving the file it describes.
	Removed bool `json:"removed,omitempty"`
	// Cached says this file was already decrypted and nothing was written this
	// run. It is the difference between "this batch produced that audio" and
	// "that audio was there all along", which is what the offer to delete the
	// source turns on: a source is only certainly redundant once its output has
	// just been written, or the record checked, in front of us.
	Cached bool `json:"cached,omitempty"`

	// MusicID is the track this item settled on, and Reason explains why it is
	// in Review. Candidates are the alternates, best first.
	MusicID    int64   `json:"musicId,omitempty"`
	Reason     string  `json:"reason,omitempty"`
	Candidates []Match `json:"candidates,omitempty"`
}

// Result is what a processor reports back for a finished item.
type Result struct {
	Output   string
	LRC      string
	Lyrics   string
	Warnings []string
	// Removed says the source file was deleted as part of this item's work.
	Removed bool
	// Cached says the work was already done and this run only recognised it.
	Cached bool
	// State is the state the item ends in. Empty means Done; a processor that
	// needs a human sets Review and fills in Reason and Candidates.
	State      State
	MusicID    int64
	Reason     string
	Candidates []Match
}

// Reporter lets a processor publish progress while it runs.
type Reporter func(stage string, done, total int64)

// Processor does the actual work for one item.
type Processor func(ctx context.Context, it *Item, report Reporter) (Result, error)

// Event is a message pushed to subscribers.
type Event struct {
	Kind  string `json:"kind"` // "item" or "batch"
	Item  *Item  `json:"item,omitempty"`
	Total int    `json:"total,omitempty"`
	Done  int    `json:"done,omitempty"`
	Run   bool   `json:"run"`
}

// Pool runs items through a processor using a fixed number of workers.
type Pool struct {
	proc    Processor
	workers int

	mu      sync.Mutex
	items   map[string]*Item
	order   []string
	subs    map[chan Event]struct{}
	queue   chan *Item
	cancel  context.CancelFunc
	running bool
	idle    func()

	completed atomic.Int64
	started   atomic.Int64
}

// The range of concurrency the pool will run. MinWorkers is not zero because a
// zero-worker pool would accept items and never run them; MaxWorkers is a
// ceiling on how much memory and CPU one batch may claim, since every worker
// holds a decrypt buffer and a file handle.
const (
	MinWorkers = 1
	MaxWorkers = 16
)

// ClampWorkers brings a requested concurrency into the range the pool accepts.
//
// It is exported so that everything which stores a worker count — the settings
// file, the command line, the web API — stores the number that will actually
// run, rather than a larger one that only looks like the setting took.
func ClampWorkers(n int) int {
	if n < MinWorkers {
		return MinWorkers
	}
	if n > MaxWorkers {
		return MaxWorkers
	}
	return n
}

// New creates a pool, clamping the worker count into range.
func New(workers int, proc Processor) *Pool {
	workers = ClampWorkers(workers)
	return &Pool{
		proc:    proc,
		workers: workers,
		items:   map[string]*Item{},
		subs:    map[chan Event]struct{}{},
	}
}

// SetWorkers changes the worker count for subsequent runs.
func (p *Pool) SetWorkers(n int) {
	n = ClampWorkers(n)
	p.mu.Lock()
	p.workers = n
	p.mu.Unlock()
}

// Workers returns the configured concurrency.
func (p *Pool) Workers() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.workers
}

// OnIdle registers a callback for the end of a run, whether it finished or was
// cancelled. It is the point at which a whole batch is known to be over, which
// is when anything the run buffered should be written out.
func (p *Pool) OnIdle(fn func()) {
	p.mu.Lock()
	p.idle = fn
	p.mu.Unlock()
}

// notifyIdle runs the registered callback without holding the lock, so the
// callback is free to be slow — it may be writing files.
func (p *Pool) notifyIdle() {
	p.mu.Lock()
	fn := p.idle
	p.mu.Unlock()
	if fn != nil {
		fn()
	}
}

// Subscribe returns a channel of events and a function to unsubscribe.
// The channel is buffered and drops events for a subscriber that stops
// reading, so a stalled browser tab cannot block the workers.
func (p *Pool) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, 64)
	p.mu.Lock()
	p.subs[ch] = struct{}{}
	p.mu.Unlock()

	return ch, func() {
		p.mu.Lock()
		if _, ok := p.subs[ch]; ok {
			delete(p.subs, ch)
			close(ch)
		}
		p.mu.Unlock()
	}
}

func (p *Pool) broadcast(evt Event) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for ch := range p.subs {
		select {
		case ch <- evt:
		default: // subscriber is behind; drop rather than stall the pool
		}
	}
}

// Start queues items and begins processing. Any run already in progress is
// cancelled first, so starting a new batch never leaves the old one running.
func (p *Pool) Start(items []*Item) {
	p.Cancel()

	ctx, cancel := context.WithCancel(context.Background())
	p.mu.Lock()
	p.cancel = cancel
	p.running = true
	p.order = p.order[:0]
	for _, it := range items {
		it.State = Pending
		it.Done, it.Total, it.Error, it.Stage = 0, 0, "", ""
		it.Output, it.LRC, it.Lyrics, it.Warn = "", "", "", ""
		it.Removed, it.Cached = false, false
		// The per-run answers are cleared; the per-file inputs — Path, Name,
		// LyricsMode, HasLyrics — are what the caller queued and must survive.
		it.MusicID, it.Reason, it.Candidates = 0, "", nil
		p.items[it.ID] = it
		p.order = append(p.order, it.ID)
	}
	total := len(p.order)
	queue := make(chan *Item)
	p.queue = queue
	workers := p.workers
	p.mu.Unlock()

	p.completed.Store(0)
	p.started.Store(int64(total))

	// Announce the batch before any worker can report on it, so subscribers
	// always see "a run started" ahead of the items it contains.
	p.broadcast(Event{Kind: "batch", Total: total, Run: true})

	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.worker(ctx, queue)
		}()
	}
	go func() {
		defer close(queue)
		for _, it := range items {
			select {
			case queue <- it:
			case <-ctx.Done():
				return
			}
		}
	}()

	// The run is over when every worker has drained the queue, not when the
	// first one notices the channel is closed. A worker that drew no items
	// leaves immediately, so closing the run from a worker would announce the
	// batch had finished while its siblings were still busy.
	go func() {
		wg.Wait()
		p.mu.Lock()
		finished := p.queue == queue // a newer batch has not taken over
		if finished {
			p.running = false
		}
		done := int(p.completed.Load())
		p.mu.Unlock()
		if finished {
			p.broadcast(Event{Kind: "batch", Total: total, Done: done, Run: false})
			p.notifyIdle()
		}
	}()
}

// Cancel stops the current run. Items not yet started are marked skipped so
// the UI does not leave them spinning forever.
func (p *Pool) Cancel() {
	p.mu.Lock()
	cancel := p.cancel
	p.cancel = nil
	running := p.running
	p.running = false
	if running {
		for _, id := range p.order {
			if it := p.items[id]; it != nil && it.State == Pending {
				it.State = Skipped
			}
		}
	}
	p.mu.Unlock()

	if cancel != nil {
		cancel()
		p.broadcast(Event{Kind: "batch", Run: false})
	}
}

// Running reports whether a batch is in progress.
func (p *Pool) Running() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.running
}

// Snapshot returns the current items in the order they were queued.
func (p *Pool) Snapshot() []*Item {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]*Item, 0, len(p.order))
	for _, id := range p.order {
		if it := p.items[id]; it != nil {
			copied := *it
			out = append(out, &copied)
		}
	}
	return out
}

// Mutate changes an item that is already in the pool and broadcasts it.
//
// This is how a decision made outside the run — a user confirming the match for
// one item — gets recorded where the state endpoint and every open page will
// see it. An unknown id is ignored rather than inserted: the caller is holding
// a path from a batch that is no longer the current one, and reviving it would
// put a row in the list that the run never queued.
func (p *Pool) Mutate(id string, fn func(*Item)) bool {
	p.mu.Lock()
	live := p.items[id]
	if live == nil {
		p.mu.Unlock()
		return false
	}
	fn(live)
	copied := *live
	run := p.running
	p.mu.Unlock()

	p.broadcast(Event{Kind: "item", Item: &copied, Run: run})
	return true
}

func (p *Pool) worker(ctx context.Context, queue <-chan *Item) {
	for it := range queue {
		if ctx.Err() != nil {
			p.update(it, func(i *Item) { i.State = Skipped })
			continue
		}
		p.update(it, func(i *Item) { i.State = Running; i.Stage = "start" })

		report := func(stage string, done, total int64) {
			p.update(it, func(i *Item) {
				i.Stage, i.Done, i.Total = stage, done, total
			})
		}

		res, err := p.proc(ctx, it, report)
		switch {
		case ctx.Err() != nil:
			// Cancelled mid-flight: the user stopped the batch, so this is
			// skipped rather than broken.
			p.update(it, func(i *Item) {
				i.State = Skipped
				i.Error = ""
			})
		case err != nil:
			p.update(it, func(i *Item) {
				i.State = Failed
				i.Error = err.Error()
			})
		default:
			state := res.State
			if state == "" {
				state = Done
			}
			p.update(it, func(i *Item) {
				i.State = state
				i.Output = res.Output
				i.LRC = res.LRC
				i.Lyrics = res.Lyrics
				i.MusicID = res.MusicID
				i.Reason = res.Reason
				i.Candidates = res.Candidates
				i.Removed = res.Removed
				i.Cached = res.Cached
				if len(res.Warnings) > 0 {
					i.Warn = res.Warnings[0]
				}
			})
		}

		n := p.completed.Add(1)
		p.mu.Lock()
		total := len(p.order)
		p.mu.Unlock()
		p.broadcast(Event{Kind: "batch", Total: total, Done: int(n), Run: ctx.Err() == nil})
	}
}

// update mutates an item under the lock and pushes the new state out.
func (p *Pool) update(it *Item, fn func(*Item)) {
	p.mu.Lock()
	live := p.items[it.ID]
	if live == nil {
		live = it
		p.items[it.ID] = live
	}
	fn(live)
	copied := *live
	run := p.running
	p.mu.Unlock()

	p.broadcast(Event{Kind: "item", Item: &copied, Run: run})
}
