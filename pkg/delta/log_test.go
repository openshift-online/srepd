package delta

import (
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func changesN(n int) []Change {
	out := make([]Change, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, Change{
			Kind:       StatusChanged,
			IncidentID: fmt.Sprintf("INC-%03d", i),
			Summary:    fmt.Sprintf("change %d", i),
		})
	}
	return out
}

func TestNewLog_Bounds(t *testing.T) {
	tests := []struct {
		name    string
		max     int
		wantMax int
	}{
		{"positive max is honoured", 10, 10},
		{"zero max falls back to default", 0, DefaultMaxChanges},
		{"negative max falls back to default", -5, DefaultMaxChanges},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			l := NewLog(tc.max)
			require.NotNil(t, l)
			assert.Equal(t, tc.wantMax, l.Max())
		})
	}
}

func TestLog_AppendAndRecent(t *testing.T) {
	l := NewLog(10)

	assert.Empty(t, l.Recent(0), "empty log returns no changes")
	assert.Equal(t, 0, l.Len())

	l.Append(changesN(3)...)
	require.Equal(t, 3, l.Len())

	got := l.Recent(0)
	require.Len(t, got, 3, "limit <= 0 returns everything")
	assert.Equal(t, "INC-000", got[0].IncidentID)
	assert.Equal(t, "INC-002", got[2].IncidentID)

	got = l.Recent(2)
	require.Len(t, got, 2, "limit truncates to the most recent entries")
	assert.Equal(t, "INC-001", got[0].IncidentID)
	assert.Equal(t, "INC-002", got[1].IncidentID)

	got = l.Recent(99)
	assert.Len(t, got, 3, "limit larger than the log returns everything")
}

func TestLog_AppendNoChangesIsNoop(t *testing.T) {
	l := NewLog(10)
	l.Append()
	assert.Equal(t, 0, l.Len())
}

func TestLog_EvictsOldestPastMax(t *testing.T) {
	l := NewLog(5)
	l.Append(changesN(8)...)

	require.Equal(t, 5, l.Len(), "log must not grow past max")

	got := l.Recent(0)
	require.Len(t, got, 5)
	assert.Equal(t, "INC-003", got[0].IncidentID, "oldest entries are evicted first")
	assert.Equal(t, "INC-007", got[4].IncidentID, "newest entry is retained")
}

func TestLog_EvictsAcrossMultipleAppends(t *testing.T) {
	l := NewLog(3)
	for _, c := range changesN(6) {
		l.Append(c)
	}
	got := l.Recent(0)
	require.Len(t, got, 3)
	assert.Equal(t, "INC-003", got[0].IncidentID)
	assert.Equal(t, "INC-005", got[2].IncidentID)
}

func TestLog_SingleAppendLargerThanMax(t *testing.T) {
	l := NewLog(2)
	l.Append(changesN(10)...)
	got := l.Recent(0)
	require.Len(t, got, 2, "a single oversized append is trimmed to max")
	assert.Equal(t, "INC-008", got[0].IncidentID)
	assert.Equal(t, "INC-009", got[1].IncidentID)
}

func TestLog_RecentReturnsCopy(t *testing.T) {
	l := NewLog(10)
	l.Append(changesN(3)...)

	got := l.Recent(0)
	require.Len(t, got, 3)

	// Mutating the returned slice must not affect the log.
	got[0].IncidentID = "MUTATED"
	got = append(got, Change{IncidentID: "APPENDED"}) //nolint:staticcheck // intentional: proves the copy is independent
	_ = got

	fresh := l.Recent(0)
	require.Len(t, fresh, 3, "appending to the returned slice must not extend the log")
	assert.Equal(t, "INC-000", fresh[0].IncidentID,
		"mutating the returned slice must not affect stored changes")
}

func TestLog_AppendCopiesInput(t *testing.T) {
	l := NewLog(10)
	src := changesN(2)
	l.Append(src...)

	// Mutating the caller's slice must not affect the log.
	src[0].IncidentID = "MUTATED"

	got := l.Recent(0)
	require.Len(t, got, 2)
	assert.Equal(t, "INC-000", got[0].IncidentID,
		"the log must not alias the caller's backing array")
}

func TestLog_ConcurrentAppendAndRecent(t *testing.T) {
	l := NewLog(50)

	const writers = 4
	const readers = 4
	const iterations = 200

	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				l.Append(Change{
					Kind:       IncidentNew,
					IncidentID: fmt.Sprintf("W%d-%d", id, i),
					Summary:    "concurrent",
				})
			}
		}(w)
	}
	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				got := l.Recent(10)
				if len(got) > 10 {
					panic("Recent returned more than the requested limit")
				}
			}
		}()
	}
	wg.Wait()

	assert.Equal(t, 50, l.Len(), "log stays bounded under concurrent writers")
}

func TestLog_NilReceiverIsSafe(t *testing.T) {
	var l *Log
	assert.NotPanics(t, func() {
		l.Append(changesN(1)...)
		assert.Empty(t, l.Recent(0))
		assert.Equal(t, 0, l.Len())
		assert.Equal(t, 0, l.Max())
	}, "a nil log must behave as an empty, write-discarding log")
}
