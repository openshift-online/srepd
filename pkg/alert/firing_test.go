package alert

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestParseFiring_AlertmanagerFormat(t *testing.T) {
	firing := `Labels:
 - alertname = MachineHealthCheckUnterminatedShortCircuitSRE
 - container = kube-rbac-proxy-mhc-mtrc
 - namespace = openshift-machine-api
 - severity = critical
Annotations:
 - description = MHC has been short circuited for too long
 - runbook = https://github.com/openshift/ops-sop/blob/master/v4/alerts/MachineHealthCheckUnterminatedShortCircuitSRE.md
Source: https://prometheus.example.com`

	result := ParseFiring(firing)

	assert.Equal(t, "MachineHealthCheckUnterminatedShortCircuitSRE", result["alertname"])
	assert.Equal(t, "kube-rbac-proxy-mhc-mtrc", result["container"])
	assert.Equal(t, "openshift-machine-api", result["namespace"])
	assert.Equal(t, "critical", result["severity"])
	assert.Equal(t, "MHC has been short circuited for too long", result["description"])
	assert.Equal(t, "https://github.com/openshift/ops-sop/blob/master/v4/alerts/MachineHealthCheckUnterminatedShortCircuitSRE.md", result["runbook"])
}

func TestParseFiring_RHOBSFormat(t *testing.T) {
	firing := `

  - alertname: ClusterOperatorDown
    cluster_id: a4ba96fe-ac69-4573-a78b-17d38eeaab99
    namespace: ocm-production-abc123
    description: The ingress operator is unavailable for cluster

`

	result := ParseFiring(firing)

	assert.Equal(t, "ClusterOperatorDown", result["alertname"])
	assert.Equal(t, "a4ba96fe-ac69-4573-a78b-17d38eeaab99", result["cluster_id"])
	assert.Equal(t, "ocm-production-abc123", result["namespace"])
	assert.Equal(t, "The ingress operator is unavailable for cluster", result["description"])
}

func TestParseFiring_Empty(t *testing.T) {
	result := ParseFiring("")
	assert.NotNil(t, result)
	assert.Empty(t, result)
}

func TestParseFiring_MalformedGraceful(t *testing.T) {
	// Garbage input should not panic and should return empty map
	result := ParseFiring("this is not a valid firing format at all\nrandom garbage")
	assert.NotNil(t, result)
	// Should not crash, result may or may not have entries
}

func TestParseFiring_AlertmanagerWithSOP(t *testing.T) {
	// Test SOP extraction from message annotation
	firing := `Labels:
 - alertname = ClusterProvisioningDelay
 - severity = high
Annotations:
 - message = cluster edf-mas has been in a failed state. SOP: https://github.com/openshift/ops-sop/blob/master/v4/alerts/ClusterProvisioningDelay.md
 - dashboard = https://grafana.example.com/d/dashboard
Source: https://prometheus.example.com`

	result := ParseFiring(firing)
	assert.Equal(t, "ClusterProvisioningDelay", result["alertname"])
	assert.Equal(t, "high", result["severity"])
	assert.Contains(t, result["message"], "SOP:")
	assert.Equal(t, "https://grafana.example.com/d/dashboard", result["dashboard"])
}

func TestParseFiring_WhitespaceOnly(t *testing.T) {
	result := ParseFiring("   \n\n   \n")
	assert.NotNil(t, result)
	assert.Empty(t, result)
}

func TestParseFiring_JSONArrayFormat(t *testing.T) {
	// Some app-interface PrometheusRules deliver the raw Alertmanager webhook
	// JSON verbatim in the firing detail field, instead of the
	// "Labels:\n - key = value" text dump. Shaped after a real
	// ClusterProvisioningDelay incident (cluster/namespace/host values below
	// are synthetic, not the real ones).
	firing := `[
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

	result := ParseFiring(firing)

	assert.Equal(t, "ClusterProvisioningDelay - production", result["alertname"])
	assert.Equal(t, "demo1-abc", result["cluster_deployment"])
	assert.Equal(t, "ProvisionFailed", result["condition"])
	assert.Equal(t, "BootstrapFailed", result["reason"])
	assert.Equal(t, "high", result["severity"])
	assert.Equal(t, "https://github.com/openshift/ops-sop/blob/master/v4/alerts/ClusterProvisioningFailure.md", result["runbook"])
	assert.Contains(t, result["message"], "SOP:")
	assert.Equal(t, "https://grafana.example.com/d/example/example-dashboard?orgId=1", result["dashboard"])
}

func TestParseFiring_JSONObjectFormat(t *testing.T) {
	// A bare JSON object (not wrapped in an array) should parse the same way.
	firing := `{
  "annotations": {
    "runbook": "https://github.com/openshift/ops-sop/blob/master/v4/alerts/Example.md"
  },
  "labels": {
    "alertname": "Example",
    "severity": "critical"
  }
}`

	result := ParseFiring(firing)

	assert.Equal(t, "Example", result["alertname"])
	assert.Equal(t, "critical", result["severity"])
	assert.Equal(t, "https://github.com/openshift/ops-sop/blob/master/v4/alerts/Example.md", result["runbook"])
}

func TestParseFiring_JSONMalformedGraceful(t *testing.T) {
	// Truncated/invalid JSON must not panic and should return an empty map,
	// same contract as TestParseFiring_MalformedGraceful.
	result := ParseFiring(`[{"labels": {"alertname": "Broken"`)
	assert.NotNil(t, result)
	assert.Empty(t, result)
}

func TestParseFiring_JSONEmptyArray(t *testing.T) {
	result := ParseFiring(`[]`)
	assert.NotNil(t, result)
	assert.Empty(t, result)
}

// --- parseFiringValue: the "firing" detail as go-pagerduty actually decodes
// it (IncidentAlert.Body is map[string]interface{}, so a JSON array/object
// detail arrives pre-decoded into []interface{} / map[string]interface{},
// never as a string). ---

func TestParseFiringValue_DecodedArray(t *testing.T) {
	decoded := decodeJSON(t, appSREJSONFiringText)

	result := parseFiringValue(decoded)

	assert.Equal(t, "ClusterProvisioningDelay - production", result["alertname"])
	assert.Equal(t, "0000aaaa1111bbbb2222cccc3333dddd", result["cluster_id"])
	assert.Equal(t, "high", result["severity"])
	assert.Equal(t, "https://github.com/openshift/ops-sop/blob/master/v4/alerts/ClusterProvisioningFailure.md", result["runbook"])
}

func TestParseFiringValue_DecodedObject(t *testing.T) {
	decoded := decodeJSON(t, `{"labels": {"alertname": "Example", "severity": "critical"}, "annotations": {"runbook": "https://example.com/sop"}}`)

	result := parseFiringValue(decoded)

	assert.Equal(t, "Example", result["alertname"])
	assert.Equal(t, "critical", result["severity"])
	assert.Equal(t, "https://example.com/sop", result["runbook"])
}

func TestParseFiringValue_String(t *testing.T) {
	// A "firing" detail that PD happens to deliver as a JSON-encoded string
	// (rather than pre-decoded) must still work — routed through ParseFiring.
	result := parseFiringValue(`[{"labels": {"alertname": "FromString"}}]`)
	assert.Equal(t, "FromString", result["alertname"])
}

func TestParseFiringValue_NilAndUnknownTypes(t *testing.T) {
	tests := []struct {
		name string
		raw  interface{}
	}{
		{"nil", nil},
		{"int", 42},
		{"float", 3.14},
		{"bool", true},
		{"string slice element type", []interface{}{"not-an-object"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := parseFiringValue(tt.raw)
			assert.NotNil(t, result)
			assert.Empty(t, result)
		})
	}
}

func TestParseFiring_JSONTextAndDecodedAgree(t *testing.T) {
	// The JSON-text path (ParseFiring) and the pre-decoded path
	// (parseFiringValue) must go through the same flatten logic and
	// therefore always agree, for every fixture — including one with a
	// non-string label value.
	fixtures := []string{
		appSREJSONFiringText,
		appSREJSONFiringTwoAlerts,
		`{"labels": {"a": "b", "n": 5}}`,
	}
	for _, f := range fixtures {
		fromText := ParseFiring(f)
		fromDecoded := parseFiringValue(decodeJSON(t, f))
		assert.Equal(t, fromText, fromDecoded, "ParseFiring and parseFiringValue disagree for: %s", f)
	}
}

func TestParseFiring_JSONLastAlertWins(t *testing.T) {
	result := ParseFiring(appSREJSONFiringTwoAlerts)

	assert.Equal(t, "Second", result["alertname"])
	assert.Equal(t, "critical", result["severity"])
}

func TestParseFiring_JSONNonStringValuesSkipped(t *testing.T) {
	// A non-string label value must not fail the whole parse — only that
	// key is skipped, everything else survives.
	result := ParseFiring(`{"labels": {"a": "b", "n": 5}}`)

	assert.Equal(t, map[string]string{"a": "b"}, result)
}

func TestParseFiring_JSONMalformedElements(t *testing.T) {
	// Wrong-typed array elements are skipped, not fatal; well-formed ones
	// still contribute.
	result := ParseFiring(`[1, "x", {"labels": {"a": "b"}}]`)

	assert.Equal(t, map[string]string{"a": "b"}, result)
}
