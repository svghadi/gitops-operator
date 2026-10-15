package controllers

import (
	"context"
	"fmt"
	"strings"
	"testing"

	argoapp "github.com/argoproj-labs/gitops-operator/argocd-operator/api/v1beta1"
	"github.com/go-logr/logr"
	pipelinesv1alpha1 "github.com/redhat-developer/gitops-operator/api/v1alpha1"
	"github.com/redhat-developer/gitops-operator/common"
	"github.com/redhat-developer/gitops-operator/internal/ai/notifications"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func agenticControllerFixture(t *testing.T) (*ReconcileGitopsService, *pipelinesv1alpha1.GitopsService) {
	t.Helper()
	t.Setenv(agenticTriggerTLSEnv, "true")
	t.Setenv("AGENTIC_RUN_NAMESPACE", "lightspeed")
	t.Setenv("AGENTIC_RUN_COOLDOWN", "10m")
	t.Setenv("AGENTIC_AUTO_DIAGNOSIS_GRACE_PERIOD", "90s")
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, appsv1.AddToScheme, rbacv1.AddToScheme, pipelinesv1alpha1.AddToScheme, argoapp.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	owner := &pipelinesv1alpha1.GitopsService{ObjectMeta: metav1.ObjectMeta{Name: "cluster", UID: "owner"}}
	argoCD := &argoapp.ArgoCD{
		ObjectMeta: metav1.ObjectMeta{Name: common.ArgoCDInstanceName, Namespace: serviceNamespace},
		Spec:       argoapp.ArgoCDSpec{AI: &argoapp.ArgoCDAISpec{Troubleshooting: true}},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(owner, argoCD).Build()
	r := &ReconcileGitopsService{Client: c, Scheme: scheme, EnableAIFeatures: true}
	return r, owner
}

func TestAgenticSetupCreatesResourcesAndDisablesCleanly(t *testing.T) {
	r, owner := agenticControllerFixture(t)
	ctx := context.Background()
	key := types.NamespacedName{Namespace: "gitops"}

	// First reconcile: should create all owned K8s resources (no requeue needed).
	result, err := r.reconcileAgenticTriggerService(key, owner, logr.Discard())
	if err != nil {
		t.Fatal(err)
	}
	if result.RequeueAfter != 0 {
		t.Fatalf("unexpected requeue: %v", result)
	}

	// Token secret should be created and owned.
	token := &corev1.Secret{}
	if err := r.Client.Get(ctx, types.NamespacedName{Name: notifications.TokenSecretName, Namespace: "gitops"}, token); err != nil {
		t.Fatal(err)
	}
	if !metav1.IsControlledBy(token, owner) {
		t.Fatal("token has no owner")
	}

	// Disable at the ArgoCD CR level.
	argoCD := &argoapp.ArgoCD{}
	_ = r.Client.Get(ctx, types.NamespacedName{Name: common.ArgoCDInstanceName, Namespace: serviceNamespace}, argoCD)
	argoCD.Spec.AI.Troubleshooting = false
	_ = r.Client.Update(ctx, argoCD)

	if _, err = r.reconcileAgenticTriggerService(key, owner, logr.Discard()); err != nil {
		t.Fatal(err)
	}
	// Deployment and token secret should be deleted.
	if err = r.Client.Get(ctx, types.NamespacedName{Name: agenticTriggerServiceName, Namespace: "gitops"}, &appsv1.Deployment{}); !apierrors.IsNotFound(err) {
		t.Fatalf("deployment retained: %v", err)
	}
	if err = r.Client.Get(ctx, types.NamespacedName{Name: notifications.TokenSecretName, Namespace: "gitops"}, &corev1.Secret{}); !apierrors.IsNotFound(err) {
		t.Fatalf("token secret retained: %v", err)
	}
}

// NotificationsConfiguration patching, token propagation, and CA propagation are
// tested in the argocd-operator's ai_notifications_test.go; this package only
// verifies that the gitops-operator creates and deletes the infrastructure objects.

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

func TestAgenticOperatorFlagGatesIntegration(t *testing.T) {
	r, owner := agenticControllerFixture(t)
	r.EnableAIFeatures = false
	key := types.NamespacedName{Namespace: "gitops"}
	if _, err := r.reconcileAgenticTriggerService(key, owner, logr.Discard()); err != nil {
		t.Fatal(err)
	}
	if err := r.Client.Get(context.Background(), types.NamespacedName{Name: agenticTriggerServiceName, Namespace: "gitops"}, &appsv1.Deployment{}); !apierrors.IsNotFound(err) {
		t.Fatal("deployment created despite flag being off")
	}
}

func TestAgenticArgoCDOptInGatesIntegration(t *testing.T) {
	r, owner := agenticControllerFixture(t)
	argoCD := &argoapp.ArgoCD{}
	_ = r.Client.Get(context.Background(), types.NamespacedName{Name: common.ArgoCDInstanceName, Namespace: serviceNamespace}, argoCD)
	argoCD.Spec.AI.Troubleshooting = false
	_ = r.Client.Update(context.Background(), argoCD)
	key := types.NamespacedName{Namespace: "gitops"}
	if _, err := r.reconcileAgenticTriggerService(key, owner, logr.Discard()); err != nil {
		t.Fatal(err)
	}
	if err := r.Client.Get(context.Background(), types.NamespacedName{Name: agenticTriggerServiceName, Namespace: "gitops"}, &appsv1.Deployment{}); !apierrors.IsNotFound(err) {
		t.Fatal("deployment created despite ArgoCD opt-in being off")
	}
}

func TestAgenticUnownedDeploymentIsPreserved(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		t.Run(fmt.Sprintf("enabled=%v", enabled), func(t *testing.T) {
			r, owner := agenticControllerFixture(t)
			r.EnableAIFeatures = enabled
			if !enabled {
				argoCD := &argoapp.ArgoCD{}
				_ = r.Client.Get(context.Background(), types.NamespacedName{Name: common.ArgoCDInstanceName, Namespace: serviceNamespace}, argoCD)
				argoCD.Spec.AI.Troubleshooting = false
				_ = r.Client.Update(context.Background(), argoCD)
			}
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

func TestAgenticSubscriptionMergeInNotifications(t *testing.T) {
	// Verify the subscription value produced by notifications.MergeSubscription
	// (the canonical implementation) is preserved when AI is enabled then disabled.
	original := "- recipients: [slack:ops, agentic-trigger]\n  triggers: [on-sync-failed, on-agentic-diagnose]\n  selector: team=platform\n"
	enabled, err := notifications.MergeSubscription(original, true)
	if err != nil {
		t.Fatal(err)
	}
	disabled, err := notifications.MergeSubscription(enabled, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(disabled, "slack:ops") || !strings.Contains(disabled, "on-sync-failed") {
		t.Fatal("user subscription entries lost after disable")
	}
	if strings.Contains(disabled, "agentic-trigger") || strings.Contains(disabled, "on-agentic-diagnose") {
		t.Fatal("AI entries still present after disable")
	}
}
