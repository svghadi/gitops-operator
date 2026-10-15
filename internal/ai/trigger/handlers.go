package trigger

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/redhat-developer/gitops-operator/internal/agentic"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func (s *TriggerService) HandleDiagnose(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	logger := s.logger.WithName("diagnose")

	token := extractBearerToken(r.Header.Get("Authorization"))
	if token == "" {
		writeJSON(w, http.StatusUnauthorized, DiagnoseResponse{Accepted: false, Reason: "missing authorization"})
		return
	}
	authResult := authenticateCaller(ctx, s.kubeClient, token, s.config.ArgocdNamespace, logger)
	if authResult == nil {
		writeJSON(w, http.StatusInternalServerError, DiagnoseResponse{Accepted: false, Reason: "authentication service unavailable"})
		return
	}
	if !authResult.Authenticated {
		writeJSON(w, http.StatusUnauthorized, DiagnoseResponse{Accepted: false, Reason: "invalid or expired token"})
		return
	}
	if !authResult.Authorized {
		writeJSON(w, http.StatusForbidden, DiagnoseResponse{Accepted: false, Reason: "caller not authorized"})
		return
	}
	var req DiagnoseRequest
	if r.Body != nil {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, DiagnoseResponse{Accepted: false, Reason: "invalid request body"})
			return
		}
	}

	name := req.Application
	namespace := req.ApplicationNamespace
	if name == "" || namespace == "" {
		writeJSON(w, http.StatusBadRequest, DiagnoseResponse{Accepted: false, Reason: "application and applicationNamespace are required"})
		return
	}

	logger.Info("diagnose request received",
		"app", namespace+"/"+name,
		"trigger", triggerAutomatic,
	)

	if s.queue == nil || s.queue.ShuttingDown() {
		writeJSON(w, http.StatusServiceUnavailable, DiagnoseResponse{Reason: "diagnosis queue unavailable"})
		return
	}
	s.enqueueApplication(namespace, name)
	writeJSON(w, http.StatusAccepted, DiagnoseResponse{Accepted: true, Reason: "diagnosis queued"})
}

// HandleDiagnosis is the handler for the diagnosis endpoint.
// It returns the latest diagnosis for an application.
func (s *TriggerService) HandleDiagnosis(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	namespace := r.PathValue("namespace")
	name := r.PathValue("name")
	logger := s.logger.WithValues("app", namespace+"/"+name)

	token := extractBearerToken(r.Header.Get("Authorization"))
	if token == "" {
		writeJSON(w, http.StatusUnauthorized, DiagnosisResponse{Status: "error"})
		return
	}

	authResult := authenticateCaller(ctx, s.kubeClient, token, s.config.ArgocdNamespace, logger)
	if authResult == nil {
		writeJSON(w, http.StatusInternalServerError, DiagnosisResponse{Status: "error"})
		return
	}
	if !authResult.Authenticated {
		writeJSON(w, http.StatusUnauthorized, DiagnosisResponse{Status: "error"})
		return
	}
	if !authResult.Authorized {
		writeJSON(w, http.StatusForbidden, DiagnosisResponse{Status: "error"})
		return
	}

	app, err := s.getApplication(ctx, namespace, name)
	if err != nil {
		writeJSON(w, http.StatusNotFound, DiagnosisResponse{Status: "error"})
		return
	}

	appUID := string(app.GetUID())
	syncStatus, _, _ := unstructured.NestedString(app.Object, "status", "sync", "status")
	healthStatus, _, _ := unstructured.NestedString(app.Object, "status", "health", "status")
	revision := agentic.Revision(app)

	// Build the application information
	appInfo := &ApplicationInfo{
		Name:         name,
		Namespace:    namespace,
		UID:          appUID,
		SyncStatus:   syncStatus,
		HealthStatus: healthStatus,
		Revision:     revision,
	}

	var latestRun *unstructured.Unstructured
	runs, err := s.listAgenticRuns(ctx, name, namespace, appUID)
	if err != nil {
		logger.Error(err, "failed to list AgenticRuns")
		writeJSON(w, http.StatusInternalServerError, DiagnosisResponse{Status: "error"})
		return
	}
	if len(runs) > 0 {
		latestRun = runs[0]
	}

	if latestRun == nil {
		writeJSON(w, http.StatusOK, DiagnosisResponse{
			Status:      "idle",
			Application: appInfo,
		})
		return
	}

	resp := s.buildDiagnosisFromRun(ctx, latestRun, name, namespace, appUID, syncStatus, healthStatus, revision)
	writeJSON(w, http.StatusOK, resp)
}

func (s *TriggerService) getApplication(ctx context.Context, namespace, name string) (*unstructured.Unstructured, error) {
	return s.dynClient.Resource(applicationGVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
}

func (s *TriggerService) createAgenticRun(ctx context.Context, run *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	ns := run.GetNamespace()
	return s.dynClient.Resource(agenticRunGVR).Namespace(ns).Create(ctx, run, metav1.CreateOptions{})
}

func (s *TriggerService) getAgenticRun(ctx context.Context, namespace, name string) (*unstructured.Unstructured, error) {
	return s.dynClient.Resource(agenticRunGVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
}

func (s *TriggerService) listAgenticRuns(ctx context.Context, appName, appNamespace, appUID string) ([]*unstructured.Unstructured, error) {
	labelSelector := fmt.Sprintf(
		"gitops.openshift.io/application-namespace=%s,gitops.openshift.io/application-uid=%s",
		appNamespace, appUID,
	)

	result, err := s.dynClient.Resource(agenticRunGVR).Namespace(s.config.RunNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: labelSelector,
	})
	if err != nil {
		return nil, err
	}

	runs := make([]*unstructured.Unstructured, 0, len(result.Items))
	for i := range result.Items {
		if result.Items[i].GetLabels()[agentic.ManagedBy] == agentic.Manager && runBelongsToApplication(&result.Items[i], appName, appNamespace, appUID) {
			runs = append(runs, &result.Items[i])
		}
	}

	sort.Slice(runs, func(i, j int) bool {
		ti := runs[i].GetAnnotations()["gitops.openshift.io/triggered-at"]
		tj := runs[j].GetAnnotations()["gitops.openshift.io/triggered-at"]
		first, firstErr := time.Parse(time.RFC3339Nano, ti)
		second, secondErr := time.Parse(time.RFC3339Nano, tj)
		if firstErr == nil && secondErr == nil && !first.Equal(second) {
			return first.After(second)
		}
		return runs[i].GetCreationTimestamp().After(runs[j].GetCreationTimestamp().Time)
	})

	return runs, nil
}

func (s *TriggerService) getAgenticResult(ctx context.Context, runName string) (*unstructured.Unstructured, error) {
	return s.dynClient.Resource(agenticResultGVR).Namespace(s.config.RunNamespace).Get(ctx, runName, metav1.GetOptions{})
}

type AgenticRunParams struct {
	AppName            string
	AppNamespace       string
	AppUID             string
	SyncStatus         string
	HealthStatus       string
	Revision           string
	TargetNS           string
	Agent              string
	SkillsImage        string
	Conditions         []AppCondition
	UnhealthyResources []UnhealthyResource
}

func buildAgenticRunObject(name, namespace string, p AgenticRunParams) *unstructured.Unstructured {
	requestText := fmt.Sprintf(
		"Diagnose the following Argo CD Application running on an OpenShift cluster.\n"+
			"The Argo CD instance is installed and managed by the Red Hat OpenShift GitOps Operator.\n"+
			"Treat this environment as Red Hat OpenShift GitOps, not as a standalone upstream Argo CD installation.\n\n"+
			"Application:\n"+
			"  Name: %s\n"+
			"  Namespace: %s\n\n"+
			"Current status:\n"+
			"  Sync Status: %s\n"+
			"  Health Status: %s\n"+
			"%s\n"+
			"Destination:\n"+
			"  Namespace: %s\n\n"+
			"Investigate the reason for the application's current sync and health status.\n"+
			"Inspect the Argo CD Application, its managed Kubernetes/OpenShift resources,\n"+
			"and relevant OpenShift GitOps resources to determine:\n"+
			"- the root cause\n"+
			"- affected resources\n"+
			"- relevant errors, events, or conditions\n"+
			"- recommended remediation\n\n"+
			"When recommending remediation:\n"+
			"- follow Red Hat OpenShift GitOps conventions\n"+
			"- account for the fact that the Argo CD control plane is managed by the OpenShift GitOps Operator\n"+
			"- do not recommend directly modifying Operator-managed Argo CD Deployments,\n"+
			"  StatefulSets, Services, or other generated control-plane resources\n"+
			"- when the problem is Argo CD instance configuration, prefer the Operator-supported\n"+
			"  configuration through the ArgoCD custom resource or other OpenShift GitOps APIs\n"+
			"- when the problem is in an Application-managed workload, identify the appropriate\n"+
			"  Git/declarative configuration change rather than treating it as an Argo CD\n"+
			"  control-plane configuration problem\n"+
			"- do not assume an upstream Argo CD remediation is appropriate if OpenShift GitOps\n"+
			"  manages that configuration differently\n\n"+
			"Do not make changes to the cluster.\n"+
			"Only diagnose the issue and recommend the supported remediation.\n",
		p.AppName,
		p.AppNamespace,
		p.SyncStatus,
		p.HealthStatus,
		buildConditionsContext(&p),
		p.TargetNS,
	)

	run := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "agentic.openshift.io/v1alpha1",
			"kind":       "AgenticRun",
			"metadata": map[string]any{
				"name":      name,
				"namespace": namespace,
				"labels": map[string]any{
					"gitops.openshift.io/application-namespace": p.AppNamespace,
					"gitops.openshift.io/application-uid":       p.AppUID,
					"gitops.openshift.io/managed-by":            "agentic-trigger",
				},
				"annotations": map[string]any{
					agentic.AppName:                             p.AppName,
					"gitops.openshift.io/trigger-type":          triggerAutomatic,
					"gitops.openshift.io/trigger-sync-status":   p.SyncStatus,
					"gitops.openshift.io/trigger-health-status": p.HealthStatus,
					"gitops.openshift.io/trigger-revision":      p.Revision,
				},
			},
			"spec": buildAgenticRunSpec(requestText, p),
		},
	}

	return run
}

func buildAgenticRunSpec(requestText string, p AgenticRunParams) map[string]any {
	spec := map[string]any{
		"request": requestText,
		"analysis": map[string]any{
			"agent": p.Agent,
		},
		"targetNamespaces": []any{p.TargetNS, p.AppNamespace},
	}
	if tools := buildToolsSpec(p.SkillsImage); len(tools) > 0 {
		spec["tools"] = tools
	}
	return spec
}

func buildToolsSpec(skillsImage string) map[string]any {
	tools := map[string]any{}
	if skillsImage != "" {
		tools["skills"] = []any{
			map[string]any{
				"image": skillsImage,
				"paths": []any{
					"/skills/argocd-diagnostics",
					"/skills/argocd-remediation",
				},
			},
		}
	}
	return tools
}

func extractConditions(app *unstructured.Unstructured) []AppCondition {
	raw, found, _ := unstructured.NestedSlice(app.Object, "status", "conditions")
	if !found {
		return nil
	}
	var conditions []AppCondition
	for _, item := range raw {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		t, _, _ := unstructured.NestedString(m, "type")
		msg, _, _ := unstructured.NestedString(m, "message")
		if t != "" {
			conditions = append(conditions, AppCondition{Type: t, Message: msg})
		}
	}
	return conditions
}

func extractUnhealthyResources(app *unstructured.Unstructured) []UnhealthyResource {
	raw, found, _ := unstructured.NestedSlice(app.Object, "status", "resources")
	if !found {
		return nil
	}
	var resources []UnhealthyResource
	for _, item := range raw {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		health, _, _ := unstructured.NestedString(m, "health", "status")
		sync, _, _ := unstructured.NestedString(m, "status")
		if health == "Degraded" || health == "Missing" || health == "Unknown" || sync == "OutOfSync" {
			msg, _, _ := unstructured.NestedString(m, "health", "message")
			resources = append(resources, UnhealthyResource{
				Group:         stringFromMap(m, "group"),
				Kind:          stringFromMap(m, "kind"),
				Namespace:     stringFromMap(m, "namespace"),
				Name:          stringFromMap(m, "name"),
				HealthStatus:  health,
				SyncStatus:    sync,
				HealthMessage: msg,
			})
		}
	}
	return resources
}

func stringFromMap(m map[string]any, key string) string {
	v, _ := m[key].(string)
	return v
}

// Application names live in annotations because they may exceed label limits.
func runBelongsToApplication(run *unstructured.Unstructured, appName, appNamespace, appUID string) bool {
	labels := run.GetLabels()
	return run.GetAnnotations()[agentic.AppName] == appName &&
		labels[agentic.AppNamespace] == appNamespace &&
		labels[agentic.AppUID] == appUID
}

func buildConditionsContext(p *AgenticRunParams) string {
	var sb strings.Builder

	// ArgoCD conditions (ComparisonError, SyncError, etc.)
	if len(p.Conditions) > 0 {
		sb.WriteString("\nReported conditions:\n")
		for _, c := range p.Conditions {
			sb.WriteString(fmt.Sprintf("  - [%s] %s\n", c.Type, c.Message))
		}
	}

	// Unhealthy/OutOfSync resources from status.resources[]
	if len(p.UnhealthyResources) > 0 {
		sb.WriteString("\nUnhealthy or OutOfSync resources reported by ArgoCD:\n")
		for _, r := range p.UnhealthyResources {
			sb.WriteString(fmt.Sprintf("  - %s/%s (%s) in %s — health: %s, sync: %s\n",
				r.Kind, r.Name, r.Group, r.Namespace, r.HealthStatus, r.SyncStatus))
			if r.HealthMessage != "" {
				sb.WriteString(fmt.Sprintf("    message: %s\n", r.HealthMessage))
			}
		}
	}

	return sb.String()
}

func (s *TriggerService) buildDiagnosisFromRun(ctx context.Context, run *unstructured.Unstructured, appName, appNS, appUID, syncStatus, healthStatus, revision string) DiagnosisResponse {
	annotations := run.GetAnnotations()
	triggerType := annotations["gitops.openshift.io/trigger-type"]
	triggeredAt := annotations["gitops.openshift.io/triggered-at"]
	triggerRevision := annotations["gitops.openshift.io/trigger-revision"]
	triggerHealth := annotations["gitops.openshift.io/trigger-health-status"]

	status := diagnosisStateForRun(run)

	stale := false
	if triggerRevision != revision || triggerHealth != healthStatus {
		stale = true
	}

	resp := DiagnosisResponse{
		Status:  status,
		Trigger: triggerType,
		Run: &RunInfo{
			Name:      run.GetName(),
			Namespace: run.GetNamespace(),
			StartedAt: triggeredAt,
		},
		Application: &ApplicationInfo{
			Name:         appName,
			Namespace:    appNS,
			UID:          appUID,
			SyncStatus:   syncStatus,
			HealthStatus: healthStatus,
			Revision:     revision,
		},
		Stale: stale,
	}

	if status == "failed" {
		conditions, _, _ := unstructured.NestedSlice(run.Object, "status", "conditions")
		for _, raw := range conditions {
			c, _ := raw.(map[string]any)
			if stringFromMap(c, "type") == "Analyzed" && stringFromMap(c, "status") == "False" {
				resp.Summary = stringFromMap(c, "message")
			}
		}
	}
	if status == "completed" || status == "failed" {
		refs, _, _ := unstructured.NestedSlice(run.Object, "status", "steps", "analysis", "results")
		if len(refs) == 0 {
			if status == "completed" {
				resp.Status = "running"
			}
			return resp
		}
		ref, _ := refs[len(refs)-1].(map[string]any)
		result, err := s.getAgenticResult(ctx, stringFromMap(ref, "name"))
		if err != nil {
			if status == "completed" {
				resp.Status = "running"
			}
			return resp
		}
		parent, _, _ := unstructured.NestedString(result.Object, "spec", "agenticRunName")
		if parent != run.GetName() {
			resp.Status = "running"
			return resp
		}
		if status == "failed" {
			if failure, _, _ := unstructured.NestedString(result.Object, "status", "failureReason"); failure != "" {
				resp.Summary = failure
			}
		} else {
			resp.Summary, _, _ = unstructured.NestedString(result.Object, "status", "diagnosis", "summary")
		}
		conditions, _, _ := unstructured.NestedSlice(result.Object, "status", "conditions")
		for _, raw := range conditions {
			c, _ := raw.(map[string]any)
			if stringFromMap(c, "type") == "Completed" && stringFromMap(c, "status") == "True" {
				resp.Run.CompletedAt = stringFromMap(c, "lastTransitionTime")
			}
		}
		options, _, _ := unstructured.NestedSlice(result.Object, "status", "options")
		for i, raw := range options {
			option, _ := raw.(map[string]any)
			summary, _, _ := unstructured.NestedString(option, "diagnosis", "summary")
			cause, _, _ := unstructured.NestedString(option, "diagnosis", "rootCause")
			if i == 0 && resp.Summary == "" {
				resp.Summary = summary
			}
			remediation, _, _ := unstructured.NestedString(option, "remediationPlan", "description")
			actions, _, _ := unstructured.NestedSlice(option, "remediationPlan", "actions")
			for _, raw := range actions {
				action, _ := raw.(map[string]any)
				remediation += "\n" + stringFromMap(action, "description")
				if command := stringFromMap(action, "command"); command != "" {
					remediation += "\n```sh\n" + command + "\n```"
				}
			}
			resp.Suggestions = append(resp.Suggestions, DiagnosisSuggestion{ID: fmt.Sprintf("%s-%d", result.GetName(), i+1), Severity: "info", Title: stringFromMap(option, "title"), Description: strings.TrimSpace(cause + "\n" + summary), Remediation: strings.TrimSpace(remediation)})
		}
	}

	return resp
}

// diagnosisStateForRun reports the trigger's diagnosis state, not the
// AgenticRun's derived phase. Analysis completes this service's analysis-only
// runs even though the upstream phase may be Proposed or NoActionRequired.
func diagnosisStateForRun(run *unstructured.Unstructured) string {
	conditions, _, _ := unstructured.NestedSlice(run.Object, "status", "conditions")
	byType := map[string]map[string]any{}
	for _, raw := range conditions {
		c, _ := raw.(map[string]any)
		byType[stringFromMap(c, "type")] = c
	}
	// Terminal failures win even when earlier successful conditions remain.
	for _, key := range []string{"EmergencyStopped", "Denied", "Escalated"} {
		if stringFromMap(byType[key], "status") == "True" {
			return "failed"
		}
	}
	// Conditions accumulate. A completed diagnosis must beat an older
	// execution condition, regardless of their order in the status slice.
	for _, key := range []string{"Verified", "Analyzed"} {
		if stringFromMap(byType[key], "status") == "True" {
			return "completed"
		}
	}
	for _, key := range []string{"Escalated", "Verified", "Executed", "Analyzed"} {
		c, exists := byType[key]
		if !exists {
			continue
		}
		switch stringFromMap(c, "status") {
		case "True":
			return "running" // Executed=True: later steps may still be active.
		case "False":
			if key == "Verified" && stringFromMap(c, "reason") == "RetryingExecution" {
				return "running"
			}
			return "failed"
		default:
			return "running"
		}
	}
	return "running" // Unknown or absent conditions cannot justify another run.
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
