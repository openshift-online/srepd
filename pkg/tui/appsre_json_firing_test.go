package tui

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/PagerDuty/go-pagerduty"
	"github.com/charmbracelet/log"
	"github.com/stretchr/testify/assert"
)

// appSREJSONFiring is a synthetic app-sre-alertmanager "firing" JSON payload
// (fabricated cluster/alert values), shaped after a real
// ClusterProvisioningDelay/HCPNodepoolUpgradeDelay-style incident where
// PagerDuty delivers "firing" as the raw Alertmanager webhook JSON instead
// of the "Labels:\n - key = value" text dump, and no top-level cluster_id
// detail exists — the cluster ID lives only in the firing JSON's label.
const appSREJSONFiring = `[
  {
    "annotations": {
      "runbook": "https://github.com/openshift/ops-sop/blob/master/v4/alerts/ClusterProvisioningFailure.md"
    },
    "labels": {
      "alertname": "ClusterProvisioningDelay - production",
      "cluster_id": "0000aaaa1111bbbb2222cccc3333dddd",
      "severity": "high"
    }
  }
]`

// decodeJSONFiring unmarshals s into a generic interface{}, reproducing the
// dynamic types go-pagerduty's map[string]interface{} decoding of
// IncidentAlert.Body would produce. Fixtures must go through this rather
// than hand-written []interface{}/map[string]interface{} literals, so they
// can't drift from the real decoded shape.
func decodeJSONFiring(t *testing.T, s string) interface{} {
	t.Helper()
	var v interface{}
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatalf("decodeJSONFiring: %v", err)
	}
	return v
}

// makeAppSREAlertWithDecodedFiring builds an IncidentAlert whose "firing"
// detail is the pre-decoded shape PagerDuty actually delivers for a JSON
// firing payload — not a Go string. There is no top-level "cluster_id"
// detail, matching HCPNodepoolUpgradeDelay-shaped incidents.
func makeAppSREAlertWithDecodedFiring(t *testing.T, firing string) pagerduty.IncidentAlert {
	return pagerduty.IncidentAlert{
		Service: pagerduty.APIObject{Summary: "app-sre-alertmanager"},
		Body: map[string]interface{}{
			"details": map[string]interface{}{
				"firing": decodeJSONFiring(t, firing),
			},
		},
	}
}

func TestGetUniqueClusters_JSONFiringLabel(t *testing.T) {
	alerts := []pagerduty.IncidentAlert{makeAppSREAlertWithDecodedFiring(t, appSREJSONFiring)}

	result := getUniqueClusters(alerts)

	assert.Equal(t, []string{"0000aaaa1111bbbb2222cccc3333dddd"}, result)
}

func TestGetSOPLink_JSONFiringRunbook(t *testing.T) {
	alerts := []pagerduty.IncidentAlert{makeAppSREAlertWithDecodedFiring(t, appSREJSONFiring)}

	link, ok := getSOPLink(alerts)

	assert.True(t, ok)
	assert.Equal(t, "https://github.com/openshift/ops-sop/blob/master/v4/alerts/ClusterProvisioningFailure.md", link)
}

func TestMapClusterServices_JSONFiringLabel(t *testing.T) {
	alerts := []pagerduty.IncidentAlert{makeAppSREAlertWithDecodedFiring(t, appSREJSONFiring)}

	result := mapClusterServices(alerts)

	assert.Equal(t, "app-sre-alertmanager", result["0000aaaa1111bbbb2222cccc3333dddd"])
}

func TestGetUniqueClusters_JSONFiringMalformedLabelClusterID(t *testing.T) {
	// A malformed cluster_id sourced from the firing JSON's label (not a
	// top-level detail) must still be rejected by the same
	// ocm.ValidClusterID guard that protects top-level cluster_ids, with
	// the same warning logged.
	firing := `[{"labels": {"alertname": "X", "cluster_id": "abc; rm -rf /"}}]`
	alerts := []pagerduty.IncidentAlert{makeAppSREAlertWithDecodedFiring(t, firing)}

	var result []string
	output := captureLogOutput(log.WarnLevel, func() {
		result = getUniqueClusters(alerts)
	})

	assert.Empty(t, result)
	assert.Contains(t, output, "skipping malformed cluster_id")
}

func TestBuildWatcherContext_JSONFiringSOP(t *testing.T) {
	m := createTestModel()
	inc := pagerduty.Incident{
		APIObject: pagerduty.APIObject{ID: "P1"},
		Title:     "ClusterProvisioningDelay - production",
		Service:   pagerduty.APIObject{Summary: "app-sre-alertmanager"},
		Status:    "triggered",
		Urgency:   "high",
	}
	m.selectedIncident = &inc
	m.incidentList = []pagerduty.Incident{inc}
	m.incidentCache[inc.ID] = &cachedIncidentData{
		alerts:       []pagerduty.IncidentAlert{makeAppSREAlertWithDecodedFiring(t, appSREJSONFiring)},
		alertsLoaded: true,
	}

	ctx := buildWatcherContext(&m)

	assert.Equal(t, 1, strings.Count(ctx, "SOP:"))
	assert.Contains(t, ctx, "SOP: https://github.com/openshift/ops-sop/blob/master/v4/alerts/ClusterProvisioningFailure.md")
	assert.NotContains(t, ctx, "annotations")
}

func TestBuildObservationContext_JSONFiringSOP(t *testing.T) {
	inc := pagerduty.Incident{
		APIObject: pagerduty.APIObject{ID: "INC-A"},
		Title:     "ClusterProvisioningDelay - production",
		Service:   pagerduty.APIObject{Summary: "app-sre-alertmanager"},
		Status:    "triggered",
		Urgency:   "high",
	}
	m := createTestModel()
	m.incidentList = []pagerduty.Incident{inc}
	m.incidentCache[inc.ID] = &cachedIncidentData{
		alerts:       []pagerduty.IncidentAlert{makeAppSREAlertWithDecodedFiring(t, appSREJSONFiring)},
		alertsLoaded: true,
	}

	obs := watcherObservation{Summary: "x", IncidentIDs: []string{"INC-A"}}
	ctx := buildObservationContext(&m, obs)

	assert.Equal(t, 1, strings.Count(ctx, "SOP:"))
	assert.Contains(t, ctx, "SOP: https://github.com/openshift/ops-sop/blob/master/v4/alerts/ClusterProvisioningFailure.md")
	assert.NotContains(t, ctx, "annotations")
}

func TestBuildWatcherContext_TextFiringSOP(t *testing.T) {
	// Pre-existing text-format firing: before this fix, watcher.go embedded
	// the entire "Labels:\n...\nAnnotations:\n..." dump under an "SOP:"
	// heading (mislabeled, since the raw dump isn't a URL). It must now
	// show only the actual SOP URL.
	firingText := `Labels:
 - alertname = ClusterProvisioningDelay
 - severity = high
Annotations:
 - runbook = https://github.com/openshift/ops-sop/blob/master/v4/alerts/ClusterProvisioningDelay.md
Source: https://prometheus.example.com`

	m := createTestModel()
	inc := pagerduty.Incident{
		APIObject: pagerduty.APIObject{ID: "P1"},
		Title:     "ClusterProvisioningDelay - production",
		Service:   pagerduty.APIObject{Summary: "app-sre-alertmanager"},
		Status:    "triggered",
		Urgency:   "high",
	}
	m.selectedIncident = &inc
	m.incidentList = []pagerduty.Incident{inc}
	m.incidentCache[inc.ID] = &cachedIncidentData{
		alerts: []pagerduty.IncidentAlert{{
			Service: pagerduty.APIObject{Summary: "app-sre-alertmanager"},
			Body: map[string]interface{}{
				"details": map[string]interface{}{
					"firing": firingText,
				},
			},
		}},
		alertsLoaded: true,
	}

	ctx := buildWatcherContext(&m)

	assert.Contains(t, ctx, "SOP: https://github.com/openshift/ops-sop/blob/master/v4/alerts/ClusterProvisioningDelay.md")
	assert.NotContains(t, ctx, "Labels:")
}
