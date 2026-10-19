package controllers

import (
	"context"

	argoapp "github.com/argoproj-labs/gitops-operator/argocd-operator/api/v1beta1"
	pipelinesv1alpha1 "github.com/redhat-developer/gitops-operator/api/v1alpha1"
	aioperator "github.com/redhat-developer/gitops-operator/internal/ai/operator"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

var _ reconcile.Reconciler = &AgenticTriggerReconciler{}

// AgenticTriggerReconciler provisions and maintains the agentic-trigger
// Deployment and its supporting resources (RBAC, Service, token Secret, CA
// ConfigMap) in the openshift-gitops namespace.
type AgenticTriggerReconciler struct {
	Client           client.Client
	Scheme           *runtime.Scheme
	EnableAIFeatures bool
}

// SetupWithManager registers the controller. It watches GitopsService as its
// primary resource and reacts to ArgoCD opt-in changes and updates to the token
// Secret and CA ConfigMap.
func (r *AgenticTriggerReconciler) SetupWithManager(mgr ctrl.Manager) error {
	deps := aioperator.DependencyRequests(serviceNamespace)
	return ctrl.NewControllerManagedBy(mgr).
		For(&pipelinesv1alpha1.GitopsService{}).
		Watches(&argoapp.ArgoCD{}, handler.EnqueueRequestsFromMapFunc(deps)).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(deps)).
		Watches(&corev1.ConfigMap{}, handler.EnqueueRequestsFromMapFunc(deps)).
		Complete(r)
}

func (r *AgenticTriggerReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	log := logf.Log.WithName("controller_agentic_trigger").WithValues(
		"Request.Namespace", req.Namespace, "Request.Name", req.Name,
	)

	instance := &pipelinesv1alpha1.GitopsService{}
	if err := r.Client.Get(ctx, types.NamespacedName{Name: req.Name}, instance); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}

	return aioperator.ReconcileAgenticTriggerService(r.Client, r.Scheme, r.EnableAIFeatures, serviceNamespace, req.NamespacedName, instance, log)
}
