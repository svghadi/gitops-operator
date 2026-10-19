package argocd

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/argoproj-labs/gitops-operator/argocd-operator/api/v1alpha1"
	argoproj "github.com/argoproj-labs/gitops-operator/argocd-operator/api/v1beta1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func aiNotificationsFixture(t *testing.T, tls bool) (*ReconcileArgoCD, *argoproj.ArgoCD) {
	t.Helper()
	t.Setenv(enableAIFeaturesEnv, "true")
	t.Setenv(aiTriggerTLSEnv, fmt.Sprint(tls))
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, v1alpha1.AddToScheme, argoproj.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	cr := &argoproj.ArgoCD{
		ObjectMeta: metav1.ObjectMeta{Name: "openshift-gitops", Namespace: "openshift-gitops"},
		Spec:       argoproj.ArgoCDSpec{AI: &argoproj.ArgoCDAISpec{Troubleshooting: true}},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cr).Build()
	return &ReconcileArgoCD{Client: cl, Scheme: scheme}, cr
}

func TestAINotificationsWaitsPropagatesRotatesAndCleansUp(t *testing.T) {
	for _, tls := range []bool{false, true} {
		t.Run(fmt.Sprintf("TLS=%v", tls), func(t *testing.T) {
			r, cr := aiNotificationsFixture(t, tls)
			ctx := context.Background()
			meta := func(name string) metav1.ObjectMeta { return metav1.ObjectMeta{Name: name, Namespace: cr.Namespace} }
			pending := func() {
				t.Helper()
				if err := r.reconcileAINotifications(cr); !errors.Is(err, errAINotificationsPending) {
					t.Fatalf("expected pending dependencies, got %v", err)
				}
			}
			create := func(obj client.Object) {
				t.Helper()
				if err := r.Create(ctx, obj); err != nil {
					t.Fatal(err)
				}
			}
			get := func(obj client.Object) {
				t.Helper()
				if err := r.Get(ctx, client.ObjectKeyFromObject(obj), obj); err != nil {
					t.Fatal(err)
				}
			}
			pending() // NotificationsConfiguration has not been created yet.
			nc := &v1alpha1.NotificationsConfiguration{ObjectMeta: meta(DefaultNotificationsConfigurationInstanceName)}
			nc.Spec.Subscriptions = map[string]string{aiNotifSubscriptionKey: "- recipients: [slack:ops]\n  triggers: [on-sync-failed]\n"}
			create(nc)
			pending() // Source token is absent.
			token := &corev1.Secret{ObjectMeta: meta(aiTriggerTokenSecretName)}
			create(token)
			pending() // Source token has not been populated.
			token.Data = map[string][]byte{"token": []byte("first-token")}
			if err := r.Update(ctx, token); err != nil {
				t.Fatal(err)
			}
			pending() // Destination secret is absent.
			target := &corev1.Secret{ObjectMeta: meta(aiNotificationsSecretName), Data: map[string][]byte{"user-token": []byte("keep")}}
			create(target)
			ca := &corev1.ConfigMap{ObjectMeta: meta(aiCAConfigMapName)}
			certs := &corev1.ConfigMap{ObjectMeta: meta(aiTLSCertsConfigMapName), Data: map[string]string{"user-host": "keep"}}
			if tls {
				pending() // Source CA is absent.
				create(ca)
				pending() // Source CA has not been populated.
				ca.Data = map[string]string{aiCABundleKey: "first-ca"}
				if err := r.Update(ctx, ca); err != nil {
					t.Fatal(err)
				}
				pending() // Destination CA map is absent.
				create(certs)
			}
			for _, generation := range []string{"first", "rotated"} {
				get(token)
				token.Data["token"] = []byte(generation + "-token")
				if err := r.Update(ctx, token); err != nil {
					t.Fatal(err)
				}
				if tls {
					get(ca)
					ca.Data[aiCABundleKey] = generation + "-ca"
					if err := r.Update(ctx, ca); err != nil {
						t.Fatal(err)
					}
				}
				if err := r.reconcileAINotifications(cr); err != nil {
					t.Fatal(err)
				}
				get(target)
				if string(target.Data[aiTriggerTokenKey]) != generation+"-token" || string(target.Data["user-token"]) != "keep" {
					t.Fatalf("incorrect token propagation: %v", target.Data)
				}
				if tls {
					get(certs)
					if certs.Data["ai-controller."+cr.Namespace+".svc"] != generation+"-ca" || certs.Data["user-host"] != "keep" {
						t.Fatalf("incorrect CA propagation: %v", certs.Data)
					}
				}
			}
			get(nc)
			endpoint := "http://ai-controller." + cr.Namespace + ".svc:8080"
			if tls {
				endpoint = "https://ai-controller." + cr.Namespace + ".svc:8443"
			}
			if !strings.Contains(nc.Spec.Services[aiNotifServiceKey], endpoint) || !strings.Contains(nc.Spec.Subscriptions[aiNotifSubscriptionKey], "slack:ops") {
				t.Fatalf("incorrect notification configuration: %+v", nc.Spec)
			}
			version := nc.ResourceVersion
			tokenVersion := target.ResourceVersion
			if err := r.reconcileAINotifications(cr); err != nil {
				t.Fatal(err)
			}
			get(nc)
			get(target)
			if nc.ResourceVersion != version || target.ResourceVersion != tokenVersion {
				t.Fatal("unchanged configuration was patched")
			}
			cr.Spec.AI.Troubleshooting = false
			if err := r.reconcileAINotifications(cr); err != nil {
				t.Fatal(err)
			}
			get(nc)
			get(target)
			if _, exists := target.Data[aiTriggerTokenKey]; exists {
				t.Fatal("integration token retained")
			}
			if string(target.Data["user-token"]) != "keep" || strings.Contains(nc.Spec.Subscriptions[aiNotifSubscriptionKey], "agentic-trigger") || !strings.Contains(nc.Spec.Subscriptions[aiNotifSubscriptionKey], "slack:ops") {
				t.Fatal("cleanup failed to preserve user configuration")
			}
			if nc.Spec.Triggers[aiNotifTriggerKey] != "" || nc.Spec.Templates[aiNotifTemplateKey] != "" || nc.Spec.Services[aiNotifServiceKey] != "" {
				t.Fatal("AI notification entries retained")
			}
			if tls {
				get(certs)
				if len(certs.Data) != 1 || certs.Data["user-host"] != "keep" {
					t.Fatal("incorrect CA cleanup")
				}
			}
		})
	}
}

func TestAINotificationsPreservesMalformedSubscription(t *testing.T) {
	r, cr := aiNotificationsFixture(t, false)
	nc := &v1alpha1.NotificationsConfiguration{ObjectMeta: metav1.ObjectMeta{Name: DefaultNotificationsConfigurationInstanceName, Namespace: cr.Namespace}}
	nc.Spec.Subscriptions = map[string]string{aiNotifSubscriptionKey: "[broken"}
	if err := r.Create(context.Background(), nc); err != nil {
		t.Fatal(err)
	}
	version := nc.ResourceVersion
	if err := r.reconcileAINotifications(cr); err == nil || errors.Is(err, errAINotificationsPending) {
		t.Fatalf("expected invalid subscription error, got %v", err)
	}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(nc), nc); err != nil {
		t.Fatal(err)
	}
	if nc.ResourceVersion != version || nc.Spec.Subscriptions[aiNotifSubscriptionKey] != "[broken" {
		t.Fatal("malformed user configuration overwritten")
	}
}

func TestAINotificationDependencyRequests(t *testing.T) {
	r, cr := aiNotificationsFixture(t, true)
	for _, tc := range []struct {
		name string
		obj  client.Object
		want int
	}{
		{"token rotation", &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: aiTriggerTokenSecretName, Namespace: cr.Namespace}}, 1},
		{"CA rotation", &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: aiCAConfigMapName, Namespace: cr.Namespace}}, 1},
		{"unrelated secret", &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: cr.Namespace}}, 0},
		{"other namespace", &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: aiTriggerTokenSecretName, Namespace: "other"}}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requests := r.aiNotificationDependencyRequests(context.Background(), tc.obj)
			if len(requests) != tc.want {
				t.Fatalf("got %v, want %d requests", requests, tc.want)
			}
			if len(requests) > 0 && requests[0].NamespacedName != client.ObjectKeyFromObject(cr) {
				t.Fatalf("wrong ArgoCD request: %v", requests)
			}
		})
	}
}

func TestAINotificationsCleanupWithoutConfiguration(t *testing.T) {
	r, cr := aiNotificationsFixture(t, true)
	t.Setenv(enableAIFeaturesEnv, "false")
	ctx := context.Background()
	target := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: aiNotificationsSecretName, Namespace: cr.Namespace}, Data: map[string][]byte{aiTriggerTokenKey: []byte("remove"), "user-token": []byte("keep")}}
	certs := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: aiTLSCertsConfigMapName, Namespace: cr.Namespace}, Data: map[string]string{"ai-controller." + cr.Namespace + ".svc": "remove", "user-host": "keep"}}
	for _, obj := range []client.Object{target, certs} {
		if err := r.Create(ctx, obj); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.reconcileAINotifications(cr); err != nil {
		t.Fatal(err)
	}
	for _, obj := range []client.Object{target, certs} {
		if err := r.Get(ctx, client.ObjectKeyFromObject(obj), obj); err != nil {
			t.Fatal(err)
		}
	}
	if len(target.Data) != 1 || string(target.Data["user-token"]) != "keep" || len(certs.Data) != 1 || certs.Data["user-host"] != "keep" {
		t.Fatal("cleanup skipped credentials or removed user data")
	}
}
