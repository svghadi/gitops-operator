# AI Troubleshooting with OpenShift Lightspeed

OpenShift GitOps can automatically diagnose failing Argo CD Applications using the
OpenShift Lightspeed agentic operator. When an Application becomes unhealthy, the
operator triggers an `AgenticRun` that runs AI-powered diagnosis against the
live cluster and reports its findings.

## Prerequisites

- An OpenShift cluster with cluster-admin access
- An LLM API key (OpenAI, Anthropic, or Google Vertex)
- The Lightspeed agentic operator installed and configured (see below)

## 1. Install and configure the Lightspeed agentic operator

The Lightspeed agentic operator is currently in development. Install it using the
quickstart scripts from the upstream repository.

```sh
git clone https://github.com/openshift/lightspeed-agentic-operator.git
cd lightspeed-agentic-operator
bash hack/quickstart/install.sh
```

### Create the LLM credentials secret

```sh
oc create secret generic llm-creds-openai -n openshift-lightspeed \
  --from-literal=OPENAI_API_KEY=<your-api-key>
```

For providers other than OpenAI, see the examples in
`hack/quickstart/examples/` of the upstream repository.

### Create the LLMProvider and Agent CRs

```yaml
apiVersion: agentic.openshift.io/v1alpha1
kind: LLMProvider
metadata:
  name: openai
  namespace: openshift-lightspeed
spec:
  type: OpenAI
  openAI:
    credentialsSecret:
      name: llm-creds-openai
---
apiVersion: agentic.openshift.io/v1alpha1
kind: Agent
metadata:
  name: default
  namespace: openshift-lightspeed
spec:
  llmProvider:
    name: openai
  model: "gpt-4o"
  timeouts:
    analysisSeconds: 120
    executionSeconds: 120
    verificationSeconds: 120
```

```sh
oc apply -n openshift-lightspeed -f <above-yaml>
oc get llmprovider,agent -n openshift-lightspeed
```

## 2. Install the GitOps operator

Use the development catalog source to install the GitOps operator from the OpenShift
console. Apply the following `CatalogSource` first, then install the operator through
OperatorHub as usual.

```yaml
apiVersion: operators.coreos.com/v1alpha1
kind: CatalogSource
metadata:
  name: gitops-operator-catalog
  namespace: openshift-marketplace
spec:
  sourceType: grpc
  image: ghcr.io/svghadi/gitops-operator-catalog:latest
  displayName: GitOps Operator (Dev)
  publisher: svghadi
  updateStrategy:
    registryPoll:
      interval: 10m
```

## 3. Enable AI troubleshooting

Set the troubleshooting flag on the ArgoCD instance:

```sh
oc -n openshift-gitops patch argocd openshift-gitops --type=merge \
  -p '{"spec":{"ai":{"troubleshooting":true}}}'
```

The `ai-controller` deployment is created asynchronously after opting in. If it is
not present immediately, check operator logs and retry the rollout command.

## 4. Verify the installation

```sh
# Check the ai-controller is running
oc -n openshift-gitops get deployment ai-controller

# Check the notifications trigger is configured
oc -n openshift-gitops get cm argocd-notifications-cm \
  -o jsonpath='{.data.trigger\.on-agentic-diagnose}{"\n"}'

# Watch for AgenticRuns when an Application fails
oc -n openshift-lightspeed get agenticruns.agentic.openshift.io -w

# Tail logs
oc -n openshift-gitops logs deployment/ai-controller --tail=100
oc -n openshift-gitops-operator logs deployment/openshift-gitops-operator-controller-manager \
  -c manager --tail=100
```

A successful diagnosis additionally requires a working `Agent`, `LLMProvider`,
credentials, and a failing test Application.

## Configuration reference

All settings below are env vars on the `openshift-gitops-operator-controller-manager`
deployment. The operator forwards relevant values to the `ai-controller` pod.

| Env var | Default | Description |
|---|---|---|
| `ENABLE_AI_FEATURES` | `true` | Master on/off switch (set to `false` to disable entirely) |
| `AGENTIC_TRIGGER_IMAGE` | `ghcr.io/svghadi/gitops-operator-agentic-trigger:latest` | Image for the ai-controller deployment |
| `AGENTIC_SKILLS_IMAGE` | `ghcr.io/redhat-developer/agentic-skills:latest` | Image containing the diagnosis skill scripts |
| `AGENTIC_RUN_NAMESPACE` | `openshift-lightspeed` | Namespace where `AgenticRun` CRs are created |
| `AGENTIC_ANALYSIS_AGENT` | `default` | Name of the `Agent` CR used for analysis |
| `AGENTIC_RUN_COOLDOWN` | `10m` | Minimum time between AgenticRuns for the same Application |
| `AGENTIC_AUTO_DIAGNOSIS_GRACE_PERIOD` | `10m` | How long an Application must be unhealthy before diagnosis triggers |
| `AGENTIC_TRIGGER_TLS_ENABLED` | `true` | Whether the ai-controller webhook uses TLS |

Per-ArgoCD-instance setting (on the `ArgoCD` CR):

| Field | Description |
|---|---|
| `spec.ai.troubleshooting: true` | Enables AI troubleshooting for this ArgoCD instance |

## Common issues

**`WorkflowResolutionFailed: Agent "default" references missing LLMProvider "openai"`**

The `LLMProvider` CR does not exist in the `AGENTIC_RUN_NAMESPACE`. Create it
following step 1 above.

