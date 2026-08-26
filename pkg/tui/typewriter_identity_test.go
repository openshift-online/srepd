package tui

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestTypewriter_StaleTickIsDropped covers the identity defect: two overlapping
// typewriters (e.g. a `:watcher` response landing while an investigation
// verdict is already typing) both drove the single shared typewriterState, so
// a stale tick from the superseded run advanced the successor a second time —
// visibly double-speed output.
//
// Per plan 417's lesson, every async message must carry enough identity to be
// routed correctly even when superseded.
func TestTypewriter_StaleTickIsDropped(t *testing.T) {
	m := createTestModel()
	m.watcherBuffer.Append("")

	// Typewriter A starts and its generation is captured from the tick it
	// schedules.
	cmdA := m.startTypewriter(m.watcherMarker, "alpha bravo charlie delta echo")
	require.NotNil(t, cmdA)
	tickA, ok := cmdA().(typewriterTickMsg)
	require.True(t, ok, "startTypewriter must schedule a typewriterTickMsg")

	// Typewriter B supersedes A before any of A's ticks are delivered.
	cmdB := m.startTypewriter(m.watcherMarker, "one two three four five six seven eight")
	require.NotNil(t, cmdB)
	tickB, ok := cmdB().(typewriterTickMsg)
	require.True(t, ok)

	assert.NotEqual(t, tickA.gen, tickB.gen,
		"each typewriter run must get a distinct generation")

	require.NotNil(t, m.typewriter)
	assert.Equal(t, tickB.gen, m.typewriter.gen,
		"the live typewriter carries the newest generation")

	// Deliver A's stale tick. It must be ignored entirely.
	before := m.typewriter.index
	resultStale, cmdStale := m.Update(tickA)
	afterStale, ok := resultStale.(model)
	require.True(t, ok)

	assert.Equal(t, before, afterStale.typewriter.index,
		"a stale tick must not advance the successor typewriter")
	assert.Nil(t, cmdStale,
		"a stale tick must not reschedule itself")

	// B's own tick advances exactly one step.
	resultLive, cmdLive := afterStale.Update(tickB)
	afterLive, ok := resultLive.(model)
	require.True(t, ok)

	assert.Equal(t, before+typewriterWordsPerTick, afterLive.typewriter.index,
		"a live tick advances exactly one step")
	assert.NotNil(t, cmdLive, "words remain, so the live run reschedules")
}

// TestTypewriter_ZeroGenTickIsDropped ensures a bare typewriterTickMsg{} — the
// shape produced before generations existed — cannot drive a live typewriter.
func TestTypewriter_ZeroGenTickIsDropped(t *testing.T) {
	m := createTestModel()
	m.watcherBuffer.Append("")

	cmd := m.startTypewriter(m.watcherMarker, "alpha bravo charlie")
	require.NotNil(t, cmd)
	require.NotNil(t, m.typewriter)

	before := m.typewriter.index

	result, tickCmd := m.Update(typewriterTickMsg{})
	updated, ok := result.(model)
	require.True(t, ok)

	assert.Equal(t, before, updated.typewriter.index,
		"an unstamped tick must not advance a generation-stamped typewriter")
	assert.Nil(t, tickCmd)
}

// TestTypewriter_GenerationIncrementsPerRun documents that generations are
// monotonic across runs on the same model.
func TestTypewriter_GenerationIncrementsPerRun(t *testing.T) {
	m := createTestModel()
	m.watcherBuffer.Append("")

	seen := make(map[uint64]bool)
	for i := 0; i < 5; i++ {
		cmd := m.startTypewriter(m.watcherMarker, "alpha bravo")
		require.NotNil(t, cmd)
		require.NotNil(t, m.typewriter)
		gen := m.typewriter.gen
		assert.False(t, seen[gen], "generation %d reused", gen)
		seen[gen] = true
	}
	assert.Len(t, seen, 5)
}

// TestTypewriter_EmptyTextDoesNotConsumeGeneration guards against the live
// typewriter being cleared by a no-op start.
func TestTypewriter_EmptyTextDoesNotConsumeGeneration(t *testing.T) {
	m := createTestModel()
	m.watcherBuffer.Append("")

	cmd := m.startTypewriter(m.watcherMarker, "alpha bravo charlie")
	require.NotNil(t, cmd)
	require.NotNil(t, m.typewriter)
	gen := m.typewriter.gen

	assert.Nil(t, m.startTypewriter(m.watcherMarker, ""),
		"empty text schedules nothing")
	require.NotNil(t, m.typewriter, "an empty start must not clear the live run")
	assert.Equal(t, gen, m.typewriter.gen,
		"an empty start must not renumber the live run")
}
