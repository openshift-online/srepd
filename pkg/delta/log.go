package delta

import "sync"

// DefaultMaxChanges is the number of changes a Log retains when no explicit
// bound is given. Matches the historical in-model bound.
const DefaultMaxChanges = 200

// Log is a mutex-guarded, bounded ring of recent changes.
//
// It exists because the TUI model is copied on every Update (the Bubble Tea
// Update method has a value receiver), so a change log stored as a slice field
// is invisible to any closure that captured an earlier copy of the model.
// Holding a *Log instead means the pointer survives the copies and every model
// generation appends to — and every tool handler reads from — one shared
// buffer. The mutex is required because tool handlers run on tea.Cmd
// goroutines while Update appends on the main loop.
//
// The zero value is not usable; construct with NewLog. A nil *Log is safe to
// use: writes are discarded and reads return nothing.
type Log struct {
	mu      sync.Mutex
	changes []Change
	max     int
}

// NewLog returns a Log retaining at most max changes. A max of zero or less
// falls back to DefaultMaxChanges.
func NewLog(max int) *Log {
	if max <= 0 {
		max = DefaultMaxChanges
	}
	return &Log{max: max}
}

// Append records changes, evicting the oldest entries once the log exceeds its
// bound. The input slice is not retained.
func (l *Log) Append(cs ...Change) {
	if l == nil || len(cs) == 0 {
		return
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	l.changes = append(l.changes, cs...)
	if len(l.changes) > l.max {
		// Re-slice into a fresh backing array so the evicted prefix becomes
		// garbage rather than being pinned by the retained tail.
		kept := make([]Change, l.max)
		copy(kept, l.changes[len(l.changes)-l.max:])
		l.changes = kept
	}
}

// Recent returns a copy of up to limit of the most recent changes, oldest
// first. A limit of zero or less returns every retained change. The caller
// owns the returned slice.
func (l *Log) Recent(limit int) []Change {
	if l == nil {
		return nil
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	if len(l.changes) == 0 {
		return nil
	}

	start := 0
	if limit > 0 && len(l.changes) > limit {
		start = len(l.changes) - limit
	}

	out := make([]Change, len(l.changes)-start)
	copy(out, l.changes[start:])
	return out
}

// Len returns the number of changes currently retained.
func (l *Log) Len() int {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.changes)
}

// Max returns the log's retention bound.
func (l *Log) Max() int {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.max
}
