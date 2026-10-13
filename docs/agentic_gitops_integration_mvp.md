# Automatic Argo CD diagnosis

The GitOps Operator manages an `agentic-trigger` service. Argo CD Notifications
sends authenticated Application health events to it; the service reads live
Application state and creates advisory-only Lightspeed `AgenticRun` resources.
It does not execute remediation or watch/list Applications.

## Enablement and prerequisites

Install the Lightspeed Agentic Operator and its `AnalysisResult` API, provision
an analysis Agent, and enable notifications on the default Argo CD instance.
The run namespace must already exist.

```sh
oc patch argocd openshift-gitops -n openshift-gitops --type=merge \
  -p '{"spec":{"notifications":{"enabled":true}}}'
oc annotate gitopsservice cluster \
  gitops.openshift.io/agentic-integration-enabled=true --overwrite
```

An explicit enablement annotation overrides `ENABLE_AGENTIC_GITOPS_INTEGRATION`,
including when the annotation is `false`. Without it, the environment setting
applies (default `false`). Annotation changes trigger reconciliation.

Dedicated resources carry a GitopsService owner reference. Reconciliation
rejects existing resources without that ownership instead of adopting them.

| Operator environment | Default | Meaning |
|---|---|---|
| `AGENTIC_RUN_NAMESPACE` | `openshift-lightspeed` | Existing namespace for runs/results |
| `AGENTIC_ANALYSIS_AGENT` | `default` | Cluster-scoped analysis Agent name |
| `AGENTIC_RUN_COOLDOWN` | `10m` | Delay after a failed analysis before its one retry; must be positive |
| `AGENTIC_AUTO_DIAGNOSIS_GRACE_PERIOD` | `10m` | Application age and sustained-health grace; may be zero |
| `AGENTIC_TRIGGER_TLS_ENABLED` | `true` | Use the OpenShift service-serving certificate |
| `AGENTIC_TRIGGER_IMAGE` | `quay.io/redhat-developer/gitops-operator-agentic-trigger:latest` | Trigger image |
| `AGENTIC_SKILLS_IMAGE` | unset | Optional diagnosis/remediation skills image |

Durations retain full precision (`90s` and `10m30s` are supported). Invalid values
prevent startup/reconciliation rather than silently selecting another policy.

## Health policy and incidents

All Applications wait until their creation grace period has elapsed.
`Degraded` and `Unknown` then qualify immediately. `Missing` and `Progressing`
also require the health `lastTransitionTime` to be at least the grace period old.
`Healthy` and `Suspended` close an incident. Missing health-transition timestamps
defer `Missing` and `Progressing` diagnosis. Notifications send these health
events without applying grace policy. The receiver enforces the policy against
live state and, when grace remains, schedules the Application key on its work
queue for the remaining time. The delayed check reads the latest Application;
it does not require another notification while the trigger process stays up.
If the first webhook never arrives, or the process restarts before an unpersisted
grace timer fires, another notification is needed. There is no Application watch
or periodic scan to recover that pending timer.

An incident gets at most **two runs**: the initial diagnosis and one retry of a
terminal analysis failure after cooldown. Success does not cause periodic runs.
Unhealthy health changes do not reset the budget. Observed recovery followed by
another failure, or a revision change, starts a new incident. Multi-source
Applications use the ordered revision list. Recovery that nobody observed cannot
be reconstructed retrospectively.

Application annotations hold the incident identity, attempt count, creation
confirmation, and retry time. They contain no AgenticRun request or Application
diagnostic snapshot. If creation was not confirmed, reconciliation first checks
the deterministic run name. An existing run is validated against the incident;
if no run exists, current Application health, revision, and grace decide whether
to create a fresh request from live state and trusted service configuration.
Recovery before creation cancels the unfinished diagnosis. Resource-version
preconditions serialize annotation decisions; deterministic run names handle
ambiguous create timeouts and duplicate deliveries. Deleting a run does not
reset its recorded budget. A new incident waits for any existing active run
for the Application to finish.

Notifications enqueue Applications for reconciliation. An AgenticRun informer
enqueues the owning Application when a managed run is added, updated, or deleted.
It validates the current Application UID before enqueueing. The worker reads live
state, saves failed-analysis retry deadlines, and schedules the remaining cooldown
on its work queue. At startup, the informer's initial list replays existing runs
so retries can be scheduled after a restart. There is no periodic run scan.
Existing runs from this branch are adopted using Application UID labels.

Notification keys contain UID, revision(s), health transition, and a one-minute
delivery epoch. If Notifications evaluates the Application again, the epoch
permits redelivery after a failed webhook or trigger restart; it does not itself
schedule reevaluation or reset the incident/run budget. Persistent health states
can therefore produce repeat webhooks when reevaluated. Existing
global notification subscriptions are preserved when the agentic entry is added.

## Disablement

```sh
oc annotate gitopsservice cluster \
  gitops.openshift.io/agentic-integration-enabled=false --overwrite
```

The operator stops the trigger Deployment and removes dedicated resources,
notification entries, injected token, and service CA entry. Existing runs/results
remain available in Lightspeed. Other subscriptions, credentials, and certificates
are preserved.
Disabling stops new triggers/retries; it does not cancel a run already submitted.

## API and verification

Only the dedicated `agentic-trigger-notifications` ServiceAccount is authorized.
Manual user requests and console-user authentication are outside this version.

- `POST /api/v1/diagnose`: JSON `{ "application": "my-app", "applicationNamespace": "openshift-gitops" }`. Returns 202 when queued.
- `GET /api/v1/applications/{namespace}/{name}/diagnosis`: normalized analysis status.

See [the API contract](agentic_diagnosis_backend_plugin_contract.md) for responses.
The operator retries incomplete token/CA/configuration setup every five seconds
and watches dependency updates. Inspect operator/trigger logs for setup failures,
creation commits, and informer or worker errors.

```sh
go test ./internal/agentic ./cmd/agentic-trigger ./controllers
go test -race ./cmd/agentic-trigger ./internal/agentic
make docker-build-agentic-trigger AGENTIC_TRIGGER_IMG=agentic-trigger:test
oc get deployment,service -n openshift-gitops agentic-trigger
oc get agenticruns.agentic.openshift.io -n openshift-lightspeed
```

For rollout, first use a test installation with an existing notification
subscription: enable, diagnose, verify one failed-run retry, recover, diagnose a
new incident, and disable. Confirm existing subscriptions and historical runs
survive. Test TLS mode changes in both directions.

An opt-in API smoke test creates a fresh namespace, uses a nonexistent Agent to
avoid model calls, and deletes the namespace afterward. It exercises the local
HTTP receiver against real TokenReview, Application, and AgenticRun APIs; it
does not deploy the receiver or replace an existing notification installation.

```sh
AGENTIC_API_SMOKE=1 go test ./cmd/agentic-trigger -run '^TestOpenShiftAgenticAPISmoke$' -count=1 -v
```
