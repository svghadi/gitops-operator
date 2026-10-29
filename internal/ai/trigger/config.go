package trigger

import (
	"os"
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
	if skillsImage == "" {
		skillsImage = "ghcr.io/redhat-developer/agentic-skills:latest"
	}

	argocdNamespace := os.Getenv("ARGOCD_NAMESPACE")
	if argocdNamespace == "" {
		argocdNamespace = "openshift-gitops"
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
