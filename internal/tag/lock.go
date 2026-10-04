package tag

import "sync"

// rewriteLocks serialises in-place rewrites of the same file.
//
// An in-place rewrite copies the whole track to a temporary file and renames it
// over the original. Two of those running against one file do not merely waste
// work — the rename that lands second discards the other's result, and a reader
// that opens the file mid-swap can catch a half-written tag. The lock is keyed
// by path rather than global because rewrites of *different* files are
// independent: a backfill batch wants several in flight at once, and a global
// lock would quietly reduce it to one.
var rewriteLocks = struct {
	mu sync.Mutex
	m  map[string]*lockEntry
}{m: map[string]*lockEntry{}}

type lockEntry struct {
	mu   sync.Mutex
	refs int
}

// lockRewrite takes the lock for one path and returns the function that
// releases it. The entry is dropped once the last holder lets go, so the map
// does not grow with every file the program ever touches.
func lockRewrite(path string) func() {
	rewriteLocks.mu.Lock()
	e := rewriteLocks.m[path]
	if e == nil {
		e = &lockEntry{}
		rewriteLocks.m[path] = e
	}
	e.refs++
	rewriteLocks.mu.Unlock()

	e.mu.Lock()
	return func() {
		e.mu.Unlock()

		rewriteLocks.mu.Lock()
		e.refs--
		if e.refs == 0 {
			delete(rewriteLocks.m, path)
		}
		rewriteLocks.mu.Unlock()
	}
}
