package tui

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/PagerDuty/go-pagerduty"
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openshift-online/srepd/pkg/ai/tools"
	"github.com/openshift-online/srepd/pkg/delta"
	"github.com/openshift-online/srepd/pkg/pd"
)

// testNow is the fixed reference clock used where delta.Diff needs a
// deterministic observation time.
var testNow = time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)

// fakeBetaProvider is an Anthropic-family provider stub that satisfies
// extractToolRunnerFactory so initToolRegistryForModel builds a real
// registry. The BetaMessageService is never invoked by these tests.
type fakeBetaProvider struct{}

func (f *fakeBetaProvider) Name() string  { return "anthropic" }
func (f *fakeBetaProvider) Model() string { return "test-model" }
func (f *fakeBetaProvider) Query(_ context.Context, _ string, _ string) (string, error) {
	return "", nil
}
func (f *fakeBetaProvider) StreamQuery(_ context.Context, _ string, _ string, ch chan<- string) error {
	close(ch)
	return nil
}
func (f *fakeBetaProvider) BetaMessages() *anthropic.BetaMessageService {
	return &anthropic.BetaMessageService{}
}

// findRegisteredTool returns the named tool from the registry.
func findRegisteredTool(t *testing.T, reg *tools.Registry, name string) tools.Tool {
	t.Helper()
	require.NotNil(t, reg, "tool registry must be initialized")
	for _, tool := range reg.Tools() {
		if tool.Name == name {
			return tool
		}
	}
	t.Fatalf("tool %q not registered; have %d tools", name, len(reg.Tools()))
	return tools.Tool{}
}

// TestGetRecentEventsTool_SeesDeltasFromUpdateLoop is the wiring test that was
// missing: it drives a real poll through Update (which has a value receiver)
// and then invokes the registered get_recent_events handler exactly as the
// registry exposes it. Before the delta.Log fix the tool closure captured a
// model struct whose recentChanges slice was never mutated by Update's copies,
// so the handler always returned "[]".
func TestGetRecentEventsTool_SeesDeltasFromUpdateLoop(t *testing.T) {
	m := createTestModel()
	m.config = &pd.Config{
		Client:      &pd.MockPagerDutyClient{},
		CurrentUser: &pagerduty.User{APIObject: pagerduty.APIObject{ID: "U-TEST"}},
	}
	m.aiProvider = &fakeBetaProvider{}
	initToolRegistryForModel(&m)

	tool := findRegisteredTool(t, m.toolRegistry, "get_recent_events")

	// A poll delivering two brand-new incidents must produce IncidentNew
	// changes that the tool can observe.
	incidents := []pagerduty.Incident{
		{APIObject: pagerduty.APIObject{ID: "INC-DELTA-1"}, Title: "First", Status: "triggered", Urgency: "high"},
		{APIObject: pagerduty.APIObject{ID: "INC-DELTA-2"}, Title: "Second", Status: "triggered", Urgency: "low"},
	}

	updated, _ := m.Update(updatedIncidentListMsg{incidents: incidents})
	next, ok := updated.(model)
	require.True(t, ok, "Update must return a model, got %T", updated)

	// The registry captured at InitialModel time must still see the changes
	// recorded by Update's copy of the model.
	result, err := tool.Handler(context.Background(), nil)
	require.NoError(t, err)

	assert.NotEqual(t, "[]", result,
		"get_recent_events must observe deltas recorded by the Update loop")
	assert.Contains(t, result, "INC-DELTA-1")
	assert.Contains(t, result, "INC-DELTA-2")

	// The successor model shares the same log — the tool registry is not
	// re-initialized per Update.
	assert.Equal(t, m.changeLog, next.changeLog,
		"the change log pointer must survive Update's value-receiver copy")
}

// TestGetRecentEventsTool_ReadsLiveLog verifies the handler reads through to
// the shared log rather than a frozen snapshot taken at registration time.
func TestGetRecentEventsTool_ReadsLiveLog(t *testing.T) {
	m := createTestModel()
	m.config = &pd.Config{
		Client:      &pd.MockPagerDutyClient{},
		CurrentUser: &pagerduty.User{APIObject: pagerduty.APIObject{ID: "U-TEST"}},
	}
	m.aiProvider = &fakeBetaProvider{}
	initToolRegistryForModel(&m)

	tool := findRegisteredTool(t, m.toolRegistry, "get_recent_events")

	result, err := tool.Handler(context.Background(), nil)
	require.NoError(t, err)
	assert.Equal(t, "[]", result, "empty log must render as an empty JSON array")

	m.changeLog.Append(delta.Change{
		Kind:       delta.StatusChanged,
		IncidentID: "INC-LIVE",
		Summary:    "Status changed: triggered → acknowledged",
	})

	result, err = tool.Handler(context.Background(), nil)
	require.NoError(t, err)
	assert.Contains(t, result, "INC-LIVE",
		"handler must read the live log, not a snapshot from registration time")
}

// TestGetRecentEventsTool_ConcurrentWithAppend exercises the real hazard: the
// tool handler runs on an investigation tea.Cmd goroutine while the Update
// loop appends new deltas. Run under -race this fails if the log is not
// mutex-guarded.
func TestGetRecentEventsTool_ConcurrentWithAppend(t *testing.T) {
	m := createTestModel()
	m.config = &pd.Config{
		Client:      &pd.MockPagerDutyClient{},
		CurrentUser: &pagerduty.User{APIObject: pagerduty.APIObject{ID: "U-TEST"}},
	}
	m.aiProvider = &fakeBetaProvider{}
	initToolRegistryForModel(&m)

	tool := findRegisteredTool(t, m.toolRegistry, "get_recent_events")

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			m.changeLog.Append(delta.Change{
				Kind:       delta.IncidentNew,
				IncidentID: fmt.Sprintf("INC-%d", i),
				Summary:    "concurrent append",
			})
		}
	}()

	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			if _, err := tool.Handler(context.Background(), nil); err != nil {
				t.Errorf("handler returned error: %v", err)
				return
			}
		}
	}()

	wg.Wait()
}
