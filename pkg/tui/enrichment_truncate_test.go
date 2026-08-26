package tui

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
)

// TestRenderEnrichmentError_IsRuneSafe covers the byte-slicing defect: a long
// error summary was cut with summary[:120], splitting multi-byte runes and
// rendering U+FFFD in the TUI.
func TestRenderEnrichmentError_IsRuneSafe(t *testing.T) {
	tests := []struct {
		name string
		text string
	}{
		{"CJK", strings.Repeat("日本語のエラー", 40)},
		{"emoji", strings.Repeat("🚀🔥", 100)},
		{"combining marks", strings.Repeat("éàü", 100)},
		{"mixed", strings.Repeat("failed 日本 🚀 ", 30)},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := createTestModel()
			errs := map[string]error{"one": errors.New(tc.text)}

			got := m.renderEnrichmentError("alerts", errs)

			assert.True(t, utf8.ValidString(got),
				"rendered error must stay valid UTF-8")
			assert.NotContains(t, got, "�",
				"rendered error must not contain the replacement character")
		})
	}
}

func TestRenderEnrichmentError_ShortSummaryUntouched(t *testing.T) {
	m := createTestModel()
	errs := map[string]error{"one": errors.New("boom 日本語")}

	got := m.renderEnrichmentError("alerts", errs)

	assert.Contains(t, got, "boom 日本語")
	assert.NotContains(t, got, "...", "a short summary must not be truncated")
}

func TestRenderEnrichmentError_TruncatesAtRuneCap(t *testing.T) {
	m := createTestModel()
	long := strings.Repeat("日", 300)
	errs := map[string]error{"one": errors.New(long)}

	got := m.renderEnrichmentError("alerts", errs)

	assert.Contains(t, got, "...", "an over-long summary must be truncated")
	assert.LessOrEqual(t, strings.Count(got, "日"), maxEnrichmentErrorRunes,
		"the summary must be capped at the rune limit")
}
