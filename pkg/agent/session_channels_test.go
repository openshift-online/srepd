package agent

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestNewSessionWithChannels_ReturnsUsableUnspawnedSession replaces the old
// SetTestChannels mutator. A constructor cannot be pointed at a live session,
// so it removes the real hazard: swapping the channels of a session whose
// readLoop is already running.
func TestNewSessionWithChannels_ReturnsUsableUnspawnedSession(t *testing.T) {
	events := make(chan Event, 1)
	done := make(chan struct{})

	s := NewSessionWithChannels(events, done)
	require.NotNil(t, s)

	assert.NotNil(t, s.lifecycleCtx,
		"the session must be fully initialized, not a half-built object")
	assert.False(t, s.spawned, "the returned session must be unspawned")
	assert.False(t, s.closed)

	events <- Event{Kind: Result, Text: "hello"}
	select {
	case ev := <-s.Events():
		assert.Equal(t, Result, ev.Kind)
		assert.Equal(t, "hello", ev.Text)
	default:
		t.Fatal("Events() must read from the supplied channel")
	}

	close(done)
	select {
	case <-s.Done():
	default:
		t.Fatal("Done() must observe the supplied channel")
	}
}

func TestNewSessionWithChannels_NilChannelsAreSupplied(t *testing.T) {
	s := NewSessionWithChannels(nil, nil)
	require.NotNil(t, s)
	assert.NotNil(t, s.Events(), "a nil events channel must be replaced with a real one")
	assert.NotNil(t, s.Done(), "a nil done channel must be replaced with a real one")
}
