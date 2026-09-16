package alert

import (
	"encoding/json"
	"testing"
)

// appSREJSONFiringText is a synthetic app-sre-alertmanager "firing" JSON
// payload, shaped after a real ClusterProvisioningDelay incident report.
// Cluster/namespace/host values are fabricated, not the real incident's.
// Includes a "cluster_id" label (in addition to the usual top-level PD
// detail) so the label-fallback path in parseAppSRE is exercised too.
const appSREJSONFiringText = `[
  {
    "annotations": {
      "dashboard": "https://grafana.example.com/d/example/example-dashboard?orgId=1",
      "html_url": "https://gitlab.example.com/example-org/example-repo/blob/main/example-alerts.yaml",
      "message": "cluster demo1-abc in namespace uhc-production-0000aaaa1111bbbb2222cccc3333dddd provisioning taking over 2 hours. Condition/Reason: ProvisionFailed / BootstrapFailed. SOP: https://github.com/openshift/ops-sop/blob/master/v4/alerts/ClusterProvisioningFailure.md",
      "runbook": "https://github.com/openshift/ops-sop/blob/master/v4/alerts/ClusterProvisioningFailure.md"
    },
    "endsAt": "0001-01-01T00:00:00Z",
    "fingerprint": "aa11bb22cc33dd44ee55ff66",
    "generatorURL": "http://prometheus.example.com:9090/graph",
    "labels": {
      "alertname": "ClusterProvisioningDelay - production",
      "cluster": "hivep01ex1",
      "cluster_deployment": "demo1-abc",
      "cluster_id": "0000aaaa1111bbbb2222cccc3333dddd",
      "condition": "ProvisionFailed",
      "environment": "production",
      "exported_namespace": "uhc-production-0000aaaa1111bbbb2222cccc3333dddd",
      "namespace": "hive",
      "platform": "aws",
      "reason": "BootstrapFailed",
      "service": "hive",
      "severity": "high",
      "team": "srep"
    },
    "startsAt": "2026-01-15T12:00:00.000Z",
    "status": "firing"
  }
]`

// appSREJSONFiringTwoAlerts is a two-element firing array used to verify
// last-alert-wins semantics: the second alert's severity/alertname must
// win, matching how the text-format parsers let a repeated key's last
// line win.
const appSREJSONFiringTwoAlerts = `[
  {"labels": {"alertname": "First", "severity": "low"}},
  {"labels": {"alertname": "Second", "severity": "critical"}}
]`

// decodeJSON unmarshals s into a generic interface{}, reproducing exactly
// the dynamic types go-pagerduty's map[string]interface{} decoding of
// IncidentAlert.Body would produce for this JSON. Test fixtures must be
// built through this helper rather than hand-written []interface{} or
// map[string]interface{} literals, so they can't drift from the real
// decoded shape.
func decodeJSON(t *testing.T, s string) interface{} {
	t.Helper()
	var v interface{}
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatalf("decodeJSON: %v", err)
	}
	return v
}
