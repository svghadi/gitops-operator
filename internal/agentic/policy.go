// Package agentic contains policy shared by the operator and notification receiver.
package agentic

import (
	"encoding/json"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const (
	EnabledAnnotation  = "gitops.openshift.io/agentic-integration-enabled"
	IncidentAnnotation = "gitops.openshift.io/diagnosis-incident"
	AppName            = "gitops.openshift.io/application-name"
	AppNamespace       = "gitops.openshift.io/application-namespace"
	AppUID             = "gitops.openshift.io/application-uid"
	ManagedBy          = "gitops.openshift.io/managed-by"
	Manager            = "agentic-trigger"

	DiagnosisState = "gitops.openshift.io/diagnosis-state"
	DiagnosisRun   = "gitops.openshift.io/diagnosis-run"
)

func Duration(value string, fallback time.Duration, allowZero bool) (time.Duration, error) {
	if value == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(value)
	if err != nil {
		return 0, err
	}
	if d < 0 || (!allowZero && d == 0) {
		return 0, fmt.Errorf("invalid duration %q", value)
	}
	return d, nil
}

func Revision(app *unstructured.Unstructured) string {
	if revisions, found, _ := unstructured.NestedStringSlice(app.Object, "status", "sync", "revisions"); found && len(revisions) > 0 {
		b, _ := json.Marshal(revisions)
		return string(b)
	}
	r, _, _ := unstructured.NestedString(app.Object, "status", "sync", "revision")
	return r
}

func Health(app *unstructured.Unstructured) string {
	h, _, _ := unstructured.NestedString(app.Object, "status", "health", "status")
	return h
}

func Recovered(health string) bool { return health == "Healthy" || health == "Suspended" }

// Eligibility returns whether diagnosis is needed and the remaining grace.
// Missing transition timestamps are not evidence of sustained poor health.
func Eligibility(app *unstructured.Unstructured, now time.Time, grace time.Duration) (bool, time.Duration) {
	health := Health(app)
	switch health {
	case "Degraded", "Unknown", "Missing", "Progressing":
	default:
		return false, 0
	}
	created := app.GetCreationTimestamp().Time
	remaining := grace - now.Sub(created)
	if health == "Missing" || health == "Progressing" {
		value, _, _ := unstructured.NestedString(app.Object, "status", "health", "lastTransitionTime")
		transition, err := time.Parse(time.RFC3339, value)
		if err != nil {
			return true, grace + time.Second
		}
		if d := grace - now.Sub(transition); d > remaining {
			remaining = d
		}
	}
	if remaining < 0 {
		remaining = 0
	}
	return true, remaining
}
