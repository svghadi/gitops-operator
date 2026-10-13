package controllers

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"strings"
	"time"

	argov1alpha1 "github.com/argoproj-labs/gitops-operator/argocd-operator/api/v1alpha1"
	"github.com/go-logr/logr"
	pipelinesv1alpha1 "github.com/redhat-developer/gitops-operator/api/v1alpha1"
	"github.com/redhat-developer/gitops-operator/internal/agentic"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/yaml"
)

const (
	agenticNotifTriggerKey                = "trigger.on-agentic-diagnose"
	agenticNotifTemplateKey               = "template.agentic-diagnose"
	agenticNotifServiceKey                = "service.webhook.agentic-trigger"
	agenticNotifSubscriptionKey           = "subscriptions"
	defaultNotificationsConfigurationName = "default-notifications-configuration"
	agenticNotifTokenSecretName           = "agentic-trigger-notifications-token"
	argocdNotificationsSecretName         = "argocd-notifications-secret"
	agenticTriggerTokenKey                = "agentic-trigger-token"
	argocdTLSCertsConfigMapName           = "argocd-tls-certs-cm"
	serviceServingCABundleKey             = "service-ca.crt"
	serviceServingCAAnnotation            = "service.beta.openshift.io/inject-cabundle"
	agenticTriggerCAConfigMapName         = "agentic-trigger-serving-ca"
)

func agenticAnnotationsChanged(old, current client.Object) bool {
	oldValue, oldPresent := old.GetAnnotations()[agentic.EnabledAnnotation]
	newValue, newPresent := current.GetAnnotations()[agentic.EnabledAnnotation]
	return oldPresent != newPresent || oldValue != newValue
}

func agenticEnabled(instance *pipelinesv1alpha1.GitopsService) bool {
	if value, exists := instance.Annotations[agentic.EnabledAnnotation]; exists {
		return strings.EqualFold(value, "true")
	}
	return strings.EqualFold(os.Getenv(agenticIntegrationEnabledEnv), "true")
}

func (r *ReconcileGitopsService) agenticDependencyRequests(ctx context.Context, obj client.Object) []reconcile.Request {
	ns, err := GetBackendNamespace(r.Client)
	if err != nil || obj.GetNamespace() != ns {
		return nil
	}
	switch obj.GetName() {
	case agenticNotifTokenSecretName, argocdNotificationsSecretName, agenticTriggerCAConfigMapName, argocdTLSCertsConfigMapName, defaultNotificationsConfigurationName:
		return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: serviceName}}}
	}
	return nil
}

func (r *ReconcileGitopsService) reconcileAgenticTriggerService(key types.NamespacedName, instance *pipelinesv1alpha1.GitopsService, logger logr.Logger) (reconcile.Result, error) {
	ctx := context.TODO()
	ns := key.Namespace
	runNS := envOrDefaultCtrl("AGENTIC_RUN_NAMESPACE", "openshift-lightspeed")
	if !agenticEnabled(instance) {
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
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: agenticNotifTokenSecretName, Namespace: ns, Annotations: map[string]string{"kubernetes.io/service-account.name": agenticTriggerNotificationsSAName}}, Type: corev1.SecretTypeServiceAccountToken},
	}
	if tlsEnabled {
		objects = append(objects, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: agenticTriggerCAConfigMapName, Namespace: ns, Annotations: map[string]string{serviceServingCAAnnotation: "true"}}})
	}
	for _, obj := range objects {
		if err := r.reconcileAgenticObject(ctx, instance, obj); err != nil {
			return reconcile.Result{}, err
		}
	}
	ready, err := r.configureAgenticDependencies(ctx, ns, tlsEnabled)
	if err != nil {
		return reconcile.Result{}, err
	}
	if !ready {
		logger.Info("Waiting for agentic notification configuration, token or serving CA")
		return reconcile.Result{RequeueAfter: 5 * time.Second}, nil
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

func (r *ReconcileGitopsService) configureAgenticDependencies(ctx context.Context, ns string, tlsEnabled bool) (bool, error) {
	ready := true
	nc := &argov1alpha1.NotificationsConfiguration{}
	if err := r.Client.Get(ctx, types.NamespacedName{Name: defaultNotificationsConfigurationName, Namespace: ns}, nc); err != nil {
		if !apierrors.IsNotFound(err) {
			return false, err
		}
		ready = false
	} else {
		before := nc.DeepCopy()
		subscriptions, err := mergeAgenticSubscription(nc.Spec.Subscriptions[agenticNotifSubscriptionKey], true)
		if err != nil {
			return false, err
		}
		if nc.Spec.Subscriptions == nil {
			nc.Spec.Subscriptions = map[string]string{}
		}
		nc.Spec.Subscriptions[agenticNotifSubscriptionKey] = subscriptions
		if nc.Spec.Triggers == nil {
			nc.Spec.Triggers = map[string]string{}
		}
		nc.Spec.Triggers[agenticNotifTriggerKey] = agenticNotificationTrigger()
		if nc.Spec.Templates == nil {
			nc.Spec.Templates = map[string]string{}
		}
		nc.Spec.Templates[agenticNotifTemplateKey] = "webhook:\n  agentic-trigger:\n    method: POST\n    path: /api/v1/diagnose\n    body: |\n      {\"application\":\"{{.app.metadata.name}}\",\"applicationNamespace\":\"{{.app.metadata.namespace}}\"}\n"
		if nc.Spec.Services == nil {
			nc.Spec.Services = map[string]string{}
		}
		scheme, port := "http", 8080
		if tlsEnabled {
			scheme, port = "https", 8443
		}
		nc.Spec.Services[agenticNotifServiceKey] = fmt.Sprintf("url: %s://%s.%s.svc:%d\nheaders:\n- name: Authorization\n  value: Bearer $agentic-trigger-token\n", scheme, agenticTriggerServiceName, ns, port)
		if !reflect.DeepEqual(before.Spec, nc.Spec) {
			if err := r.Client.Patch(ctx, nc, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
				return false, err
			}
		}
	}
	token, target := &corev1.Secret{}, &corev1.Secret{}
	for _, item := range []struct {
		name string
		obj  *corev1.Secret
	}{{agenticNotifTokenSecretName, token}, {argocdNotificationsSecretName, target}} {
		if err := r.Client.Get(ctx, types.NamespacedName{Name: item.name, Namespace: ns}, item.obj); err != nil {
			if !apierrors.IsNotFound(err) {
				return false, err
			}
			ready = false
		}
	}
	if len(token.Data["token"]) == 0 || target.ResourceVersion == "" {
		ready = false
	} else if string(target.Data[agenticTriggerTokenKey]) != string(token.Data["token"]) {
		before := target.DeepCopy()
		if target.Data == nil {
			target.Data = map[string][]byte{}
		}
		target.Data[agenticTriggerTokenKey] = token.Data["token"]
		if err := r.Client.Patch(ctx, target, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
			return false, err
		}
	}
	if tlsEnabled {
		ca, certs := &corev1.ConfigMap{}, &corev1.ConfigMap{}
		for _, item := range []struct {
			name string
			obj  *corev1.ConfigMap
		}{{agenticTriggerCAConfigMapName, ca}, {argocdTLSCertsConfigMapName, certs}} {
			if err := r.Client.Get(ctx, types.NamespacedName{Name: item.name, Namespace: ns}, item.obj); err != nil {
				if !apierrors.IsNotFound(err) {
					return false, err
				}
				ready = false
			}
		}
		host := fmt.Sprintf("%s.%s.svc", agenticTriggerServiceName, ns)
		if ca.Data[serviceServingCABundleKey] == "" || certs.ResourceVersion == "" {
			ready = false
		} else if certs.Data[host] != ca.Data[serviceServingCABundleKey] {
			before := certs.DeepCopy()
			if certs.Data == nil {
				certs.Data = map[string]string{}
			}
			certs.Data[host] = ca.Data[serviceServingCABundleKey]
			if err := r.Client.Patch(ctx, certs, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
				return false, err
			}
		}
	}
	return ready, nil
}

// Split only the integration's recipient/trigger pair out of user subscriptions.
// Maps preserve other supported fields rather than round-tripping a lossy struct.
func mergeAgenticSubscription(value string, enabled bool) (string, error) {
	var entries []map[string]any
	if err := yaml.UnmarshalStrict([]byte(value), &entries); err != nil {
		return "", fmt.Errorf("invalid notification subscriptions: %w", err)
	}
	var result []map[string]any
	for _, entry := range entries {
		recipients, rok := entry["recipients"].([]any)
		if !rok {
			return "", fmt.Errorf("subscription recipients must be a list")
		}
		triggers, tok := entry["triggers"].([]any)
		if entry["triggers"] != nil && !tok {
			return "", fmt.Errorf("subscription triggers must be a list")
		}
		var otherRecipients, otherTriggers []any
		hasRecipient, hasTrigger := false, false
		for _, r := range recipients {
			if _, ok := r.(string); !ok {
				return "", fmt.Errorf("subscription recipient must be a string")
			}
			if r == agenticTriggerServiceName {
				hasRecipient = true
			} else {
				otherRecipients = append(otherRecipients, r)
			}
		}
		for _, t := range triggers {
			if _, ok := t.(string); !ok {
				return "", fmt.Errorf("subscription trigger must be a string")
			}
			if t == "on-agentic-diagnose" {
				hasTrigger = true
			} else {
				otherTriggers = append(otherTriggers, t)
			}
		}
		if !hasRecipient || !hasTrigger {
			result = append(result, entry)
			continue
		}
		if len(otherRecipients) > 0 {
			copy := map[string]any{}
			for k, v := range entry {
				copy[k] = v
			}
			copy["recipients"] = otherRecipients
			result = append(result, copy)
		}
		if len(otherTriggers) > 0 {
			entry["recipients"] = []any{agenticTriggerServiceName}
			entry["triggers"] = otherTriggers
			result = append(result, entry)
		}
	}
	if enabled {
		result = append(result, map[string]any{"recipients": []any{agenticTriggerServiceName}, "triggers": []any{"on-agentic-diagnose"}})
	}
	if len(result) == 0 {
		return "", nil
	}
	b, err := yaml.Marshal(result)
	return string(b), err
}

func agenticNotificationTrigger() string {
	// The epoch allows redelivery if Notifications evaluates the Application
	// again after a lost webhook. It does not schedule that evaluation; once a
	// webhook arrives, the trigger workqueue owns the grace deadline.
	once := "  oncePer: app.metadata.uid + ':' + toJSON(app.status.sync.revisions ?? [app.status.sync.revision ?? '']) + ':' + (app.status.health.lastTransitionTime ?? '') + ':' + string(int(time.Now().Unix() / 60))\n"
	var b strings.Builder
	for _, h := range []string{"Degraded", "Unknown", "Missing", "Progressing", "Healthy", "Suspended"} {
		fmt.Fprintf(&b, "- when: app.status.health.status == '%s'\n", h)
		b.WriteString(once + "  send: [agentic-diagnose]\n")
	}
	return b.String()
}

func (r *ReconcileGitopsService) cleanupAgenticTrigger(ctx context.Context, ns, runNS string, owner *pipelinesv1alpha1.GitopsService) error {
	// Stop accepting work before editing configuration.
	integrationPresent := false
	deploy := &appsv1.Deployment{}
	if err := r.Client.Get(ctx, types.NamespacedName{Name: agenticTriggerServiceName, Namespace: ns}, deploy); err == nil {
		if !metav1.IsControlledBy(deploy, owner) {
			return fmt.Errorf("refusing to remove agentic Deployment not owned by GitopsService %s", owner.Name)
		}
		integrationPresent = true
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
	nc := &argov1alpha1.NotificationsConfiguration{}
	if err := r.Client.Get(ctx, types.NamespacedName{Name: defaultNotificationsConfigurationName, Namespace: ns}, nc); err == nil {
		if _, exists := nc.Spec.Triggers[agenticNotifTriggerKey]; exists {
			integrationPresent = true
		}
		if _, exists := nc.Spec.Services[agenticNotifServiceKey]; exists {
			integrationPresent = true
		}
		if !integrationPresent {
			return nil
		}
		before := nc.DeepCopy()
		value, err := mergeAgenticSubscription(nc.Spec.Subscriptions[agenticNotifSubscriptionKey], false)
		if err != nil {
			return err
		}
		delete(nc.Spec.Triggers, agenticNotifTriggerKey)
		delete(nc.Spec.Templates, agenticNotifTemplateKey)
		delete(nc.Spec.Services, agenticNotifServiceKey)
		if value == "" {
			delete(nc.Spec.Subscriptions, agenticNotifSubscriptionKey)
		} else {
			nc.Spec.Subscriptions[agenticNotifSubscriptionKey] = value
		}
		if !reflect.DeepEqual(before.Spec, nc.Spec) {
			if err := r.Client.Patch(ctx, nc, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
				return err
			}
		}
	} else if !apierrors.IsNotFound(err) {
		return err
	}
	if !integrationPresent {
		return nil
	}
	secret := &corev1.Secret{}
	if err := r.Client.Get(ctx, types.NamespacedName{Name: argocdNotificationsSecretName, Namespace: ns}, secret); err == nil {
		if _, exists := secret.Data[agenticTriggerTokenKey]; exists {
			before := secret.DeepCopy()
			delete(secret.Data, agenticTriggerTokenKey)
			if err := r.Client.Patch(ctx, secret, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
				return err
			}
		}
	} else if !apierrors.IsNotFound(err) {
		return err
	}
	certs := &corev1.ConfigMap{}
	if err := r.Client.Get(ctx, types.NamespacedName{Name: argocdTLSCertsConfigMapName, Namespace: ns}, certs); err == nil {
		host := fmt.Sprintf("%s.%s.svc", agenticTriggerServiceName, ns)
		if _, exists := certs.Data[host]; exists {
			before := certs.DeepCopy()
			delete(certs.Data, host)
			if err := r.Client.Patch(ctx, certs, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
				return err
			}
		}
	} else if !apierrors.IsNotFound(err) {
		return err
	}
	objects := []client.Object{newAgenticTriggerDeployment(ns, true), newAgenticTriggerService(ns, true), newAgenticTriggerRoleBinding(runNS, ns), newAgenticTriggerRole(runNS), newAgenticTriggerClusterRoleBinding(ns), newAgenticTriggerClusterRole(), newAgenticTriggerServiceAccount(ns), newAgenticTriggerNotificationsServiceAccount(ns), &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: agenticNotifTokenSecretName, Namespace: ns}}, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: agenticTriggerServiceName + "-tls", Namespace: ns}}, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: agenticTriggerCAConfigMapName, Namespace: ns}}}
	for _, obj := range objects {
		if err := r.Client.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}
