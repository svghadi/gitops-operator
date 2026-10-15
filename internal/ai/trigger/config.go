package trigger

import (
	"os"
	"strings"
	"time"

	"github.com/redhat-developer/gitops-operator/internal/agentic"
)

// LoadServiceConfig builds ServiceConfig from environment variables.
func LoadServiceConfig() (ServiceConfig, error) {
	cooldown, err := agentic.Duration(os.Getenv("AGENTIC_RUN_COOLDOWN"), 10*time.Minute, false)
	if err != nil {
		return ServiceConfig{}, err
	}
	autoDiagnosisGracePeriod, err := agentic.Duration(os.Getenv("AGENTIC_AUTO_DIAGNOSIS_GRACE_PERIOD"), 10*time.Minute, true)
	if err != nil {
		return ServiceConfig{}, err
	}

	namespace := os.Getenv("AGENTIC_RUN_NAMESPACE")
	if namespace == "" {
		namespace = "openshift-lightspeed"
	}

	agent := os.Getenv("AGENTIC_ANALYSIS_AGENT")
	if agent == "" {
		agent = "default"
	}

	skillsImage := os.Getenv("AGENTIC_SKILLS_IMAGE")

	argocdNamespace := os.Getenv("ARGOCD_NAMESPACE")
	if argocdNamespace == "" {
		argocdNamespace = "openshift-gitops"
	}
	if data, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace"); err == nil {
		argocdNamespace = strings.TrimSpace(string(data))
	}

	return ServiceConfig{
		RunNamespace:             namespace,
		AnalysisAgent:            agent,
		Cooldown:                 cooldown,
		SkillsImage:              skillsImage,
		ArgocdNamespace:          argocdNamespace,
		AutoDiagnosisGracePeriod: autoDiagnosisGracePeriod,
	}, nil
}
