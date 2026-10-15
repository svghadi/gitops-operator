// Package notifications provides AI-specific Argo CD notification configuration.
// It manages the trigger, template, service, and subscription entries injected into
// the default NotificationsConfiguration CR, as well as propagation of the SA token
// and (optionally) the TLS serving CA into the Argo CD notification secrets.
package notifications

const (
	TriggerKey          = "trigger.on-agentic-diagnose"
	TemplateKey         = "template.agentic-diagnose"
	ServiceKey          = "service.webhook.agentic-trigger"
	SubscriptionKey     = "subscriptions"
	ConfigurationName   = "default-notifications-configuration"
	TokenSecretName     = "agentic-trigger-notifications-token"
	NotificationsSecret = "argocd-notifications-secret"
	TriggerTokenKey     = "agentic-trigger-token"
	TLSCertsConfigMap   = "argocd-tls-certs-cm"
	CABundleKey         = "service-ca.crt"
	CAConfigMap         = "agentic-trigger-serving-ca"
	ServiceName         = "agentic-trigger"
)
