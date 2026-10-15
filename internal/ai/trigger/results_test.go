package trigger

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestAnalysisResultContract(t *testing.T) {
	s, dyn, _ := incidentFixture(t)
	data, err := os.ReadFile("testdata/analysis-result.json")
	if err != nil {
		t.Fatal(err)
	}
	result := &unstructured.Unstructured{}
	if err := json.Unmarshal(data, &result.Object); err != nil {
		t.Fatal(err)
	}
	if err := dyn.Tracker().Create(agenticResultGVR, result, "lightspeed"); err != nil {
		t.Fatal(err)
	}
	run := &unstructured.Unstructured{Object: map[string]any{"metadata": map[string]any{"name": "diagnosis", "namespace": "lightspeed"}, "status": map[string]any{"conditions": []any{map[string]any{"type": "Analyzed", "status": "True"}}, "steps": map[string]any{"analysis": map[string]any{"results": []any{map[string]any{"name": "old-result", "outcome": "Failed"}, map[string]any{"name": "diagnosis-analysis-2", "outcome": "Succeeded"}}}}}}}
	response := s.buildDiagnosisFromRun(context.Background(), run, "demo", "gitops", "uid", "Synced", "Degraded", "")
	if response.Status != "completed" || response.Summary != "The pod cannot pull the requested image." || response.Run.CompletedAt != "2026-09-29T12:02:00Z" || len(response.Suggestions) != 1 {
		t.Fatalf("unexpected response: %+v", response)
	}
	if response.Suggestions[0].Severity != "info" || !strings.Contains(response.Suggestions[0].Remediation, "oc get deployment") {
		t.Fatalf("unexpected suggestion: %+v", response.Suggestions[0])
	}
	// No-action results use the top-level diagnosis and have no options.
	_ = unstructured.SetNestedMap(result.Object, map[string]any{"diagnosis": map[string]any{"summary": "No action is needed.", "rootCause": "Recovered"}, "actionRequired": "False"}, "status")
	if err := dyn.Tracker().Update(agenticResultGVR, result, "lightspeed"); err != nil {
		t.Fatal(err)
	}
	response = s.buildDiagnosisFromRun(context.Background(), run, "demo", "gitops", "uid", "Synced", "Healthy", "")
	if response.Summary != "No action is needed." || len(response.Suggestions) != 0 {
		t.Fatalf("no-action response: %+v", response)
	}
	// A result belonging to another run must not leak into this diagnosis.
	_ = unstructured.SetNestedField(result.Object, "other-run", "spec", "agenticRunName")
	_ = dyn.Tracker().Update(agenticResultGVR, result, "lightspeed")
	response = s.buildDiagnosisFromRun(context.Background(), run, "demo", "gitops", "uid", "Synced", "Healthy", "")
	if response.Status != "running" || response.Summary != "" {
		t.Fatalf("foreign result returned: %+v", response)
	}
	_ = dyn.Tracker().Delete(agenticResultGVR, "lightspeed", result.GetName())
	response = s.buildDiagnosisFromRun(context.Background(), run, "demo", "gitops", "uid", "Synced", "Healthy", "")
	if response.Status != "running" {
		t.Fatal("missing successful result must stay pending")
	}
}

func TestAnalysisFailureReasonAndConditionMapping(t *testing.T) {
	s, _, _ := incidentFixture(t)
	for _, tc := range []struct{ kind, status, reason, want string }{{"Analyzed", "False", "AgentNotFound", "failed"}, {"Analyzed", "Unknown", "Starting", "running"}, {"Analyzed", "True", "NoActionRequired", "completed"}, {"EmergencyStopped", "True", "Stopped", "failed"}, {"Denied", "True", "PolicyDenied", "failed"}} {
		run := &unstructured.Unstructured{Object: map[string]any{"status": map[string]any{"conditions": []any{map[string]any{"type": tc.kind, "status": tc.status, "reason": tc.reason, "message": "Agent is unavailable"}}}}}
		if got := diagnosisStateForRun(run); got != tc.want {
			t.Fatalf("%s %s = %s", tc.kind, tc.status, got)
		}
		if tc.kind == "Analyzed" && tc.status == "False" {
			response := s.buildDiagnosisFromRun(context.Background(), run, "demo", "gitops", "uid", "Synced", "Degraded", "")
			if response.Status != "failed" || response.Summary != "Agent is unavailable" {
				t.Fatalf("failure hidden: %+v", response)
			}
		}
	}
}

func TestDiagnosisStateForRun(t *testing.T) {
	condition := func(kind, status string) map[string]any {
		return map[string]any{"type": kind, "status": status}
	}
	tests := []struct {
		name       string
		conditions []map[string]any
		want       string
	}{
		{"no conditions", nil, "running"},
		{"unrecognized", []map[string]any{condition("Approved", "True")}, "running"},
		{"unknown", []map[string]any{condition("Analyzed", "Unknown")}, "running"},
		{"executed", []map[string]any{condition("Executed", "True")}, "running"},
		{"analyzed", []map[string]any{condition("Analyzed", "True")}, "completed"},
		{"verified", []map[string]any{condition("Verified", "True")}, "completed"},
		{"denied", []map[string]any{condition("Denied", "True")}, "failed"},
		{"emergency stopped", []map[string]any{condition("EmergencyStopped", "True")}, "failed"},
		{"escalated", []map[string]any{condition("Escalated", "True")}, "failed"},
		{"executed then analyzed", []map[string]any{condition("Executed", "True"), condition("Analyzed", "True")}, "completed"},
		{"analyzed then executed", []map[string]any{condition("Analyzed", "True"), condition("Executed", "True")}, "completed"},
		{"executed then verified", []map[string]any{condition("Executed", "True"), condition("Verified", "True")}, "completed"},
		{"verified then executed", []map[string]any{condition("Verified", "True"), condition("Executed", "True")}, "completed"},
		{"analyzed then verified", []map[string]any{condition("Analyzed", "True"), condition("Verified", "True")}, "completed"},
		{"verified then analyzed", []map[string]any{condition("Verified", "True"), condition("Analyzed", "True")}, "completed"},
		{"executed then denied", []map[string]any{condition("Executed", "True"), condition("Denied", "True")}, "failed"},
		{"denied then executed", []map[string]any{condition("Denied", "True"), condition("Executed", "True")}, "failed"},
		{"analyzed then denied", []map[string]any{condition("Analyzed", "True"), condition("Denied", "True")}, "failed"},
		{"denied then analyzed", []map[string]any{condition("Denied", "True"), condition("Analyzed", "True")}, "failed"},
		{"analyzed false", []map[string]any{condition("Analyzed", "False")}, "failed"},
		{"executed false", []map[string]any{condition("Executed", "False")}, "failed"},
		{"verified false", []map[string]any{condition("Verified", "False")}, "failed"},
		{"escalated false", []map[string]any{condition("Escalated", "False")}, "failed"},
		{"escalated unknown", []map[string]any{condition("Escalated", "Unknown")}, "running"},
		{"analyzed then escalated", []map[string]any{condition("Analyzed", "True"), condition("Escalated", "True")}, "failed"},
		{"analyzed then emergency stopped", []map[string]any{condition("Analyzed", "True"), condition("EmergencyStopped", "True")}, "failed"},
	}
	retrying := condition("Verified", "False")
	retrying["reason"] = "RetryingExecution"
	tests = append(tests, struct {
		name       string
		conditions []map[string]any
		want       string
	}{"verification retry", []map[string]any{retrying}, "running"})
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			raw := make([]any, len(tc.conditions))
			for i, c := range tc.conditions {
				raw[i] = c
			}
			run := &unstructured.Unstructured{Object: map[string]any{"status": map[string]any{"conditions": raw}}}
			if got := diagnosisStateForRun(run); got != tc.want {
				t.Fatalf("diagnosisStateForRun() = %q, want %q", got, tc.want)
			}
		})
	}
}
