package alert

import (
	"encoding/json"
	"regexp"
	"strings"
)

// alertmanagerKVPattern matches " - key = value" lines from Alertmanager firing format.
var alertmanagerKVPattern = regexp.MustCompile(`^\s*-\s+(\S+)\s+=\s+(.+)$`)

// rhobsKVPattern matches "    key: value" lines from RHOBS YAML-like firing format.
var rhobsKVPattern = regexp.MustCompile(`^\s+-?\s*(\S+):\s+(.+)$`)

// ParseFiring auto-detects the firing field format and extracts all key-value pairs
// into a flat map. Handles three formats:
//   - Alertmanager text format: starts with "Labels:" and uses " - key = value" lines
//   - RHOBS YAML format: uses "  - key: value" or "    key: value" lines
//   - Raw Alertmanager webhook JSON: a JSON object, or array of objects, each
//     with "labels"/"annotations" maps
//
// This only handles "firing" as a Go string. go-pagerduty decodes
// IncidentAlert.Body as map[string]interface{}, so a JSON detail (array or
// object) normally arrives already decoded into []interface{} /
// map[string]interface{} — never as a string. Callers reading the detail
// straight off the alert body should use parseFiringValue instead, which
// accepts either shape and delegates the JSON-text case back to this
// function via the same flatten logic.
//
// Returns an empty (non-nil) map for empty or unrecognized input.
func ParseFiring(firing string) map[string]string {
	result := make(map[string]string)
	trimmed := strings.TrimSpace(firing)
	if trimmed == "" {
		return result
	}

	if strings.HasPrefix(trimmed, "[") || strings.HasPrefix(trimmed, "{") {
		var decoded interface{}
		if err := json.Unmarshal([]byte(trimmed), &decoded); err != nil {
			return result
		}
		flattenAlertObjects(decodeAlertObjects(decoded), result)
		return result
	}

	lines := strings.Split(firing, "\n")

	// Detect format by looking at the content
	isAlertmanager := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "Labels:" {
			isAlertmanager = true
			break
		}
	}

	if isAlertmanager {
		parseAlertmanagerFiring(lines, result)
	} else {
		parseRHOBSFiring(lines, result)
	}

	return result
}

// parseFiringValue accepts the "firing" detail exactly as decoded from the
// PagerDuty alert body — string, []interface{}, map[string]interface{}, or
// anything else (including nil) — and flattens it into the same map
// ParseFiring produces for the text formats. This is what alert.go's
// getFiringDetail calls; ParseFiring is kept public for callers that
// already have "firing" as a string in hand.
//
//	string                   → ParseFiring (text formats, or JSON text)
//	[]interface{}            → list of alert objects, flattened
//	map[string]interface{}   → single alert object, flattened
//	anything else / nil      → empty, non-nil map
//
// The string case and the pre-decoded cases both end up calling
// flattenAlertObjects, so the two paths cannot disagree with each other.
func parseFiringValue(raw interface{}) map[string]string {
	result := make(map[string]string)
	if s, ok := raw.(string); ok {
		return ParseFiring(s)
	}
	flattenAlertObjects(decodeAlertObjects(raw), result)
	return result
}

// decodeAlertObjects normalizes a decoded JSON firing value — a single alert
// object or an array of them — into a slice of alert objects for
// flattenAlertObjects. Array elements that are not objects are skipped, not
// fatal. Anything else (nil, a JSON scalar, ...) contributes nothing.
func decodeAlertObjects(raw interface{}) []map[string]interface{} {
	switch v := raw.(type) {
	case map[string]interface{}:
		return []map[string]interface{}{v}
	case []interface{}:
		var alerts []map[string]interface{}
		for _, elem := range v {
			if obj, ok := elem.(map[string]interface{}); ok {
				alerts = append(alerts, obj)
			}
		}
		return alerts
	default:
		return nil
	}
}

// flattenAlertObjects copies each alert object's "labels" then
// "annotations" into result, in order. Later alerts overwrite earlier ones
// for a repeated key — matching the text-format parsers, where a repeated
// key's last line wins. Non-string label/annotation values are skipped, not
// fatal, so one bad value (e.g. a numeric label) doesn't drop the rest.
func flattenAlertObjects(alerts []map[string]interface{}, result map[string]string) {
	for _, a := range alerts {
		copyStringValues(a["labels"], result)
		copyStringValues(a["annotations"], result)
	}
}

// copyStringValues copies the string-valued entries of raw (expected to be
// a map[string]interface{}) into result. Anything else — wrong type, nil —
// contributes nothing.
func copyStringValues(raw interface{}, result map[string]string) {
	m, ok := raw.(map[string]interface{})
	if !ok {
		return
	}
	for k, v := range m {
		if s, ok := v.(string); ok {
			result[k] = s
		}
	}
}

// parseAlertmanagerFiring parses the Alertmanager " - key = value" format.
// Handles both Labels and Annotations sections.
func parseAlertmanagerFiring(lines []string, result map[string]string) {
	for _, line := range lines {
		// Skip section headers and Source: line
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || trimmed == "Labels:" || trimmed == "Annotations:" || strings.HasPrefix(trimmed, "Source:") {
			continue
		}

		matches := alertmanagerKVPattern.FindStringSubmatch(line)
		if matches != nil {
			key := strings.TrimSpace(matches[1])
			value := strings.TrimSpace(matches[2])
			result[key] = value
		}
	}
}

// parseRHOBSFiring parses the RHOBS YAML-like "key: value" format.
func parseRHOBSFiring(lines []string, result map[string]string) {
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}

		matches := rhobsKVPattern.FindStringSubmatch(line)
		if matches != nil {
			key := strings.TrimSpace(matches[1])
			value := strings.TrimSpace(matches[2])
			result[key] = value
		}
	}
}
