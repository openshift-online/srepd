package agent

import "sync"

// defaultStderrTailBytes bounds how much child-process stderr a session
// retains. The buffer exists only so spawn can look for sessionInUseMarker, so
// the tail is what matters; without a bound a chatty or wedged child could
// grow it without limit for the life of the session.
const defaultStderrTailBytes = 64 * 1024

// tailBuffer is an io.Writer that retains only the last cap bytes written to
// it. Writes always report the full input length so it composes with
// io.MultiWriter, which treats a short write as an error.
//
// Safe for concurrent use: the child's stderr is written from an os/exec
// goroutine while spawn reads String() from the caller's goroutine.
type tailBuffer struct {
	mu  sync.Mutex
	buf []byte
	cap int
}

// newTailBuffer returns a tailBuffer retaining at most capBytes. A
// non-positive capBytes falls back to defaultStderrTailBytes.
func newTailBuffer(capBytes int) *tailBuffer {
	if capBytes <= 0 {
		capBytes = defaultStderrTailBytes
	}
	return &tailBuffer{cap: capBytes}
}

// Write appends p, discarding the oldest bytes once the buffer exceeds its
// cap. It never returns an error and always reports len(p) as written.
func (b *tailBuffer) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	// A single write larger than the cap can skip the append entirely.
	if len(p) >= b.cap {
		b.buf = append(b.buf[:0], p[len(p)-b.cap:]...)
		return len(p), nil
	}

	b.buf = append(b.buf, p...)
	if len(b.buf) > b.cap {
		b.buf = append(b.buf[:0], b.buf[len(b.buf)-b.cap:]...)
	}
	return len(p), nil
}

// String returns the retained tail as a string.
func (b *tailBuffer) String() string {
	if b == nil {
		return ""
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}

// Len returns the number of bytes currently retained.
func (b *tailBuffer) Len() int {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.buf)
}

// Cap returns the buffer's retention bound.
func (b *tailBuffer) Cap() int {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.cap
}
