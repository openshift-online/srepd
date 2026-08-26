package agent

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSummarizeToolInput_CommandIsRuneSafe covers the byte-slicing defect: a
// command longer than the cap was cut with s[:80], which splits a multi-byte
// rune and emits U+FFFD into the TUI.
func TestSummarizeToolInput_CommandIsRuneSafe(t *testing.T) {
	tests := []struct {
		name string
		cmd  string
	}{
		{"CJK", strings.Repeat("日本語テキスト", 40)},
		{"emoji", strings.Repeat("🚀🔥", 100)},
		{"combining marks", strings.Repeat("éàü", 100)},
		{"mixed", strings.Repeat("oc get pods 日本 🚀 ", 20)},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			input, err := json.Marshal(map[string]string{"command": tc.cmd})
			require.NoError(t, err)

			got := summarizeToolInput(input)

			assert.True(t, utf8.ValidString(got),
				"summary must stay valid UTF-8, got %q", got)
			assert.NotContains(t, got, "�",
				"summary must not contain the replacement character")

			trimmed := strings.TrimSuffix(got, "...")
			assert.LessOrEqual(t, utf8.RuneCountInString(trimmed), 80,
				"summary must be capped at 80 runes")
		})
	}
}

func TestSummarizeToolInput_ShortCommandUntouched(t *testing.T) {
	input, err := json.Marshal(map[string]string{"command": "oc get pods 日本語"})
	require.NoError(t, err)

	got := summarizeToolInput(input)
	assert.Equal(t, "oc get pods 日本語", got,
		"a command under the cap must be returned verbatim")
}

func TestSummarizeToolInput_RawFallbackIsRuneSafe(t *testing.T) {
	// No description/command/file_path key: the raw JSON is truncated instead.
	input, err := json.Marshal(map[string]string{
		"unknown_key": strings.Repeat("日本語テキスト🚀", 40),
	})
	require.NoError(t, err)

	got := summarizeToolInput(input)

	assert.True(t, utf8.ValidString(got), "raw fallback must stay valid UTF-8")
	assert.NotContains(t, got, "�")

	trimmed := strings.TrimSuffix(got, "...")
	assert.LessOrEqual(t, utf8.RuneCountInString(trimmed), 100,
		"raw fallback must be capped at 100 runes")
}
