package controllers

import (
	"context"
	"testing"

	argoapp "github.com/argoproj-labs/gitops-operator/argocd-operator/api/v1beta1"
	pipelinesv1alpha1 "github.com/redhat-developer/gitops-operator/api/v1alpha1"
	"github.com/redhat-developer/gitops-operator/common"
	aioperator "github.com/redhat-developer/gitops-operator/internal/ai/operator"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

func TestAgenticTriggerUsesArgoCDNamespace(t *testing.T) {
	t.Setenv("ARGOCD_OPERATOR_NAMESPACE", "openshift-gitops-operator")
	t.Setenv("AGENTIC_RUN_NAMESPACE", "openshift-lightspeed")
	t.Setenv("AGENTIC_RUN_COOLDOWN", "10m")
	t.Setenv("AGENTIC_AUTO_DIAGNOSIS_GRACE_PERIOD", "10m")
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
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(owner, argoCD).Build()
	r := &AgenticTriggerReconciler{Client: cl, Scheme: scheme, EnableAIFeatures: true}
	request := reconcile.Request{NamespacedName: client.ObjectKeyFromObject(owner)}
	ctx := context.Background()
	if _, err := r.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	deployment := &appsv1.Deployment{}
	key := client.ObjectKey{Name: aioperator.ServiceName, Namespace: serviceNamespace}
	if err := cl.Get(ctx, key, deployment); err != nil {
		t.Fatalf("trigger missing from ArgoCD namespace: %v", err)
	}
	if err := cl.Get(ctx, client.ObjectKey{Name: aioperator.ServiceName, Namespace: "openshift-gitops-operator"}, &appsv1.Deployment{}); !apierrors.IsNotFound(err) {
		t.Fatalf("unexpected trigger in operator namespace: %v", err)
	}
	argoCD.Spec.AI.Troubleshooting = false
	if err := cl.Update(ctx, argoCD); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	if err := cl.Get(ctx, key, deployment); !apierrors.IsNotFound(err) {
		t.Fatalf("trigger retained after ArgoCD opt-out: %v", err)
	}
}
