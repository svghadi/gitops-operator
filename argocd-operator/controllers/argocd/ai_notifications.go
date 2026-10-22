package argocd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"

	"github.com/argoproj-labs/gitops-operator/argocd-operator/api/v1alpha1"
	argoproj "github.com/argoproj-labs/gitops-operator/argocd-operator/api/v1beta1"
	aiconfig "github.com/argoproj-labs/gitops-operator/argocd-operator/pkg/ai"
	"github.com/argoproj-labs/gitops-operator/argocd-operator/controllers/argoutil"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/yaml"
)

const (
	aiNotifTriggerKey         = "trigger.on-agentic-diagnose"
	aiNotifTemplateKey        = "template.agentic-diagnose"
	aiNotifServiceKey         = "service.webhook.agentic-trigger"
	aiNotifSubscriptionKey    = "subscriptions"
	aiTriggerTokenSecretName  = "agentic-trigger-notifications-token"
	aiNotificationsSecretName = "argocd-notifications-secret"
	aiTriggerTokenKey         = "agentic-trigger-token"
	aiTLSCertsConfigMapName   = "argocd-tls-certs-cm"
	aiCABundleKey             = "service-ca.crt"
	aiCAConfigMapName         = "agentic-trigger-serving-ca"
	aiTriggerServiceName      = aiconfig.ServiceName
	aiWebhookName             = "agentic-trigger"
	aiTriggerTLSEnv           = "AGENTIC_TRIGGER_TLS_ENABLED"
	enableAIFeaturesEnv       = "ENABLE_AI_FEATURES"
)

// Missing dependencies are expected while the service-account and service-CA
// controllers populate their resources. The reconciler schedules a short retry.
var errAINotificationsPending = errors.New("waiting for AI notification configuration, token or serving CA")

// aiNotificationDependencyRequests routes source and destination changes to
// ArgoCD, including resources that may not carry an ArgoCD owner reference.
func (r *ReconcileArgoCD) aiNotificationDependencyRequests(ctx context.Context, obj client.Object) []reconcile.Request {
	switch obj.(type) {
	case *corev1.Secret:
		if obj.GetName() != aiTriggerTokenSecretName && obj.GetName() != aiNotificationsSecretName {
			return nil
		}
	case *corev1.ConfigMap:
		if obj.GetName() != aiCAConfigMapName && obj.GetName() != aiTLSCertsConfigMapName {
			return nil
		}
	case *v1alpha1.NotificationsConfiguration:
		if obj.GetName() != DefaultNotificationsConfigurationInstanceName {
			return nil
		}
	default:
		return nil
	}
	instances := &argoproj.ArgoCDList{}
	if err := r.List(ctx, instances, client.InNamespace(obj.GetNamespace())); err != nil {
		log.Error(err, "unable to find ArgoCD instances for AI dependency update")
		return nil
	}
	requests := make([]reconcile.Request, 0, len(instances.Items))
	for _, instance := range instances.Items {
		requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&instance)})
	}
	return requests
}

// AINotificationsEnabled returns true when the operator-level ENABLE_AI_FEATURES
// env var is set and the ArgoCD instance has opted in via .spec.ai.troubleshooting.
func AINotificationsEnabled(cr *argoproj.ArgoCD) bool {
	return strings.EqualFold(os.Getenv(enableAIFeaturesEnv), "true") &&
		cr.Spec.AI != nil && cr.Spec.AI.Troubleshooting
}

// reconcileAINotifications configures or removes AI-specific notification entries
// (trigger, template, webhook service, subscription, SA token, TLS CA) in the
// ArgoCD instance's NotificationsConfiguration CR and supporting secrets.
func (r *ReconcileArgoCD) reconcileAINotifications(cr *argoproj.ArgoCD) error {
	if !AINotificationsEnabled(cr) {
		return r.cleanupAINotifications(cr)
	}

	ns := cr.Namespace
	tlsEnabled := !strings.EqualFold(os.Getenv(aiTriggerTLSEnv), "false")

	nc := &v1alpha1.NotificationsConfiguration{}
	if err := r.Client.Get(context.TODO(), types.NamespacedName{Name: DefaultNotificationsConfigurationInstanceName, Namespace: ns}, nc); err != nil {
		if apierrors.IsNotFound(err) {
			return errAINotificationsPending
		}
		return err
	}
	before := nc.DeepCopy()
	subscriptions, err := mergeAISubscription(nc.Spec.Subscriptions[aiNotifSubscriptionKey], true)
	if err != nil {
		return err
	}
	if nc.Spec.Subscriptions == nil {
		nc.Spec.Subscriptions = map[string]string{}
	}
	nc.Spec.Subscriptions[aiNotifSubscriptionKey] = subscriptions
	if nc.Spec.Triggers == nil {
		nc.Spec.Triggers = map[string]string{}
	}
	nc.Spec.Triggers[aiNotifTriggerKey] = aiNotificationTrigger()
	if nc.Spec.Templates == nil {
		nc.Spec.Templates = map[string]string{}
	}
	nc.Spec.Templates[aiNotifTemplateKey] = "webhook:\n  agentic-trigger:\n    method: POST\n    path: /api/v1/diagnose\n    body: |\n      {\"application\":\"{{.app.metadata.name}}\",\"applicationNamespace\":\"{{.app.metadata.namespace}}\"}\n"
	if nc.Spec.Services == nil {
		nc.Spec.Services = map[string]string{}
	}
	scheme, port := "http", 8080
	if tlsEnabled {
		scheme, port = "https", 8443
	}
	// The agentic-trigger Service lives in the operator namespace, not the ArgoCD namespace.
	triggerNS, err := argoutil.GetOperatorNamespace()
	if err != nil {
		return fmt.Errorf("could not determine operator namespace for agentic-trigger webhook URL: %w", err)
	}
	nc.Spec.Services[aiNotifServiceKey] = fmt.Sprintf("url: %s://%s.%s.svc:%d\nheaders:\n- name: Authorization\n  value: Bearer $agentic-trigger-token\n", scheme, aiTriggerServiceName, triggerNS, port)
	if !reflect.DeepEqual(before.Spec, nc.Spec) {
		if err := r.Client.Patch(context.TODO(), nc, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
			return err
		}
	}

	// Copy the SA token into argocd-notifications-secret so the webhook can authenticate.
	token := &corev1.Secret{}
	if err := r.Client.Get(context.TODO(), types.NamespacedName{Name: aiTriggerTokenSecretName, Namespace: ns}, token); err != nil {
		if apierrors.IsNotFound(err) {
			return errAINotificationsPending
		}
		return err
	}
	if len(token.Data["token"]) == 0 {
		return errAINotificationsPending
	}
	notifSecret := &corev1.Secret{}
	if err := r.Client.Get(context.TODO(), types.NamespacedName{Name: aiNotificationsSecretName, Namespace: ns}, notifSecret); err != nil {
		if apierrors.IsNotFound(err) {
			return errAINotificationsPending
		}
		return err
	}
	if string(notifSecret.Data[aiTriggerTokenKey]) != string(token.Data["token"]) {
		before := notifSecret.DeepCopy()
		if notifSecret.Data == nil {
			notifSecret.Data = map[string][]byte{}
		}
		notifSecret.Data[aiTriggerTokenKey] = token.Data["token"]
		if err := r.Client.Patch(context.TODO(), notifSecret, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
			return err
		}
	}

	if !tlsEnabled {
		return nil
	}

	// Copy the serving CA bundle into argocd-tls-certs-cm so Argo CD trusts the webhook endpoint.
	ca := &corev1.ConfigMap{}
	if err := r.Client.Get(context.TODO(), types.NamespacedName{Name: aiCAConfigMapName, Namespace: ns}, ca); err != nil {
		if apierrors.IsNotFound(err) {
			return errAINotificationsPending
		}
		return err
	}
	if ca.Data[aiCABundleKey] == "" {
		return errAINotificationsPending
	}
	certs := &corev1.ConfigMap{}
	if err := r.Client.Get(context.TODO(), types.NamespacedName{Name: aiTLSCertsConfigMapName, Namespace: ns}, certs); err != nil {
		if apierrors.IsNotFound(err) {
			return errAINotificationsPending
		}
		return err
	}
	host := fmt.Sprintf("%s.%s.svc", aiTriggerServiceName, triggerNS)
	if certs.Data[host] != ca.Data[aiCABundleKey] {
		before := certs.DeepCopy()
		if certs.Data == nil {
			certs.Data = map[string]string{}
		}
		certs.Data[host] = ca.Data[aiCABundleKey]
		if err := r.Client.Patch(context.TODO(), certs, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
			return err
		}
	}
	return nil
}

func (r *ReconcileArgoCD) cleanupAINotifications(cr *argoproj.ArgoCD) error {
	ns := cr.Namespace

	nc := &v1alpha1.NotificationsConfiguration{}
	if err := r.Client.Get(context.TODO(), types.NamespacedName{Name: DefaultNotificationsConfigurationInstanceName, Namespace: ns}, nc); err == nil {
		before := nc.DeepCopy()
		value, err := mergeAISubscription(nc.Spec.Subscriptions[aiNotifSubscriptionKey], false)
		if err != nil {
			return err
		}
		delete(nc.Spec.Triggers, aiNotifTriggerKey)
		delete(nc.Spec.Templates, aiNotifTemplateKey)
		delete(nc.Spec.Services, aiNotifServiceKey)
		if value == "" {
			delete(nc.Spec.Subscriptions, aiNotifSubscriptionKey)
		} else {
			nc.Spec.Subscriptions[aiNotifSubscriptionKey] = value
		}
		if !reflect.DeepEqual(before.Spec, nc.Spec) {
			if err := r.Client.Patch(context.TODO(), nc, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
				return err
			}
		}
	} else if !apierrors.IsNotFound(err) {
		return err
	}

	secret := &corev1.Secret{}
	if err := r.Client.Get(context.TODO(), types.NamespacedName{Name: aiNotificationsSecretName, Namespace: ns}, secret); err == nil {
		if _, exists := secret.Data[aiTriggerTokenKey]; exists {
			before := secret.DeepCopy()
			delete(secret.Data, aiTriggerTokenKey)
			if err := r.Client.Patch(context.TODO(), secret, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
				return err
			}
		}
	} else if !apierrors.IsNotFound(err) {
		return err
	}

	triggerNS, _ := argoutil.GetOperatorNamespace()
	if triggerNS == "" {
		triggerNS = ns
	}
	certs := &corev1.ConfigMap{}
	if err := r.Client.Get(context.TODO(), types.NamespacedName{Name: aiTLSCertsConfigMapName, Namespace: ns}, certs); err == nil {
		host := fmt.Sprintf("%s.%s.svc", aiTriggerServiceName, triggerNS)
		if _, exists := certs.Data[host]; exists {
			before := certs.DeepCopy()
			delete(certs.Data, host)
			if err := r.Client.Patch(context.TODO(), certs, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
				return err
			}
		}
	} else if !apierrors.IsNotFound(err) {
		return err
	}

	return nil
}

// aiNotificationTrigger returns the YAML for the on-agentic-diagnose trigger.
// The epoch-based oncePer key allows redelivery when Notifications re-evaluates
// an Application after a lost webhook without scheduling that re-evaluation itself.
func aiNotificationTrigger() string {
	once := "  oncePer: app.metadata.uid + ':' + toJSON(app.status.sync.revisions ?? [app.status.sync.revision ?? '']) + ':' + (app.status.health.lastTransitionTime ?? '') + ':' + string(int(time.Now().Unix() / 60))\n"
	var b strings.Builder
	for _, h := range []string{"Degraded", "Unknown", "Missing", "Progressing", "Healthy", "Suspended"} {
		fmt.Fprintf(&b, "- when: app.status.health.status == '%s'\n", h)
		b.WriteString(once + "  send: [agentic-diagnose]\n")
	}
	return b.String()
}

// mergeAISubscription idempotently inserts or removes the agentic recipient/trigger
// pair from the subscription YAML list, preserving all user-managed entries.
func mergeAISubscription(value string, enabled bool) (string, error) {
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
		for _, rec := range recipients {
			if _, ok := rec.(string); !ok {
				return "", fmt.Errorf("subscription recipient must be a string")
			}
			if rec == aiWebhookName {
				hasRecipient = true
			} else {
				otherRecipients = append(otherRecipients, rec)
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
			cp := map[string]any{}
			for k, v := range entry {
				cp[k] = v
			}
			cp["recipients"] = otherRecipients
			result = append(result, cp)
		}
		if len(otherTriggers) > 0 {
			entry["recipients"] = []any{aiWebhookName}
			entry["triggers"] = otherTriggers
			result = append(result, entry)
		}
	}
	if enabled {
		result = append(result, map[string]any{
			"recipients": []any{aiWebhookName},
			"triggers":   []any{"on-agentic-diagnose"},
		})
	}
	if len(result) == 0 {
		return "", nil
	}
	b, err := yaml.Marshal(result)
	return string(b), err
}
