package agent

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTailBuffer_WritesUnderCap(t *testing.T) {
	b := newTailBuffer(64)

	n, err := b.Write([]byte("hello "))
	require.NoError(t, err)
	assert.Equal(t, 6, n, "Write must report the full input length")

	n, err = b.Write([]byte("world"))
	require.NoError(t, err)
	assert.Equal(t, 5, n)

	assert.Equal(t, "hello world", b.String())
	assert.Equal(t, 11, b.Len())
}

func TestTailBuffer_ExactlyAtCap(t *testing.T) {
	b := newTailBuffer(5)

	n, err := b.Write([]byte("abcde"))
	require.NoError(t, err)
	assert.Equal(t, 5, n)
	assert.Equal(t, "abcde", b.String())
	assert.Equal(t, 5, b.Len())
}

func TestTailBuffer_KeepsTailWhenOverCap(t *testing.T) {
	b := newTailBuffer(5)

	_, err := b.Write([]byte("abcdefgh"))
	require.NoError(t, err)

	assert.Equal(t, "defgh", b.String(),
		"the buffer keeps the most recent bytes, not the oldest")
	assert.Equal(t, 5, b.Len())
}

func TestTailBuffer_SingleWriteLargerThanCap(t *testing.T) {
	b := newTailBuffer(8)

	huge := strings.Repeat("x", 1000) + "TAIL8888"
	n, err := b.Write([]byte(huge))
	require.NoError(t, err)
	assert.Equal(t, len(huge), n, "Write must report the full input length even when trimmed")

	assert.Equal(t, "TAIL8888", b.String())
	assert.Equal(t, 8, b.Len())
}

func TestTailBuffer_MultiWriteWraparound(t *testing.T) {
	b := newTailBuffer(6)

	for _, chunk := range []string{"aa", "bb", "cc", "dd", "ee"} {
		_, err := b.Write([]byte(chunk))
		require.NoError(t, err)
	}

	assert.Equal(t, "ccddee", b.String(),
		"successive writes evict the oldest bytes")
	assert.Equal(t, 6, b.Len())
}

func TestTailBuffer_DetectsMarkerNearTheEnd(t *testing.T) {
	// The real use: the "already in use" spawn-retry marker must survive a
	// flood of preceding stderr output.
	b := newTailBuffer(defaultStderrTailBytes)

	_, err := b.Write([]byte(strings.Repeat("noise\n", 100000)))
	require.NoError(t, err)
	_, err = b.Write([]byte("Session ID already in use\n"))
	require.NoError(t, err)

	assert.Contains(t, b.String(), sessionInUseMarker)
	assert.LessOrEqual(t, b.Len(), defaultStderrTailBytes,
		"the buffer must stay bounded under a stderr flood")
}

func TestTailBuffer_EmptyWrite(t *testing.T) {
	b := newTailBuffer(4)
	n, err := b.Write(nil)
	require.NoError(t, err)
	assert.Equal(t, 0, n)
	assert.Equal(t, "", b.String())
}

func TestTailBuffer_NonPositiveCapFallsBack(t *testing.T) {
	for _, cap := range []int{0, -1} {
		b := newTailBuffer(cap)
		_, err := b.Write([]byte("data"))
		require.NoError(t, err)
		assert.Equal(t, "data", b.String())
		assert.Equal(t, defaultStderrTailBytes, b.Cap(),
			"a non-positive cap falls back to the default")
	}
}

func TestTailBuffer_ConcurrentWrites(t *testing.T) {
	b := newTailBuffer(1024)

	done := make(chan struct{})
	for i := 0; i < 4; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			for j := 0; j < 200; j++ {
				_, _ = b.Write([]byte("chunk"))
			}
		}()
	}
	go func() {
		defer func() { done <- struct{}{} }()
		for j := 0; j < 200; j++ {
			_ = b.String()
		}
	}()

	for i := 0; i < 5; i++ {
		<-done
	}

	assert.LessOrEqual(t, b.Len(), 1024)
}
