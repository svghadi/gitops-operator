package controllers

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	argov1alpha1 "github.com/argoproj-labs/gitops-operator/argocd-operator/api/v1alpha1"
	"github.com/argoproj/notifications-engine/pkg/triggers"
	"github.com/go-logr/logr"
	pipelinesv1alpha1 "github.com/redhat-developer/gitops-operator/api/v1alpha1"
	"github.com/redhat-developer/gitops-operator/internal/agentic"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"
)

func agenticControllerFixture(t *testing.T) (*ReconcileGitopsService, *pipelinesv1alpha1.GitopsService) {
	t.Helper()
	t.Setenv(agenticIntegrationEnabledEnv, "false")
	t.Setenv(agenticTriggerTLSEnv, "true")
	t.Setenv("AGENTIC_RUN_NAMESPACE", "lightspeed")
	t.Setenv("AGENTIC_RUN_COOLDOWN", "10m")
	t.Setenv("AGENTIC_AUTO_DIAGNOSIS_GRACE_PERIOD", "90s")
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, appsv1.AddToScheme, rbacv1.AddToScheme, pipelinesv1alpha1.AddToScheme, argov1alpha1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	owner := &pipelinesv1alpha1.GitopsService{ObjectMeta: metav1.ObjectMeta{Name: "cluster", UID: "owner", Annotations: map[string]string{agentic.EnabledAnnotation: "true"}}}
	nc := &argov1alpha1.NotificationsConfiguration{ObjectMeta: metav1.ObjectMeta{Name: defaultNotificationsConfigurationName, Namespace: "gitops"}}
	nc.Spec.Subscriptions = map[string]string{"subscriptions": "- recipients: [slack:ops]\n  triggers: [on-sync-failed]\n  selector: team=platform\n"}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(owner, nc, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: argocdNotificationsSecretName, Namespace: "gitops"}, Data: map[string][]byte{"user-token": []byte("keep")}}, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: argocdTLSCertsConfigMapName, Namespace: "gitops"}, Data: map[string]string{"git.example": "keep"}}).Build()
	return &ReconcileGitopsService{Client: c, Scheme: scheme}, owner
}

func TestAgenticSetupWaitsForDependenciesAndDisablesCleanly(t *testing.T) {
	r, owner := agenticControllerFixture(t)
	ctx := context.Background()
	key := types.NamespacedName{Namespace: "gitops"}
	result, err := r.reconcileAgenticTriggerService(key, owner, logr.Discard())
	if err != nil {
		t.Fatal(err)
	}
	if result.RequeueAfter != 5*time.Second {
		t.Fatalf("requeue=%v", result)
	}
	token := &corev1.Secret{}
	if err := r.Client.Get(ctx, types.NamespacedName{Name: agenticNotifTokenSecretName, Namespace: "gitops"}, token); err != nil {
		t.Fatal(err)
	}
	if !metav1.IsControlledBy(token, owner) {
		t.Fatal("token has no owner")
	}
	token.Data = map[string][]byte{"token": []byte("populated")}
	if err := r.Client.Update(ctx, token); err != nil {
		t.Fatal(err)
	}
	ca := &corev1.ConfigMap{}
	if err := r.Client.Get(ctx, types.NamespacedName{Name: agenticTriggerCAConfigMapName, Namespace: "gitops"}, ca); err != nil {
		t.Fatal(err)
	}
	ca.Data = map[string]string{serviceServingCABundleKey: "ca"}
	if err := r.Client.Update(ctx, ca); err != nil {
		t.Fatal(err)
	}
	result, err = r.reconcileAgenticTriggerService(key, owner, logr.Discard())
	if err != nil || result.RequeueAfter != 0 {
		t.Fatalf("ready=%v err=%v", result, err)
	}
	notif := &corev1.Secret{}
	_ = r.Client.Get(ctx, types.NamespacedName{Name: argocdNotificationsSecretName, Namespace: "gitops"}, notif)
	if string(notif.Data[agenticTriggerTokenKey]) != "populated" {
		t.Fatal("token not copied")
	}
	nc := &argov1alpha1.NotificationsConfiguration{}
	_ = r.Client.Get(ctx, types.NamespacedName{Name: defaultNotificationsConfigurationName, Namespace: "gitops"}, nc)
	if !strings.Contains(nc.Spec.Subscriptions["subscriptions"], "slack:ops") {
		t.Fatal("user subscription lost")
	}
	owner.Annotations[agentic.EnabledAnnotation] = "false"
	if _, err = r.reconcileAgenticTriggerService(key, owner, logr.Discard()); err != nil {
		t.Fatal(err)
	}
	if err = r.Client.Get(ctx, types.NamespacedName{Name: agenticTriggerServiceName, Namespace: "gitops"}, &appsv1.Deployment{}); !apierrors.IsNotFound(err) {
		t.Fatalf("deployment retained: %v", err)
	}
	_ = r.Client.Get(ctx, types.NamespacedName{Name: argocdNotificationsSecretName, Namespace: "gitops"}, notif)
	if _, ok := notif.Data[agenticTriggerTokenKey]; ok {
		t.Fatal("integration credential retained")
	}
	if string(notif.Data["user-token"]) != "keep" {
		t.Fatal("user credential lost")
	}
	_ = r.Client.Get(ctx, types.NamespacedName{Name: defaultNotificationsConfigurationName, Namespace: "gitops"}, nc)
	if strings.Contains(nc.Spec.Subscriptions["subscriptions"], "agentic-trigger") || !strings.Contains(nc.Spec.Subscriptions["subscriptions"], "slack:ops") {
		t.Fatal("incorrect subscription cleanup")
	}
}

func TestAgenticTLSReconciliationPreservesServiceAddress(t *testing.T) {
	r, owner := agenticControllerFixture(t)
	ctx := context.Background()
	key := types.NamespacedName{Namespace: "gitops"}
	for _, tls := range []string{"false", "true", "false"} {
		t.Setenv(agenticTriggerTLSEnv, tls)
		if _, err := r.reconcileAgenticTriggerService(key, owner, logr.Discard()); err != nil {
			t.Fatal(err)
		}
		svc := &corev1.Service{}
		_ = r.Client.Get(ctx, types.NamespacedName{Name: agenticTriggerServiceName, Namespace: "gitops"}, svc)
		if svc.Spec.ClusterIP == "" {
			svc.Spec.ClusterIP = "10.0.0.1"
			if err := r.Client.Update(ctx, svc); err != nil {
				t.Fatal(err)
			}
		} else if svc.Spec.ClusterIP != "10.0.0.1" {
			t.Fatal("clusterIP overwritten")
		}
		deployment := &appsv1.Deployment{}
		_ = r.Client.Get(ctx, types.NamespacedName{Name: agenticTriggerServiceName, Namespace: "gitops"}, deployment)
		port := int32(8080)
		if tls == "true" {
			port = 8443
		}
		if svc.Spec.Ports[0].Port != port || deployment.Spec.Template.Spec.Containers[0].Ports[0].ContainerPort != port {
			t.Fatal("TLS ports disagree")
		}
		if (len(deployment.Spec.Template.Spec.Volumes) > 0) != (tls == "true") {
			t.Fatal("TLS volume mismatch")
		}
	}
}

func TestAgenticAnnotationOverridesEnvironment(t *testing.T) {
	t.Setenv(agenticIntegrationEnabledEnv, "true")
	owner := &pipelinesv1alpha1.GitopsService{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{agentic.EnabledAnnotation: "false"}}}
	if agenticEnabled(owner) {
		t.Fatal("false annotation ignored")
	}
	delete(owner.Annotations, agentic.EnabledAnnotation)
	if !agenticEnabled(owner) {
		t.Fatal("environment ignored")
	}
	t.Setenv(agenticIntegrationEnabledEnv, "false")
	owner.Annotations[agentic.EnabledAnnotation] = "true"
	if !agenticEnabled(owner) {
		t.Fatal("true annotation ignored")
	}
}

func TestAgenticAnnotationOnlyUpdate(t *testing.T) {
	old := &pipelinesv1alpha1.GitopsService{ObjectMeta: metav1.ObjectMeta{Generation: 1}}
	current := old.DeepCopy()
	current.Annotations = map[string]string{agentic.EnabledAnnotation: "true"}
	if !agenticAnnotationsChanged(old, current) {
		t.Fatal("annotation-only enablement ignored")
	}
	current.Annotations[agentic.EnabledAnnotation] = ""
	if !agenticAnnotationsChanged(old, current) {
		t.Fatal("explicit annotation presence ignored")
	}
	current.Annotations = map[string]string{"unrelated": "value"}
	if agenticAnnotationsChanged(old, current) {
		t.Fatal("unrelated annotation triggered agentic update")
	}
}

func TestMergeAgenticSubscriptions(t *testing.T) {
	original := "- recipients: [slack:ops, agentic-trigger]\n  triggers: [on-sync-failed, on-agentic-diagnose]\n  selector: team=platform\n"
	enabled, err := mergeAgenticSubscription(original, true)
	if err != nil {
		t.Fatal(err)
	}
	again, err := mergeAgenticSubscription(enabled, true)
	if err != nil || again != enabled {
		t.Fatal("merge not idempotent")
	}
	disabled, err := mergeAgenticSubscription(enabled, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(disabled, "slack:ops") || !strings.Contains(disabled, "on-sync-failed") || !strings.Contains(disabled, "team=platform") {
		t.Fatal("user configuration lost")
	}
	for _, invalid := range []string{"not: a-list", "- recipients: slack:ops", "[broken"} {
		if _, err := mergeAgenticSubscription(invalid, true); err == nil {
			t.Fatalf("accepted %q", invalid)
		}
	}
}

func TestAgenticNotificationEngineDeliversEarlyHealthAndAllowsRedelivery(t *testing.T) {
	var clauses []triggers.Condition
	if err := yaml.Unmarshal([]byte(agenticNotificationTrigger()), &clauses); err != nil {
		t.Fatal(err)
	}
	engine, err := triggers.NewService(map[string][]triggers.Condition{"diagnose": clauses})
	if err != nil {
		t.Fatal(err)
	}
	created := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	now := created.Add(time.Second) // Before the default diagnosis grace expires.
	app := map[string]any{"metadata": map[string]any{"uid": "app-uid", "creationTimestamp": created.Format(time.RFC3339)}, "status": map[string]any{"health": map[string]any{"status": "Degraded", "lastTransitionTime": created.Format(time.RFC3339)}, "sync": map[string]any{"revisions": []any{"a", "b"}}}}
	vars := map[string]any{"app": app, "time": map[string]any{"Now": func() time.Time { return now }}}
	for i, health := range []string{"Degraded", "Unknown", "Missing", "Progressing", "Healthy", "Suspended"} {
		app["status"].(map[string]any)["health"].(map[string]any)["status"] = health
		result, err := engine.Run("diagnose", vars)
		if err != nil {
			t.Fatal(err)
		}
		if len(result) != len(clauses) || !result[i].Triggered || result[i].OncePer == "" {
			t.Fatalf("%s was not delivered immediately: %+v", health, result)
		}
		for j, clause := range result {
			if j != i && clause.Triggered {
				t.Fatalf("%s also triggered clause %d", health, j)
			}
		}
	}
	app["status"].(map[string]any)["health"].(map[string]any)["status"] = "Degraded"
	first, err := engine.Run("diagnose", vars)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	retry, err := engine.Run("diagnose", vars)
	if err != nil {
		t.Fatal(err)
	}
	if !retry[0].Triggered || retry[0].OncePer == first[0].OncePer {
		t.Fatal("a later notification evaluation cannot redeliver")
	}
}

func TestAgenticMalformedSubscriptionPreserved(t *testing.T) {
	r, owner := agenticControllerFixture(t)
	ctx := context.Background()
	nc := &argov1alpha1.NotificationsConfiguration{}
	key := client.ObjectKey{Name: defaultNotificationsConfigurationName, Namespace: "gitops"}
	_ = r.Client.Get(ctx, key, nc)
	nc.Spec.Subscriptions["subscriptions"] = "[broken"
	_ = r.Client.Update(ctx, nc)
	_, err := r.reconcileAgenticTriggerService(types.NamespacedName{Namespace: "gitops"}, owner, logr.Discard())
	if err == nil {
		t.Fatal("expected malformed subscription error")
	}
	_ = r.Client.Get(ctx, key, nc)
	if nc.Spec.Subscriptions["subscriptions"] != "[broken" {
		t.Fatal("malformed YAML overwritten")
	}
}

func TestAgenticUnownedDeploymentIsPreserved(t *testing.T) {
	for _, enabled := range []string{"true", "false"} {
		t.Run(enabled, func(t *testing.T) {
			r, owner := agenticControllerFixture(t)
			owner.Annotations[agentic.EnabledAnnotation] = enabled
			ctx := context.Background()
			deployment := newAgenticTriggerDeployment("gitops", true)
			if err := r.Client.Create(ctx, deployment); err != nil {
				t.Fatal(err)
			}
			before := deployment.DeepCopy()
			if _, err := r.reconcileAgenticTriggerService(types.NamespacedName{Namespace: "gitops"}, owner, logr.Discard()); err == nil {
				t.Fatal("accepted deployment without integration ownership")
			}
			if err := r.Client.Get(ctx, client.ObjectKeyFromObject(deployment), deployment); err != nil {
				t.Fatal(err)
			}
			if deployment.ResourceVersion != before.ResourceVersion || len(deployment.OwnerReferences) != 0 {
				t.Fatal("unowned deployment was modified")
			}
		})
	}
}

func TestAgenticOwnedResourceRecreated(t *testing.T) {
	r, owner := agenticControllerFixture(t)
	key := types.NamespacedName{Namespace: "gitops"}
	_, err := r.reconcileAgenticTriggerService(key, owner, logr.Discard())
	if err != nil {
		t.Fatal(err)
	}
	obj := newAgenticTriggerServiceAccount("gitops")
	if err := r.Client.Delete(context.Background(), obj); err != nil {
		t.Fatal(err)
	}
	_, err = r.reconcileAgenticTriggerService(key, owner, logr.Discard())
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Client.Get(context.Background(), client.ObjectKeyFromObject(obj), obj); err != nil {
		t.Fatal(fmt.Errorf("not recreated: %w", err))
	}
}
