package tui

import (
	"fmt"
	"sync"
	"testing"

	"github.com/PagerDuty/go-pagerduty"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openshift-online/srepd/pkg/ai/tools"
	"github.com/openshift-online/srepd/pkg/pd"
)

// askViaUpdateLoop builds an ask the way production does: through
// Update(investigationMsg{...}). This matters enormously for these tests.
//
// buildAskFromVerdict has a POINTER receiver but its only production call site
// (tui.go, the investigationMsg TierActionable branch) is inside
// `func (m model) Update` — a VALUE receiver. So `m.buildAskFromVerdict(...)`
// implicitly takes &m of Update's *local copy*, a stack value that dies the
// moment Update returns. Calling buildAskFromVerdict directly from a test on a
// test-local `m` variable does NOT reproduce this: the test then reassigns that
// same variable and the captured pointer appears to see live state.
//
// It returns the ask and the model Update produced.
func askViaUpdateLoop(t *testing.T, m model, verdict tools.Verdict, incidentIDs []string) (Ask, model) {
	t.Helper()

	m.approvals = newApprovalsStrip()
	m.help = newHelp()

	result, _ := m.Update(investigationMsg{
		observation: "watcher observation",
		verdict:     verdict,
		incidentIDs: incidentIDs,
	})
	updated, ok := result.(model)
	require.True(t, ok, "Update must return a model")
	require.Equal(t, 1, updated.approvals.Count(),
		"an actionable verdict with a non-empty Action must add exactly one ask")

	return updated.approvals.asks[0], updated
}

// TestBuildAskFromVerdict_Escalation_ObservesLaterIncidentListUpdate proves the
// stale-read defect.
//
// The Action closure returned through the Update loop dereferences a dead stack
// copy of the model, so a subsequent Update replacing m.incidentList is
// invisible to it.
//
// Real-world impact: the closure resolves the incident from a frozen snapshot
// and dispatches it to unAcknowledgeIncidentsMsg, which reads
// incident.EscalationPolicy.ID (tui.go:1660). A stale snapshot escalates to the
// OLD policy, or — if the stale copy had no policy — the incident is silently
// dropped via a log.Warn branch with no user-visible failure.
func TestBuildAskFromVerdict_Escalation_ObservesLaterIncidentListUpdate(t *testing.T) {
	m := createTestModel()
	m.config = &pd.Config{Client: &pd.MockPagerDutyClient{}}

	// The origin as the watcher first saw it: policy POLICY-OLD.
	m.incidentList = []pagerduty.Incident{{
		APIObject:        pagerduty.APIObject{ID: "INC-ORIGIN"},
		Title:            "Origin incident",
		EscalationPolicy: pagerduty.APIObject{ID: "POLICY-OLD"},
	}}
	m.selectedIncident = nil

	ask, m := askViaUpdateLoop(t, m, escalationVerdict(), []string{"INC-ORIGIN"})
	require.Equal(t, AskEscalationSuggestion, ask.Kind)
	require.Equal(t, "INC-ORIGIN", ask.IncidentID)

	// A LATER poll moves the incident onto a new escalation policy.
	result, _ := m.Update(updatedIncidentListMsg{incidents: []pagerduty.Incident{{
		APIObject:        pagerduty.APIObject{ID: "INC-ORIGIN"},
		Title:            "Origin incident",
		EscalationPolicy: pagerduty.APIObject{ID: "POLICY-NEW"},
	}}})
	m, ok := result.(model)
	require.True(t, ok)
	require.Equal(t, "POLICY-NEW", m.incidentList[0].EscalationPolicy.ID,
		"sanity: the live model observed the new policy")

	// The user now accepts the ask.
	cmd := ask.Action()
	require.NotNil(t, cmd)
	msg := cmd()

	reesc, ok := msg.(unAcknowledgeIncidentsMsg)
	require.True(t, ok, "expected unAcknowledgeIncidentsMsg, got %T", msg)
	require.Len(t, reesc.incidents, 1)
	assert.Equal(t, "INC-ORIGIN", reesc.incidents[0].ID,
		"target identity must always be ask.IncidentID")
	assert.Equal(t, "POLICY-NEW", reesc.incidents[0].EscalationPolicy.ID,
		"the accept-time resolution must observe live incident state, not a "+
			"frozen copy of Update's local model")
}

// TestBuildAskFromVerdict_Escalation_ObservesIncidentRemovedFromList is the
// second half of the same defect: an origin that has LEFT the queue since the
// ask was created must fall through to the PagerDuty fetch, not be resolved out
// of a dead snapshot that still contains it.
func TestBuildAskFromVerdict_Escalation_ObservesIncidentRemovedFromList(t *testing.T) {
	m := createTestModel()
	m.config = &pd.Config{Client: &pd.MockPagerDutyClient{}}

	m.incidentList = []pagerduty.Incident{{
		APIObject:        pagerduty.APIObject{ID: "INC-ORIGIN"},
		Title:            "Stale title from the frozen snapshot",
		EscalationPolicy: pagerduty.APIObject{ID: "POLICY-OLD"},
	}}
	m.selectedIncident = nil

	ask, m := askViaUpdateLoop(t, m, escalationVerdict(), []string{"INC-ORIGIN"})
	require.Equal(t, "INC-ORIGIN", ask.IncidentID)

	// A later poll removes the origin from the queue entirely.
	result, _ := m.Update(updatedIncidentListMsg{incidents: []pagerduty.Incident{
		{APIObject: pagerduty.APIObject{ID: "INC-UNRELATED"}, Title: "Unrelated"},
	}})
	m, ok := result.(model)
	require.True(t, ok)
	require.Nil(t, findIncidentByID(m.incidentList, "INC-ORIGIN"),
		"sanity: the live list no longer holds the origin")

	cmd := ask.Action()
	require.NotNil(t, cmd)
	msg := cmd()

	reesc, ok := msg.(unAcknowledgeIncidentsMsg)
	require.True(t, ok, "expected unAcknowledgeIncidentsMsg, got %T", msg)
	require.Len(t, reesc.incidents, 1)
	assert.Equal(t, "INC-ORIGIN", reesc.incidents[0].ID,
		"target identity must always be ask.IncidentID")
	assert.NotEqual(t, "Stale title from the frozen snapshot", reesc.incidents[0].Title,
		"an origin absent from the LIVE list must be fetched from PagerDuty, "+
			"not resolved out of a frozen snapshot")
}

// noteVerdictForLiveStateTest is a verdict shape that inferAskKind maps to
// AskDraftNote (no escalation/command keywords).
func noteVerdictForLiveStateTest() tools.Verdict {
	return tools.Verdict{
		Tier:    tools.TierActionable,
		Summary: "Summarize findings",
		Action:  "Record the cluster health findings in a note",
	}
}

// TestBuildAskFromVerdict_DraftNote_ResolvesAgainstLiveModel covers the note
// path, which captures the same dead *model via m.postAINoteToIncidentCmd.
func TestBuildAskFromVerdict_DraftNote_ResolvesAgainstLiveModel(t *testing.T) {
	m := createTestModel()
	m.config = &pd.Config{Client: &pd.MockPagerDutyClient{}}
	m.incidentList = []pagerduty.Incident{
		{APIObject: pagerduty.APIObject{ID: "INC-NOTE"}, Title: "Note target"},
	}

	ask, m := askViaUpdateLoop(t, m, noteVerdictForLiveStateTest(), []string{"INC-NOTE"})
	require.Equal(t, AskDraftNote, ask.Kind)
	require.Equal(t, "INC-NOTE", ask.IncidentID)

	// A later Update replaces the incident list.
	result, _ := m.Update(updatedIncidentListMsg{incidents: nil})
	_, ok := result.(model)
	require.True(t, ok)

	cmd := ask.Action()
	require.NotNil(t, cmd)
	msg := cmd()

	note, ok := msg.(addedIncidentNoteMsg)
	require.True(t, ok, "expected addedIncidentNoteMsg, got %T", msg)
	assert.Equal(t, "INC-NOTE", note.incidentID,
		"the note must target ask.IncidentID")
}

// TestAskAction_RaceWithUpdateLoop drives an accepted ask's Action on one
// goroutine while the Update loop mutates model state on another. Under -race
// this fails if the closure holds a *model whose fields the Update loop
// concurrently writes.
//
// Note: approvalsStrip.Accept(idx) REMOVES the ask, so accepting in a loop runs
// the action exactly once. This invokes the stored Action directly to get
// genuinely repeated concurrent invocations.
func TestAskAction_RaceWithUpdateLoop(t *testing.T) {
	m := createTestModel()
	m.config = &pd.Config{Client: &pd.MockPagerDutyClient{}}
	m.incidentList = []pagerduty.Incident{{
		APIObject:        pagerduty.APIObject{ID: "INC-ORIGIN"},
		Title:            "Origin incident",
		EscalationPolicy: pagerduty.APIObject{ID: "POLICY-OLD"},
	}}

	ask, m := askViaUpdateLoop(t, m, escalationVerdict(), []string{"INC-ORIGIN"})
	require.Equal(t, AskEscalationSuggestion, ask.Kind)

	const iterations = 300

	var wg sync.WaitGroup
	wg.Add(2)

	// Goroutine 1: the tea.Cmd goroutine running the accepted ask's action.
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			if cmd := ask.Action(); cmd != nil {
				_ = cmd()
			}
		}
	}()

	// Goroutine 2: the Update loop replacing the incident list every poll.
	go func() {
		defer wg.Done()
		local := m
		for i := 0; i < iterations; i++ {
			result, _ := local.Update(updatedIncidentListMsg{incidents: []pagerduty.Incident{{
				APIObject:        pagerduty.APIObject{ID: "INC-ORIGIN"},
				Title:            "Origin incident",
				EscalationPolicy: pagerduty.APIObject{ID: "POLICY-NEW"},
			}}})
			if mm, ok := result.(model); ok {
				local = mm
			}
		}
	}()

	wg.Wait()
}

// TestAskAction_RaceWithInPlaceIncidentTitleWrite is the confirmed race.
//
// updatedIncidentTitleMsg writes m.incidentList[i].Title IN PLACE
// (tui.go:533). The slice header in the ask closure's captured model aliases
// the same backing array, so the closure's findIncidentByID walk reads
// pagerduty.Incident values on a tea.Cmd goroutine while the Update loop
// writes one of them on the main loop. Unsynchronized, that is a genuine data
// race on the shared array — not merely a stale read.
func TestAskAction_RaceWithInPlaceIncidentTitleWrite(t *testing.T) {
	m := createTestModel()
	m.config = &pd.Config{Client: &pd.MockPagerDutyClient{}}
	m.incidentList = []pagerduty.Incident{{
		APIObject:        pagerduty.APIObject{ID: "INC-ORIGIN"},
		Title:            "Origin incident",
		EscalationPolicy: pagerduty.APIObject{ID: "POLICY-OLD"},
	}}

	ask, m := askViaUpdateLoop(t, m, escalationVerdict(), []string{"INC-ORIGIN"})
	require.Equal(t, AskEscalationSuggestion, ask.Kind)

	const iterations = 300

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			if cmd := ask.Action(); cmd != nil {
				_ = cmd()
			}
		}
	}()

	go func() {
		defer wg.Done()
		local := m
		for i := 0; i < iterations; i++ {
			result, _ := local.Update(updatedIncidentTitleMsg{
				incidentID: "INC-ORIGIN",
				newTitle:   fmt.Sprintf("retitled-%d", i),
			})
			if mm, ok := result.(model); ok {
				local = mm
			}
		}
	}()

	wg.Wait()
}
