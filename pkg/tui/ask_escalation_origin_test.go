package tui

import (
	"os"
	"strings"
	"testing"

	"github.com/PagerDuty/go-pagerduty"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openshift-online/srepd/pkg/ai/tools"
	"github.com/openshift-online/srepd/pkg/pd"
)

// escalationVerdict is the verdict shape that inferAskKind maps to
// AskEscalationSuggestion.
func escalationVerdict() tools.Verdict {
	return tools.Verdict{
		Tier:    tools.TierActionable,
		Summary: "Re-escalate",
		Action:  "Re-escalate this incident",
	}
}

// TestBuildAskFromVerdict_Escalation_ActsOnOriginNotSelection mirrors the
// browse-away pattern from TestBuildAskFromVerdict_DraftNote_TargetsOriginalIncident.
// The escalation action must resolve the incident by the ask's origin ID at
// accept time, never by reading m.selectedIncident.
func TestBuildAskFromVerdict_Escalation_ActsOnOriginNotSelection(t *testing.T) {
	m := createTestModel()
	m.config = &pd.Config{Client: &pd.MockPagerDutyClient{}}

	origin := pagerduty.Incident{
		APIObject: pagerduty.APIObject{ID: "INC-ORIGIN"},
		Title:     "Origin incident",
		Urgency:   "high",
	}
	other := pagerduty.Incident{
		APIObject: pagerduty.APIObject{ID: "INC-OTHER"},
		Title:     "Some other incident",
		Urgency:   "low",
	}
	m.incidentList = []pagerduty.Incident{origin, other}

	// The watcher fired on the origin while the user was looking at "other".
	m.selectedIncident = &other

	ask := m.buildAskFromVerdict(escalationVerdict(), []string{"INC-ORIGIN"})
	require.Equal(t, AskEscalationSuggestion, ask.Kind)
	assert.Equal(t, "INC-ORIGIN", ask.IncidentID)

	// User browses somewhere else again before accepting.
	m.selectedIncident = &pagerduty.Incident{
		APIObject: pagerduty.APIObject{ID: "INC-THIRD"},
		Title:     "Third incident",
	}

	cmd := ask.Action()
	require.NotNil(t, cmd)
	msg := cmd()

	reesc, ok := msg.(unAcknowledgeIncidentsMsg)
	require.True(t, ok, "expected unAcknowledgeIncidentsMsg, got %T", msg)
	require.Len(t, reesc.incidents, 1)
	assert.Equal(t, "INC-ORIGIN", reesc.incidents[0].ID,
		"escalation must act on the originating incident, not the UI selection")
	assert.Equal(t, "Origin incident", reesc.incidents[0].Title,
		"the dispatched incident must be fully populated, not a zero value")
}

// TestBuildAskFromVerdict_Escalation_NothingSelectedNoZeroDispatch is the
// defect the plan calls out: with nothing selected the incidentID guard passes
// (the origin ID is non-empty) but the snapshotted incident is a zero value,
// so a pagerduty.Incident{} with an empty ID was dispatched to
// unAcknowledgeIncidentsMsg.
func TestBuildAskFromVerdict_Escalation_NothingSelectedNoZeroDispatch(t *testing.T) {
	m := createTestModel()
	m.config = &pd.Config{Client: &pd.MockPagerDutyClient{}}

	origin := pagerduty.Incident{
		APIObject: pagerduty.APIObject{ID: "INC-ORIGIN"},
		Title:     "Origin incident",
	}
	m.incidentList = []pagerduty.Incident{origin}
	m.selectedIncident = nil

	ask := m.buildAskFromVerdict(escalationVerdict(), []string{"INC-ORIGIN"})
	require.Equal(t, AskEscalationSuggestion, ask.Kind)
	assert.Equal(t, "INC-ORIGIN", ask.IncidentID)

	cmd := ask.Action()
	require.NotNil(t, cmd)
	msg := cmd()

	reesc, ok := msg.(unAcknowledgeIncidentsMsg)
	require.True(t, ok, "expected unAcknowledgeIncidentsMsg, got %T", msg)
	require.Len(t, reesc.incidents, 1)
	assert.NotEmpty(t, reesc.incidents[0].ID,
		"a zero-value pagerduty.Incident must never be dispatched")
	assert.Equal(t, "INC-ORIGIN", reesc.incidents[0].ID)
}

// TestBuildAskFromVerdict_Escalation_UnresolvableIDErrorsCleanly ensures an
// origin that has left the queue and cannot be fetched produces a status
// message rather than a panic or a zero-value dispatch.
func TestBuildAskFromVerdict_Escalation_UnresolvableIDErrorsCleanly(t *testing.T) {
	m := createTestModel()
	m.config = &pd.Config{Client: &pd.MockPagerDutyClient{}}

	// "err" is the mock's convention for a failing lookup, and it is absent
	// from the incident list.
	m.incidentList = []pagerduty.Incident{
		{APIObject: pagerduty.APIObject{ID: "INC-UNRELATED"}, Title: "Unrelated"},
	}
	m.selectedIncident = nil

	ask := m.buildAskFromVerdict(escalationVerdict(), []string{"err"})
	require.Equal(t, AskEscalationSuggestion, ask.Kind)
	assert.Equal(t, "err", ask.IncidentID)

	cmd := ask.Action()
	require.NotNil(t, cmd)

	var msg tea.Msg
	assert.NotPanics(t, func() { msg = cmd() })

	status, ok := msg.(setStatusMsg)
	require.True(t, ok, "unresolvable origin must produce setStatusMsg, got %T", msg)
	assert.Contains(t, status.string, "no longer in queue")
}

// TestBuildAskFromVerdict_Escalation_FetchesOriginNotInList covers the case
// where the origin has aged out of m.incidentList but is still fetchable from
// PagerDuty.
func TestBuildAskFromVerdict_Escalation_FetchesOriginNotInList(t *testing.T) {
	m := createTestModel()
	m.config = &pd.Config{Client: &pd.MockPagerDutyClient{}}
	m.incidentList = nil
	m.selectedIncident = nil

	ask := m.buildAskFromVerdict(escalationVerdict(), []string{"INC-AGED-OUT"})
	assert.Equal(t, "INC-AGED-OUT", ask.IncidentID)

	cmd := ask.Action()
	require.NotNil(t, cmd)
	msg := cmd()

	reesc, ok := msg.(unAcknowledgeIncidentsMsg)
	require.True(t, ok, "expected unAcknowledgeIncidentsMsg, got %T", msg)
	require.Len(t, reesc.incidents, 1)
	assert.Equal(t, "INC-AGED-OUT", reesc.incidents[0].ID,
		"an origin missing from the list must be fetched by ID")
}

// TestAskActionsDoNotReadSelectedIncident is a structural guard: no ask Action
// closure inside buildAskFromVerdict may read m.selectedIncident. The identity
// is fixed above the kind switch (at creation time) and every action must use
// ask.IncidentID from there. This scans the source region rather than the
// behaviour so a future edit that reintroduces the defect fails here even if
// no behavioural test happens to cover that kind.
func TestAskActionsDoNotReadSelectedIncident(t *testing.T) {
	src, err := os.ReadFile("model.go")
	require.NoError(t, err)

	text := string(src)

	start := strings.Index(text, "func (m *model) buildAskFromVerdict(")
	require.GreaterOrEqual(t, start, 0, "buildAskFromVerdict not found in model.go")

	// The action closures live between the `switch kind {` dispatch and the
	// function's closing `return ask`.
	bodyStart := strings.Index(text[start:], "switch kind {")
	require.GreaterOrEqual(t, bodyStart, 0, "kind switch not found in buildAskFromVerdict")
	bodyEnd := strings.Index(text[start:], "\n\treturn ask\n}")
	require.Greater(t, bodyEnd, bodyStart, "end of buildAskFromVerdict not found")

	actionRegion := text[start+bodyStart : start+bodyEnd]

	assert.NotContains(t, actionRegion, "m.selectedIncident",
		"ask Action closures must never read m.selectedIncident — resolve by "+
			"ask.IncidentID instead (see plan 422 / plan 417 identity lesson)")
}
