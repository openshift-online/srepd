package delta

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testNow is the fixed reference clock used by the delta package tests so
// Diff's output is deterministic.
var testNow = time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)

func TestDiff_StampsChangeTimestamps(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)

	prev := []Snapshot{
		{ID: "INC-1", Title: "One", Status: "triggered", Urgency: "high"},
		{ID: "INC-GONE", Title: "Gone"},
	}
	curr := []Snapshot{
		{ID: "INC-1", Title: "One", Status: "acknowledged", Urgency: "high"},
		{ID: "INC-NEW", Title: "New"},
	}

	changes := Diff(prev, curr, now)
	require.NotEmpty(t, changes)

	for _, c := range changes {
		assert.Equal(t, now, c.At,
			"every change from a single Diff must carry the caller's reference time (kind=%s)", c.Kind)
	}
}

func TestDiff_ZeroTimeIsPreserved(t *testing.T) {
	changes := Diff(nil, []Snapshot{{ID: "INC-1", Title: "One"}}, time.Time{})
	require.Len(t, changes, 1)
	assert.True(t, changes[0].At.IsZero(),
		"Diff must not substitute its own clock — it stays pure")
}

func TestRelativeTime(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name string
		at   time.Time
		want string
	}{
		{"zero value", time.Time{}, ""},
		{"same instant", now, "just now"},
		{"sub-second", now.Add(-500 * time.Millisecond), "just now"},
		{"one second", now.Add(-1 * time.Second), "1s ago"},
		{"seconds", now.Add(-45 * time.Second), "45s ago"},
		{"one minute boundary", now.Add(-60 * time.Second), "1m ago"},
		{"minutes", now.Add(-3 * time.Minute), "3m ago"},
		{"minutes rounds down", now.Add(-3*time.Minute - 59*time.Second), "3m ago"},
		{"one hour boundary", now.Add(-60 * time.Minute), "1h ago"},
		{"hours", now.Add(-5 * time.Hour), "5h ago"},
		{"one day boundary", now.Add(-24 * time.Hour), "1d ago"},
		{"days", now.Add(-3 * 24 * time.Hour), "3d ago"},
		{"many days", now.Add(-90 * 24 * time.Hour), "90d ago"},
		{"future timestamp", now.Add(30 * time.Second), "just now"},
		{"far future timestamp", now.Add(72 * time.Hour), "just now"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, relativeTime(tc.at, now))
		})
	}
}

func TestRelativeTime_ZeroNowFallsBackToClock(t *testing.T) {
	// A zero reference time means the caller had no clock; treat the change
	// as un-aged rather than reporting a nonsensical multi-decade age.
	assert.Equal(t, "", relativeTime(time.Now(), time.Time{}))
}

func TestNarrate_IncludesRelativeTimes(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)

	changes := []Change{
		{Kind: IncidentNew, IncidentID: "INC-1", Summary: "New incident: One (svc)", At: now.Add(-3 * time.Minute)},
		{Kind: StatusChanged, IncidentID: "INC-2", Summary: "Status changed: a → b", At: now.Add(-2 * time.Hour)},
	}

	result := Narrate(changes, now)

	assert.Contains(t, result, "3m ago",
		"Narrate's now parameter must produce relative times, not be dead")
	assert.Contains(t, result, "2h ago")
	assert.Contains(t, result, "INC-1")
	assert.Contains(t, result, "Status changed: a → b")
}

func TestNarrate_OmitsTimeForZeroValuedAt(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)

	changes := []Change{
		{Kind: IncidentNew, IncidentID: "INC-1", Summary: "New incident"},
	}

	result := Narrate(changes, now)

	assert.Contains(t, result, "INC-1")
	assert.NotContains(t, result, "ago",
		"an unstamped change must not claim an age")
}
