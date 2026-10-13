export PATH="$HOME/.orbstack/bin:$PATH"

make docker-build-no-test \
  IMG=quay.io/mangaal/gitops-operator:skill-test \
  CONTAINER_RUNTIME=docker PLATFORM=linux/amd64

make docker-build-agentic-trigger \
  AGENTIC_TRIGGER_IMG=quay.io/mangaal/agentic-trigger:skill-test \
  CONTAINER_RUNTIME=docker PLATFORM=linux/amd64

make docker-push IMG=quay.io/mangaal/gitops-operator:skill-test CONTAINER_RUNTIME=docker
make docker-push-agentic-trigger AGENTIC_TRIGGER_IMG=quay.io/mangaal/agentic-trigger:skill-test CONTAINER_RUNTIME=docker
----
make deploy IMG=quay.io/mangaal/gitops-operator:skill-test

# same tag (skill-test) will not be pulled unless pods are recreated
oc -n openshift-gitops-operator delete pod -l control-plane=gitops-operator
oc -n openshift-gitops delete pod -l app.kubernetes.io/name=agentic-trigger

oc -n openshift-gitops-operator rollout status deploy/openshift-gitops-operator-controller-manager
oc -n openshift-gitops rollout status deploy/agentic-trigger

# notifications (needed for auto-diagnosis)
oc -n openshift-gitops patch argocd openshift-gitops --type=merge \
  -p '{"spec":{"notifications":{"enabled":true}}}'

# confirm new-app age gate is live (must include creationTimestamp)
oc -n openshift-gitops get cm argocd-notifications-cm \
  -o jsonpath='{.data.trigger\.on-agentic-diagnose}'
echo
