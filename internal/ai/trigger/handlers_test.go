package trigger

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/redhat-developer/gitops-operator/internal/agentic"
)

func TestBuildAgenticRunObject_LabelsPreserved(t *testing.T) {
	obj := buildAgenticRunObject("diagnosis", "openshift-lightspeed", AgenticRunParams{
		AppName:      "my-app",
		AppNamespace: "openshift-gitops",
		AppUID:       "uid-123",
		SyncStatus:   "OutOfSync",
		HealthStatus: "Degraded",
		TargetNS:     "demo",
		Agent:        "default",
	})

	labels := obj.GetLabels()
	expectedLabels := map[string]string{
		"gitops.openshift.io/application-namespace": "openshift-gitops",
		"gitops.openshift.io/application-uid":       "uid-123",
		"gitops.openshift.io/managed-by":            "agentic-trigger",
	}
	if obj.GetAnnotations()["gitops.openshift.io/application-name"] != "my-app" {
		t.Fatal("full name annotation missing")
	}
	for k, v := range expectedLabels {
		if labels[k] != v {
			t.Errorf("label %q: expected %q, got %v", k, v, labels[k])
		}
	}
}

func TestBuildAgenticRunObject_AnnotationsPreserved(t *testing.T) {
	obj := buildAgenticRunObject("diagnosis", "openshift-lightspeed", AgenticRunParams{
		AppName:      "my-app",
		AppNamespace: "openshift-gitops",
		SyncStatus:   "OutOfSync",
		HealthStatus: "Degraded",
		Revision:     "abc123",
		TargetNS:     "demo",
		Agent:        "default",
	})

	annotations := obj.GetAnnotations()
	if annotations["gitops.openshift.io/trigger-type"] != "automatic" {
		t.Errorf("trigger-type annotation wrong: %v", annotations["gitops.openshift.io/trigger-type"])
	}
	if annotations["gitops.openshift.io/trigger-sync-status"] != "OutOfSync" {
		t.Errorf("sync-status annotation wrong: %v", annotations["gitops.openshift.io/trigger-sync-status"])
	}
	if annotations["gitops.openshift.io/trigger-health-status"] != "Degraded" {
		t.Errorf("health-status annotation wrong: %v", annotations["gitops.openshift.io/trigger-health-status"])
	}
	if annotations["gitops.openshift.io/trigger-revision"] != "abc123" {
		t.Errorf("revision annotation wrong: %v", annotations["gitops.openshift.io/trigger-revision"])
	}
}

func TestDiagnosisEndpointUsesLatestOwnedRun(t *testing.T) {
	s, dyn, now := incidentFixture(t)
	first := reconcileApp(t, s)
	app, err := s.getApplication(context.Background(), "gitops", "demo")
	if err != nil {
		t.Fatal(err)
	}
	if app.GetAnnotations()[agentic.IncidentAnnotation] == "" {
		t.Fatal("incident was not persisted")
	}

	state, err := decodeIncident(app)
	if err != nil {
		t.Fatal(err)
	}
	state.Attempt = 2
	s.prepareRunAttempt(app, state)
	if err := s.saveIncident(context.Background(), app, state); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/applications/gitops/demo/diagnosis", nil)
	request.SetPathValue("namespace", "gitops")
	request.SetPathValue("name", "demo")
	request.Header.Set("Authorization", "Bearer token")
	checkRun := func(want string) {
		t.Helper()
		response := httptest.NewRecorder()
		s.HandleDiagnosis(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
		}
		var diagnosis DiagnosisResponse
		if err := json.Unmarshal(response.Body.Bytes(), &diagnosis); err != nil {
			t.Fatal(err)
		}
		if diagnosis.Run == nil || diagnosis.Run.Name != want {
			t.Fatalf("selected run=%+v, want %q", diagnosis.Run, want)
		}
	}
	checkRun(first.GetName()) // While attempt 2 is unconfirmed, show the last existing run.

	app, err = s.getApplication(context.Background(), "gitops", "demo")
	if err != nil {
		t.Fatal(err)
	}
	second := s.buildIncidentRun(app, state)
	annotations := second.GetAnnotations()
	annotations["gitops.openshift.io/triggered-at"] = now.Add(time.Minute).Format(time.RFC3339Nano)
	second.SetAnnotations(annotations)
	if err := dyn.Tracker().Create(agenticRunGVR, second, "lightspeed"); err != nil {
		t.Fatal(err)
	}
	checkRun(second.GetName()) // The latest owned run is selected.
}
