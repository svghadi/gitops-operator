# Agentic diagnosis service contract

The current service supports **automatic notification callers only**. Both
endpoints require a bearer token whose TokenReview identity is exactly
`system:serviceaccount:<trigger-namespace>:agentic-trigger-notifications`.
Unauthenticated callers receive 401; other authenticated identities receive 403.
Console user tokens and manual diagnosis are not supported by this version.

## POST `/api/v1/diagnose`

```json
{"application":"my-app","applicationNamespace":"openshift-gitops"}
```

The receiver authenticates the caller and queues the Application. It returns
202 before diagnosis is reconciled:

```json
{"accepted":true,"reason":"diagnosis queued"}
```

The worker fetches the live Application; request status fields are not trusted.
Recovery closes a persisted incident. Qualifying unhealthy states pass creation-age
and sustained-health grace checks. When grace remains, the queue schedules another
check after the remaining duration. Invalid JSON or missing names returns 400;
an unavailable queue returns 503. A 202 means only that work was queued, not that
an AgenticRun was created. Use the diagnosis GET endpoint to check the result.

Each incident permits one initial run and one retry of a terminal analysis
failure after cooldown. Revision changes or observed recovery followed by a new
failure create a new incident; unhealthy flaps do not. AgenticRun add, update, and
delete events enqueue the owning Application. A startup informer list restores
retry scheduling after restart; no periodic AgenticRun scanner runs. There is at
most one active managed run per Application.

## GET `/api/v1/applications/{namespace}/{name}/diagnosis`

```json
{
  "status":"completed",
  "trigger":"automatic",
  "run":{"name":"gitops-diag-<identity>-1","namespace":"openshift-lightspeed","startedAt":"2026-09-29T12:00:00Z","completedAt":"2026-09-29T12:02:00Z"},
  "application":{"name":"my-app","namespace":"openshift-gitops","uid":"<uid>","syncStatus":"Synced","healthStatus":"Degraded","revision":"<revision>"},
  "stale":false,
  "summary":"The pod cannot pull its requested image.",
  "suggestions":[{"id":"<result-name>-1","severity":"info","title":"Correct the image reference","description":"The requested tag does not exist.","remediation":"Update the deployment image in Git."}]
}
```

Statuses remain `idle`, `running`, `completed`, and `failed`; authentication/API
errors use an HTTP error response. Optional fields are omitted when unavailable.
Applications recreated with a different UID do not inherit earlier diagnoses.
Revision or health changes mark an existing diagnosis stale. For multi-source
Applications, `revision` is the JSON-encoded ordered revision array.

The supported Lightspeed API is `agentic.openshift.io/v1alpha1`:

- Run conditions determine lifecycle state, including analysis failures whose
  reason is more specific than `Failed`.
- Read the **last** reference in `status.steps.analysis.results` and fetch that
  `AnalysisResult` in the run namespace. Verify `spec.agenticRunName`.
- Summary comes from `status.diagnosis.summary`, falling back to the first
  option's diagnosis summary. No-action results need no options.
- Each `status.options` entry maps to a suggestion containing its title,
  diagnosis root cause/summary, and remediation description/actions. Severity is
  `info` because the source API has no diagnosis-severity field; remediation risk
  is not treated as severity.
- Completion time comes from the result's `Completed=True` condition.
- Failed analysis exposes `status.failureReason`, or the run condition message
  if no result exists. A successful run with a missing/unavailable result stays
  `running` until its result is available, preventing an empty completed card.

## Correlation, persistence, and permissions

New runs use Application UID and namespace labels plus
`gitops.openshift.io/managed-by: agentic-trigger`. The full Application name is
stored in an annotation so names longer than 63 characters remain valid.

The integration owns `gitops.openshift.io/diagnosis-incident` on Applications and
its diagnosis metadata on managed runs. The incident stores the deterministic
run identity, creation confirmation, and retry budget across restarts; it is not
an end-user configuration interface. Before a run is created, its request is
built from the current Application and service configuration. The created run
retains its diagnostic context and is restricted to advisory-only analysis.
Diagnosis selects the latest owned AgenticRun using its Application correlation
metadata.
Unknown incident fields are rejected.

The trigger needs `get,patch` on Applications, `create` on TokenReviews,
`create,get,list,watch` on AgenticRuns in its configured namespace, and `get,list`
on AnalysisResults there. It never lists or watches Applications. The AgenticRun
informer uses an initial list and then watches changes with periodic resync disabled.

Feature settings, prerequisites, and lifecycle behavior are documented in
[the integration guide](agentic_gitops_integration_mvp.md).
