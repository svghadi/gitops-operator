package controllers

import (
	"strings"
	"testing"
)

func TestAgenticDiagnoseTriggerDeliversHealthWithoutGraceFiltering(t *testing.T) {
	got := agenticNotificationTrigger()

	for _, health := range []string{"Degraded", "Unknown", "Missing", "Progressing", "Healthy", "Suspended"} {
		if !strings.Contains(got, "app.status.health.status == '"+health+"'") {
			t.Errorf("trigger missing health clause %s:\n%s", health, got)
		}
	}
	if strings.Contains(got, "creationTimestamp") || strings.Contains(got, "time.Parse") || strings.Contains(got, "time.Now().Sub") {
		t.Fatalf("notification trigger still filters by grace: %s", got)
	}
	if strings.Count(got, "  send: [agentic-diagnose]") != 6 {
		t.Fatalf("missing notification delivery clause: %s", got)
	}
}
