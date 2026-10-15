package controllers

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"strings"
	"time"

	argoapp "github.com/argoproj-labs/gitops-operator/argocd-operator/api/v1beta1"
	"github.com/go-logr/logr"
	pipelinesv1alpha1 "github.com/redhat-developer/gitops-operator/api/v1alpha1"
	"github.com/redhat-developer/gitops-operator/common"
	"github.com/redhat-developer/gitops-operator/internal/agentic"
	"github.com/redhat-developer/gitops-operator/internal/ai/notifications"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const serviceServingCAAnnotation = "service.beta.openshift.io/inject-cabundle"

// agenticDependencyRequests enqueues a GitopsService reconcile when any of the
// agentic-trigger supporting resources change (token Secret, CA ConfigMap).
func (r *ReconcileGitopsService) agenticDependencyRequests(ctx context.Context, obj client.Object) []reconcile.Request {
	ns, err := GetBackendNamespace(r.Client)
	if err != nil || obj.GetNamespace() != ns {
		return nil
	}
	switch obj.GetName() {
	case notifications.TokenSecretName, notifications.CAConfigMap:
		return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: serviceName}}}
	}
	return nil
}

// isAIEnabled returns true when the operator-level flag is set AND the managed
// ArgoCD instance has .spec.ai.troubleshooting enabled.
func (r *ReconcileGitopsService) isAIEnabled(ctx context.Context) bool {
	if !r.EnableAIFeatures {
		return false
	}
	argoCD := &argoapp.ArgoCD{}
	if err := r.Client.Get(ctx, types.NamespacedName{
		Name:      common.ArgoCDInstanceName,
		Namespace: serviceNamespace,
	}, argoCD); err != nil {
		return false
	}
	return argoCD.Spec.AI != nil && argoCD.Spec.AI.Troubleshooting
}

func (r *ReconcileGitopsService) reconcileAgenticTriggerService(key types.NamespacedName, instance *pipelinesv1alpha1.GitopsService, logger logr.Logger) (reconcile.Result, error) {
	ctx := context.TODO()
	ns := key.Namespace
	runNS := envOrDefaultCtrl("AGENTIC_RUN_NAMESPACE", "openshift-lightspeed")
	if !r.isAIEnabled(ctx) {
		return reconcile.Result{}, r.cleanupAgenticTrigger(ctx, ns, runNS, instance)
	}
	grace, err := agentic.Duration(os.Getenv("AGENTIC_AUTO_DIAGNOSIS_GRACE_PERIOD"), 10*time.Minute, true)
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("agentic grace: %w", err)
	}
	cooldown, err := agentic.Duration(os.Getenv("AGENTIC_RUN_COOLDOWN"), 10*time.Minute, false)
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("agentic cooldown: %w", err)
	}
	tlsEnabled := !strings.EqualFold(os.Getenv(agenticTriggerTLSEnv), "false")
	deploy := newAgenticTriggerDeployment(ns, tlsEnabled)
	for i := range deploy.Spec.Template.Spec.Containers[0].Env {
		e := &deploy.Spec.Template.Spec.Containers[0].Env[i]
		switch e.Name {
		case "AGENTIC_RUN_COOLDOWN":
			e.Value = cooldown.String()
		case "AGENTIC_AUTO_DIAGNOSIS_GRACE_PERIOD":
			e.Value = grace.String()
		}
	}
	objects := []client.Object{
		newAgenticTriggerServiceAccount(ns), newAgenticTriggerNotificationsServiceAccount(ns),
		newAgenticTriggerClusterRole(), newAgenticTriggerClusterRoleBinding(ns),
		newAgenticTriggerRole(runNS), newAgenticTriggerRoleBinding(runNS, ns),
		deploy, newAgenticTriggerService(ns, tlsEnabled),
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:        notifications.TokenSecretName,
				Namespace:   ns,
				Annotations: map[string]string{"kubernetes.io/service-account.name": agenticTriggerNotificationsSAName},
			},
			Type: corev1.SecretTypeServiceAccountToken,
		},
	}
	if tlsEnabled {
		objects = append(objects, &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:        notifications.CAConfigMap,
				Namespace:   ns,
				Annotations: map[string]string{serviceServingCAAnnotation: "true"},
			},
		})
	}
	for _, obj := range objects {
		if err := r.reconcileAgenticObject(ctx, instance, obj); err != nil {
			return reconcile.Result{}, err
		}
	}
	return reconcile.Result{}, nil
}

// Only overwrite fields owned by this integration, preserving API defaults,
// allocated Service addresses and asynchronously populated token/CA data.
func (r *ReconcileGitopsService) reconcileAgenticObject(ctx context.Context, owner *pipelinesv1alpha1.GitopsService, desired client.Object) error {
	current := desired.DeepCopyObject().(client.Object)
	err := r.Client.Get(ctx, client.ObjectKeyFromObject(desired), current)
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	if apierrors.IsNotFound(err) {
		labels := desired.GetLabels()
		if labels == nil {
			labels = map[string]string{}
		}
		labels[agentic.ManagedBy] = agentic.Manager
		desired.SetLabels(labels)
		if err := controllerutil.SetControllerReference(owner, desired, r.Scheme); err != nil {
			return err
		}
		return r.Client.Create(ctx, desired)
	}
	if !metav1.IsControlledBy(current, owner) {
		return fmt.Errorf("refusing to update agentic resource %s not owned by GitopsService %s", client.ObjectKeyFromObject(current), owner.Name)
	}
	before := current.DeepCopyObject().(client.Object)
	labels := current.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	labels[agentic.ManagedBy] = agentic.Manager
	current.SetLabels(labels)
	annotations := current.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	for k, v := range desired.GetAnnotations() {
		annotations[k] = v
	}
	current.SetAnnotations(annotations)
	switch obj := current.(type) {
	case *appsv1.Deployment:
		d := desired.(*appsv1.Deployment)
		obj.Spec.Replicas = d.Spec.Replicas
		obj.Spec.Template.Labels = d.Spec.Template.Labels
		obj.Spec.Template.Spec.ServiceAccountName = d.Spec.Template.Spec.ServiceAccountName
		obj.Spec.Template.Spec.Volumes = d.Spec.Template.Spec.Volumes
		if len(obj.Spec.Template.Spec.Containers) == 0 {
			obj.Spec.Template.Spec.Containers = d.Spec.Template.Spec.Containers
		} else {
			c, dc := &obj.Spec.Template.Spec.Containers[0], d.Spec.Template.Spec.Containers[0]
			c.Image, c.ImagePullPolicy, c.Args, c.Env, c.Ports, c.VolumeMounts = dc.Image, dc.ImagePullPolicy, dc.Args, dc.Env, dc.Ports, dc.VolumeMounts
		}
	case *corev1.Service:
		d := desired.(*corev1.Service)
		obj.Spec.Selector = d.Spec.Selector
		// NodePort is also API-allocated if the Service type was customized.
		if len(obj.Spec.Ports) == 1 && len(d.Spec.Ports) == 1 {
			d.Spec.Ports[0].NodePort = obj.Spec.Ports[0].NodePort
		}
		obj.Spec.Ports = d.Spec.Ports
		if _, ok := d.Annotations["service.beta.openshift.io/serving-cert-secret-name"]; !ok {
			delete(obj.Annotations, "service.beta.openshift.io/serving-cert-secret-name")
		}
	case *rbacv1.ClusterRole:
		obj.Rules = desired.(*rbacv1.ClusterRole).Rules
	case *rbacv1.Role:
		obj.Rules = desired.(*rbacv1.Role).Rules
	case *rbacv1.ClusterRoleBinding:
		obj.Subjects = desired.(*rbacv1.ClusterRoleBinding).Subjects
	case *rbacv1.RoleBinding:
		obj.Subjects = desired.(*rbacv1.RoleBinding).Subjects
	}
	if reflect.DeepEqual(before, current) {
		return nil
	}
	return r.Client.Patch(ctx, current, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
}

func (r *ReconcileGitopsService) cleanupAgenticTrigger(ctx context.Context, ns, runNS string, owner *pipelinesv1alpha1.GitopsService) error {
	// Scale down first to stop accepting new work.
	deploy := &appsv1.Deployment{}
	if err := r.Client.Get(ctx, types.NamespacedName{Name: agenticTriggerServiceName, Namespace: ns}, deploy); err == nil {
		if !metav1.IsControlledBy(deploy, owner) {
			return fmt.Errorf("refusing to remove agentic Deployment not owned by GitopsService %s", owner.Name)
		}
		before := deploy.DeepCopy()
		zero := int32(0)
		deploy.Spec.Replicas = &zero
		if !reflect.DeepEqual(before.Spec.Replicas, deploy.Spec.Replicas) {
			if err := r.Client.Patch(ctx, deploy, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
				return err
			}
		}
	} else if !apierrors.IsNotFound(err) {
		return err
	}

	// Delete all owned K8s resources. The argocd-operator's reconcileAINotifications
	// is responsible for cleaning up the NotificationsConfiguration entries, token,
	// and TLS CA when .spec.ai.troubleshooting becomes false.
	objects := []client.Object{
		newAgenticTriggerDeployment(ns, true), newAgenticTriggerService(ns, true),
		newAgenticTriggerRoleBinding(runNS, ns), newAgenticTriggerRole(runNS),
		newAgenticTriggerClusterRoleBinding(ns), newAgenticTriggerClusterRole(),
		newAgenticTriggerServiceAccount(ns), newAgenticTriggerNotificationsServiceAccount(ns),
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: notifications.TokenSecretName, Namespace: ns}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: agenticTriggerServiceName + "-tls", Namespace: ns}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: notifications.CAConfigMap, Namespace: ns}},
	}
	for _, obj := range objects {
		if err := r.Client.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}
