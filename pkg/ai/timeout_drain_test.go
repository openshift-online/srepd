package ai

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAnthropicQuery_AppliesPackageTimeout verifies the anthropic provider
// honours the same timeout contract as ollama and openai: a caller context
// with no deadline gets one applied. Without ensureTimeout a hung API call
// would block the tea.Cmd goroutine forever.
func TestAnthropicQuery_AppliesPackageTimeout(t *testing.T) {
	p, err := newAnthropicProvider(Config{Model: "test-model"}, "test-key")
	require.NoError(t, err)

	assert.Equal(t, defaultRequestTimeout, p.requestTimeout,
		"the provider must carry the package request timeout")

	// A deadline-less context must gain one. Point the provider at an
	// unroutable endpoint with a very short timeout so the call returns
	// promptly rather than hanging.
	p.requestTimeout = 20 * time.Millisecond

	start := time.Now()
	_, err = p.Query(context.Background(), "sys", "user")
	elapsed := time.Since(start)

	require.Error(t, err, "an unreachable endpoint must return an error, not hang")
	assert.Less(t, elapsed, 10*time.Second,
		"Query must bound a deadline-less context (took %s)", elapsed)
}

func TestAnthropicQuery_RespectsCallerDeadline(t *testing.T) {
	p, err := newAnthropicProvider(Config{Model: "test-model"}, "test-key")
	require.NoError(t, err)
	p.requestTimeout = time.Hour

	// A caller deadline must win over the provider's own timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err = p.Query(ctx, "sys", "user")
	elapsed := time.Since(start)

	require.Error(t, err)
	assert.Less(t, elapsed, 10*time.Second,
		"the caller's deadline must be honoured (took %s)", elapsed)
}

// streamSendCase drives one provider's StreamQuery. newProvider builds a
// provider pointed at the supplied base URL (an httptest.Server), and query
// invokes that provider's StreamQuery.
type streamSendCase struct {
	name string
	// handler streams several chunks in this provider's own wire format,
	// flushing each so the client's scanner loop sees them individually.
	handler func(w http.ResponseWriter, r *http.Request)
	// query builds the provider against baseURL and runs StreamQuery.
	query func(t *testing.T, baseURL string, ctx context.Context, ch chan<- string) error
}

// streamChunkCount is the number of text chunks each fake server emits. It must
// exceed the one token the test consumes so a later send blocks on the unread
// channel — the state the ctx.Done() guard exists to escape.
const streamChunkCount = 8

func streamSendCases() []streamSendCase {
	return []streamSendCase{
		{
			name: "anthropic",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				flusher, ok := w.(http.Flusher)
				if !ok {
					return
				}
				_, _ = fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"m\",\"content\":[],\"stop_reason\":null,\"stop_sequence\":null,\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n")
				flusher.Flush()
				_, _ = fmt.Fprint(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
				flusher.Flush()
				for i := 0; i < streamChunkCount; i++ {
					frame := fmt.Sprintf("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"tok%d\"}}\n\n", i)
					if _, err := fmt.Fprint(w, frame); err != nil {
						return
					}
					flusher.Flush()
				}
			},
			query: func(t *testing.T, baseURL string, ctx context.Context, ch chan<- string) error {
				t.Helper()
				p, err := newAnthropicProvider(Config{Model: "m", Endpoint: baseURL}, "k")
				require.NoError(t, err)
				return p.StreamQuery(ctx, "sys", "user", ch)
			},
		},
		{
			name: "ollama",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/x-ndjson")
				w.WriteHeader(http.StatusOK)
				for i := 0; i < streamChunkCount; i++ {
					frame := fmt.Sprintf("{\"message\":{\"role\":\"assistant\",\"content\":\"tok%d\"},\"done\":false}\n", i)
					if _, err := fmt.Fprint(w, frame); err != nil {
						return
					}
					if flusher, ok := w.(http.Flusher); ok {
						flusher.Flush()
					}
				}
			},
			query: func(t *testing.T, baseURL string, ctx context.Context, ch chan<- string) error {
				t.Helper()
				p, err := newOllamaProvider(Config{Model: "m", Endpoint: baseURL})
				require.NoError(t, err)
				return p.StreamQuery(ctx, "sys", "user", ch)
			},
		},
		{
			name: "openai",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				for i := 0; i < streamChunkCount; i++ {
					frame := fmt.Sprintf("data: {\"choices\":[{\"delta\":{\"content\":\"tok%d\"}}]}\n\n", i)
					if _, err := fmt.Fprint(w, frame); err != nil {
						return
					}
					if flusher, ok := w.(http.Flusher); ok {
						flusher.Flush()
					}
				}
			},
			query: func(t *testing.T, baseURL string, ctx context.Context, ch chan<- string) error {
				t.Helper()
				p, err := newOpenAICompatProvider(Config{Model: "m", Endpoint: baseURL}, "k")
				require.NoError(t, err)
				return p.StreamQuery(ctx, "sys", "user", ch)
			},
		},
	}
}

// TestStreamQuery_CancellationUnblocksSend covers the drain contract at the
// site it actually lives: a StreamQuery that has reached its scanner loop and
// is blocked sending a token to a consumer that stopped reading. Cancelling
// the context must unblock that send and return, rather than leaking the
// provider goroutine for the life of the process.
//
// The earlier version of this test cancelled the context before calling
// StreamQuery, so httpClient.Do failed immediately and the send site was never
// reached — deleting all three ctx.Done() guards left it passing. This one
// serves live chunks from an httptest.Server, reads exactly one token, and
// only then cancels, so the guard is genuinely exercised.
func TestStreamQuery_CancellationUnblocksSend(t *testing.T) {
	for _, tc := range streamSendCases() {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(tc.handler))
			defer srv.Close()

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			// Unbuffered: after the single read below, the next send blocks
			// until the guard observes cancellation.
			ch := make(chan string)

			errCh := make(chan error, 1)
			go func() {
				errCh <- tc.query(t, srv.URL, ctx, ch)
			}()

			// Sequencing is by channel, not by sleeping: receiving a token
			// proves the provider reached the send site and is now streaming.
			select {
			case tok, ok := <-ch:
				require.True(t, ok, "provider closed ch before sending any token")
				require.NotEmpty(t, tok, "provider must stream a non-empty token")
			case err := <-errCh:
				t.Fatalf("StreamQuery returned before streaming any token: %v", err)
			case <-time.After(10 * time.Second):
				t.Fatal("provider never streamed a token")
			}

			// The consumer is now gone. The provider is blocked on its next
			// send; only the ctx.Done() guard can free it.
			cancel()

			select {
			case err := <-errCh:
				require.Error(t, err, "a cancelled StreamQuery must report an error")
				assert.True(t, errors.Is(err, context.Canceled),
					"StreamQuery must return a context error, got %v", err)
			case <-time.After(10 * time.Second):
				t.Fatal("StreamQuery blocked after context cancellation — the send site must select on ctx.Done()")
			}
		})
	}
}

// TestStreamQuery_PreCancelledContextFailsFast keeps the coverage the original
// drain test actually provided: a context cancelled before the call must abort
// at the HTTP request rather than hanging. This is a weaker property than the
// send-site guard above (it passes with the guards removed) and is retained
// only so the fail-fast path stays asserted.
func TestStreamQuery_PreCancelledContextFailsFast(t *testing.T) {
	anthropicP, err := newAnthropicProvider(Config{Model: "m"}, "k")
	require.NoError(t, err)

	ollamaP, err := newOllamaProvider(Config{Model: "m", Endpoint: "http://127.0.0.1:1"})
	require.NoError(t, err)

	openaiP, err := newOpenAICompatProvider(Config{Model: "m", Endpoint: "http://127.0.0.1:1"}, "k")
	require.NoError(t, err)

	cases := []struct {
		name  string
		query func(ctx context.Context, ch chan<- string) error
	}{
		{"anthropic", func(ctx context.Context, ch chan<- string) error {
			return anthropicP.StreamQuery(ctx, "sys", "user", ch)
		}},
		{"ollama", func(ctx context.Context, ch chan<- string) error {
			return ollamaP.StreamQuery(ctx, "sys", "user", ch)
		}},
		{"openai", func(ctx context.Context, ch chan<- string) error {
			return openaiP.StreamQuery(ctx, "sys", "user", ch)
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel() // consumer is already gone
			defer cancel()

			// Unbuffered and never read: any unguarded send blocks forever.
			ch := make(chan string)

			done := make(chan struct{})
			go func() {
				defer close(done)
				_ = tc.query(ctx, ch)
			}()

			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("StreamQuery must abort promptly on an already-cancelled context")
			}
		})
	}
}

// TestStreamQuery_ClosesChannelOnCancellation ensures the drain contract still
// closes ch so the consumer's range loop terminates.
func TestStreamQuery_ClosesChannelOnCancellation(t *testing.T) {
	p, err := newOllamaProvider(Config{Model: "m", Endpoint: "http://127.0.0.1:1"})
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	ch := make(chan string, 4)
	_ = p.StreamQuery(ctx, "sys", "user", ch)

	// Draining must terminate: StreamQuery closes ch on every return path.
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		for range ch { //nolint:revive // draining to observe close
		}
	}()

	select {
	case <-drained:
	case <-time.After(5 * time.Second):
		t.Fatal("StreamQuery must close ch on every return path")
	}
}
