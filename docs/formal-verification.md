# Formal verification

Two checks cover behavior that unit tests can only sample: TLA+ models of the
session lifecycle and of hub retention, and native Go fuzz targets. A third
check covers the chart RBAC over its whole namespace configuration space. Each
has a script or command and a CI job.

| Check | Script | CI job |
|---|---|---|
| TLC on `SessionResume.tla` and `HubRetention.tla` | `hack/tlc.sh` | `tlc` in [verify.yml](../.github/workflows/verify.yml), on pull requests |
| TLC with two sessions | `hack/tlc.sh --deep` | `tlc-deep` in verify.yml, nightly and on demand |
| Every fuzz target | `hack/fuzz.sh` | `fuzz-nightly` in verify.yml |
| Chart RBAC property check | `MAYFLY_REQUIRE_HELM=1 go test ./test/chart/...` | `chart` step of the `test` job in [ci.yml](../.github/workflows/ci.yml) |

`hack/tlc.sh` needs a JDK 17 or newer (`java` on `PATH`, or `JAVA`). It
downloads the pinned, checksummed `tla2tools.jar` once into
`${XDG_CACHE_HOME:-$HOME/.cache}/mayfly/tla`. `hack/fuzz.sh` needs only Go.

## What is verified

The model is `spec/tla/SessionResume.tla`. It follows the implemented protocol,
including the parts that are easy to get wrong:

- a user message takes its turn key from the in-memory hub, after the journal
  has reserved that sequence block;
- the runtime journals a step before applying it to the conversation;
- the gateway records a tool call before it dispatches, and applies the resume
  table on the next begin;
- a pod death loses only in-memory state; the journal, ledger, sequence floor,
  session record, and workspace survive;
- a control-plane restart loses the hub, in-flight begins, and registered
  instances; adoption seeds the hub above the durable floor, restores the
  thread, and refuses a session a DELETE removed;
- a replacement pod gets a new runtime instance and replays recorded steps
  without repeating a recorded tool result;
- finalize and cleanup run once, and cleanup removes the pod, Secret,
  ConfigMap, NetworkPolicy, PVC, journal, and ledger.

`SessionResume.cfg` checks every invariant and both liveness properties with
one session, two turns, one control-plane restart, one pod kill, and one
replacement pod. Sessions do not share mutable state, so one session covers
every property; a second session only multiplies the liveness state space.
The bounds keep a full run under three minutes on a desktop.

`deep/SessionResume_two_sessions.cfg` runs nightly. It checks the safety
invariants with two sessions, one turn each, and one control-plane restart
that hits both at once, without pod kills or replacements: about 4.4 million
distinct states and 20 minutes on a 6-core desktop. Liveness with two sessions
is out of reach at any useful bound.

`SessionResume_buggy.cfg` and `SessionResume_interrupt_buggy.cfg` are the negative checks: they must fail. See
[spec/tla/README.md](../spec/tla/README.md) for the cfg convention.

## Action to Go map

| TLA+ action | Go code it models |
|---|---|
| `Create` | `session.Manager.Create` (`internal/session/manager.go`), `runner.Kubernetes.Start`, `k8s.Client.CreateSession` |
| `PostMessage` | `session.Manager.PostMessage` and `reserveSequence` (`internal/session/lifecycle.go`), `journal.Reserve` (`internal/journal/journal.go`) |
| `Close` | `session.Manager.Close` |
| `TTLExpire` | `session.Manager.reap` (TTL and idle close) |
| `UserDelete` | `session.Manager.Delete` (tombstone, failed with reason `deleted`) |
| `RuntimeFinalize` | `session.Manager.Complete` and `finalize` (`internal/session/lifecycle.go`) |
| `FallbackExport` | `session.Manager.fallback` and `failFallback` |
| `Cleanup` | `session.Manager.cleanup` and `deleteJournal`, `runner.Kubernetes.Stop` |
| `RuntimeStartTurn` | the runtime's inbox loop (`internal/agent/runtime.go`: `Run`) and `Manager.RuntimeConnected` |
| `JournalUserStep` | `agent.handleTurn` step 0 and `session.Manager.AppendJournal` (`internal/session/journal.go`) |
| `ModelCall` | `agent.executeModel` |
| `JournalModelStep` | `agent.handleTurn` and `runOrReplay`, then `AppendJournal` |
| `ApplyStep` | `agent.runOrReplay` applying a recorded step, and `prepareResume` truncating to the recorded `ConvLen` |
| `ToolBegin` | `gateway.Gateway.Begin`, `beginFlight`, `begin`, `resumedByAnotherInstance`, and `interrupt` (`internal/gateway/gateway.go`) |
| `ToolExecute` | the tool body: an inline or sidecar call, or `beginRemote` for a service or HTTP MCP server |
| `ToolComplete` | `gateway.Gateway.Complete` and `journal.Ledger.Complete` (`internal/journal/ledger.go`) |
| `JournalToolStep` | `agent.executeTool` returning its output, which `AppendJournal` records |
| `PodForceDelete` | `session.Manager.pollPods` and `podMissing`, and an out-of-band pod deletion |
| `PodGracefulDelete` | `session.Manager.podDeleting` (a drain, eviction, preemption, or `kubectl delete`) |
| `ControlPlaneRestart` | a control-plane process restart; no single Go function, because the process is gone |
| `Adopt` | `session.Manager.adopt`, `adoptScan`, `adoptPods`, `adoptPodless`, `seedSequence`, and `registerAdopted` |
| `ReplacementPodResume` | `session.Manager.restartSoon` and `restart` (`internal/session/restart.go`), `restoreJournalThread`, and the runtime's `prepareResume` and `armResume` |
| `Stutter` | no Go function; a terminal session with nothing left to do |

The model's fixed turn script is user, model, idempotent tool, non-idempotent
tool, assistant. `ToolAt` names the tool of a step, so the call key is the same
`<session>/<turn>/<step>` the gateway derives.

## Invariants and liveness

| Property | Meaning | Tests that sample it |
|---|---|---|
| `TypeOK` | Every variable stays in its declared domain | none; model well-formedness |
| `UserMessageAtMostOnce` | A turn key is issued once, and a journaled step is applied to the conversation once, even across a restart | `TestControlPlaneRestartContinuesTurn`, `internal/journal` turn tests, `internal/session` journal tests |
| `TurnKeyReserved` | Every issued turn key is at or below the durable sequence floor | `internal/journal` `Reserve`/`HighWater` tests, `TestAdoptResumesPodlessSession` |
| `NonIdempotentAtMostOnce` | A non-idempotent tool body runs at most once per call key; a resume gets the interrupted result | `TestGatewayResumeTable`, `test/integration` resume cases, `TestSessionResumesAfterPodDeletion` |
| `CompletedReplays` | A `done` ledger record replays and never executes again | `TestGatewayResumeTable` (`done replays the result`), `TestSessionResumesAfterGracefulPodDeletion` |
| `IdempotentSameKey` | An idempotent call is keyed by the turn the control plane issued, so a re-dispatch reuses the key | `TestGatewayResumeTable` (`pending idempotent dispatches again`), `test/integration` idempotent resume |
| `NoResurrection` | A deleted session is never registered again by adoption | `TestDeleteRefusesLateAdoptionRegistration`, `TestRegisterAdoptedKeepsLiveSession` |
| `JournalPrefix` | A turn's journal is a prefix: no step is journaled before its predecessor | `verifyTurn` gap test, `internal/agent` replay tests |
| `ConversationBounded` | The conversation never runs ahead of the journal and trails it by at most the step being applied | `prepareResume` tests, `internal/agent` conversation replay tests |
| `Termination` | Every created session reaches `completed`, `expired`, or `failed` | `TestSessionTTLExpiry`, `TestDeleteSession`, `TestControlPlaneRestartContinuesTurn` |
| `Reclamation` | Every created session eventually has no pod, Secret, ConfigMap, NetworkPolicy, PVC, journal, or ledger | `TestShutdownWaitsForCleanup`, `TestDeleteSession` |

Liveness assumes weak fairness on the runtime's step actions and on the
control-plane maintenance actions: `Adopt`, `ReplacementPodResume`,
`TTLExpire`, `RuntimeFinalize`, `FallbackExport`, and `Cleanup`. It assumes no
fairness for user actions (`PostMessage`, `Close`, `UserDelete`) or for the
faults, which are bounded.

## Negative check

The model has two fault switches, both `TRUE` in the positive cfg:

- `LedgerBeforeDispatch`: record the call as pending before dispatching it.
- `InterruptOnResume`: turn a pending non-idempotent call from another runtime
  instance into a recorded interrupted result instead of a second dispatch.

`SessionResume_buggy.cfg` sets `LedgerBeforeDispatch = FALSE`. A pod kill
between the tool body and the result record then loses the pending record, and
the replacement dispatches the call again. TLC reports
`NonIdempotentAtMostOnce`. `SessionResume_interrupt_buggy.cfg` sets
`InterruptOnResume = FALSE` and must also report the same invariant. A negative
cfg fails the script if TLC passes it, and fails it if TLC reports a different
error, so an unrelated parse error cannot pass as the expected failure.

The check exists to show the positive run is not vacuous: the same model,
with one implemented guarantee removed, exhibits the bug the guarantee
prevents.

## Hub retention model

`spec/tla/HubRetention.tla` models one session stream in `internal/hub` and the
per-event cap in `internal/session`. It covers the memory bounds that a hostile
runtime can drive: one event's size, the retained thread's byte budget, the
event count cap, the inbox exemption, and the sequence contract.

| TLA+ action | Go code it models |
|---|---|
| `RuntimeAppend` | `session.Manager.EmitRuntimeEvents` and `boundRuntimeEvent` (`internal/session/lifecycle.go`), then `hub.Hub.Append` and `hub.stream.append` (`internal/hub/hub.go`) |
| `HumanPost` | `session.Manager.PostMessage` (`internal/session/lifecycle.go`) and `hub.Hub.Append` |
| `Consume` | the runtime's inbox cursor (`platformclient.Client.Inbox`, `internal/agent/runtime.go`); the hub keeps the inbox copy until its count cap drops it |
| `Trim` | `hub.stream.trimLocked` and `evictOldestLocked`: drop the oldest event while over `sessions.event_buffer` or `sessions.event_buffer_bytes` |
| `Stutter` | no Go function; a stream with no pending work |

| Property | Meaning | Tests that sample it |
|---|---|---|
| `TypeOK` | Every variable stays in its declared domain | none; model well-formedness |
| `MonotonicSeq` | Every append assigns a larger sequence number | `FuzzSequenceInvariants`, `FuzzHubAppendBudget` |
| `EventCapEnforced` | No retained event exceeds `sessions.max_event_bytes` | `TestEmitRuntimeEventsTruncatesOversizedFields`, `FuzzEventBound` |
| `RetainedWithinBudget` | Retained thread bytes never exceed `sessions.event_buffer_bytes` | `TestByteBudgetEvictsOldestEvents`, `FuzzHubAppendBudget` |
| `ContiguousSuffix` | Retained sequences are a contiguous suffix ending at the latest sequence, so a replay shows a gap instead of a hole | `TestByteBudgetEvictsOldestEvents`, `FuzzHubAppendBudget`, `FuzzSequenceInvariants` |
| `InboxNeverEvictedByRuntime` | A runtime append never removes an unconsumed human message from the inbox | `TestInboxSurvivesRuntimeEventFlood`, `FuzzHubAppendBudget` |

The positive cfg uses eight appends over sizes `{1, 2, 3}`, a two-event cap,
and a four-byte budget: 4548 states generated, 1147 distinct, under a second.
`HubRetention_evict_inbox_buggy.cfg` sets `ExemptInbox = FALSE`, so trimming the
thread also consumes the matching inbox entry; TLC must report
`InboxNeverEvictedByRuntime` (103 states generated, 36 distinct).
`HubRetention_no_cap_buggy.cfg` sets `CapEvents = FALSE`, so an event of size
`MaxEvent+1` is retained; TLC must report `EventCapEnforced` (54 states
generated, 27 distinct).

The model abstracts these details:

- Event sizes are abstract bytes from `Sizes`, not JSON or Go string lengths.
- The inbox is a sequence of sequences, not a separate slice copy, and its
  count cap is outside the model. `Consume` removes the oldest message when the
  runtime reads it; the real hub keeps the copy until `sessions.event_buffer`
  drops it.
- A human message is one byte. `Manager.PostMessage` already caps it at 32 KiB.
- The dedup-key table is outside the model. The Go hub drops a `StepKey` when
  its event is evicted, which `TestEvictedStepKeyCanBeAppendedAgain` samples.
- `Restore` of a journaled event is not an action. The journal bounds an entry
  through `journal.max_entry_bytes`, and the hub byte budget still applies to
  the restored event, but `EventCapEnforced` covers runtime appends only.
- Subscribers and live fan-out are outside the model; it checks retention, not
  channel delivery.

## Fuzz targets for the bounds

- `FuzzHubAppendBudget` (`internal/hub`) drives appends, restores, and replays,
  and asserts the byte budget, the count cap, the contiguous suffix, and that
  the inbox keeps its human message.
- `FuzzEventBound` (`internal/session`) asserts that a shortened text or
  payload string stays within the cap, keeps a prefix of the input, and stays
  valid UTF-8 when the input was valid. Seeds sit on the cap and on a
  multibyte rune that straddles it.
- `FuzzGitHubParse`, `FuzzGenericParse`, and `FuzzDatadogParse` assert that a
  derived title stays within `session.MaxTitleBytes` and is valid UTF-8, with
  boundary seeds at the cap, one byte over, and a straddling rune.

## What is abstracted away

- bbolt writes are atomic. A journal or ledger write either lands or not.
- One control-plane replica. The model has no leader election or two writers.
- No network partitions or Kubernetes API eventual consistency. An API call
  either succeeds or the runtime's retry budget decides the outcome.
- Timeouts are abstract. A close grace or connect timeout is an action that is
  enabled forever rather than after a fixed delay.
- The conversation is a step count, not bytes. `ConvLen` is one unit per step.
- Tool results are opaque. The model records `done`, `pending`, or
  `interrupted`; it does not compare result bytes or argument hashes.
- Plugin rate limits, the in-flight cap, grants, and audit records are outside
  the model.
- Built-in tools are outside the ledger and may run again after a resume; the
  model has only ledgered tool calls.
- Streaming deltas are not journaled, so a retried step can repeat deltas; the
  model does not represent deltas.
- A sequence block is the constant `Block`, and one `PostMessage` performs one
  reservation. The real control plane also reserves for non-turn events.
- The model keeps an unstarted inbox message across a control-plane restart.
  The real hub is in memory, so a message the runtime never read, including
  the initial prompt of a session whose pod has not connected, can be lost.
  The model does not cover that loss; it is tracked in
  [#59](https://github.com/hayfever/mayfly/issues/59), whose fix must also drop
  this assumption from the model.
- The DELETE tombstone is treated as durable. The real tombstone is in memory;
  the record deletion and synchronous object cleanup make the removal stick.
- The `deleted` terminal state is `failed` with reason `deleted`, as in the
  code; the model does not add a separate `deleted` status.

## What is not verified

The models are checked against the design, not against the Go code. Nothing in
the repository shows that a Go execution is a behavior of a model, and the
reverse direction (a model behavior that the Go code permits) is also
unchecked. Trace conformance, for example by recording a Go test's journal and
ledger operations and replaying them through the model, is future work.

A fuzz target checks the input handling of the package that adds it; the
targets do not check the lifecycle. `hack/fuzz.sh` discovers targets
automatically; a branch with no fuzz target exits 0 and says so.

## Chart RBAC property check

`test/chart/rbac_test.go` checks the chart RBAC over the whole configuration
space that changes it. It runs `helm template` for every equality partition of
the release namespace, `config.sessions.namespace`, and
`config.plugins.namespace`:

| Partition | Release | Sessions | Plugins |
|---|---|---|---|
| all equal | `mayfly` | `mayfly` | `mayfly` |
| release equals sessions | `mayfly` | `mayfly` | `plugins` |
| release equals plugins | `mayfly` | `sessions` | `mayfly` |
| sessions equals plugins | `mayfly` | `sessions` | `sessions` |
| all distinct | `mayfly` | `sessions` | `plugins` |

Each partition is crossed with `rbac.create` set to true and false, and with
the default and a custom `serviceAccount.controlPlaneName`. The test also
renders the chart with default values and with `e2e/values.yaml`.

The check parses the multi-document YAML and asserts these properties:

- Coverage: every `(namespace, apiGroup, resource, verb)` row in the RBAC
  table in [deployment.md](deployment.md#rbac-per-namespace) is granted by a
  Role in that namespace.
- Least privilege: no Role grants a permission outside that table. The pods
  `watch` verb is the one tolerated extra, because earlier chart releases
  grant it.
- Every namespace that needs a Role has exactly one Role and one RoleBinding
  with the chart fullname.
- Every RoleBinding refers to a Role in the same namespace and binds exactly
  one subject: the control-plane ServiceAccount in the release namespace.
- No `(kind, namespace, name)` object is rendered twice.
- No cluster-scoped RBAC: the chart renders no `ClusterRole` and no
  `ClusterRoleBinding`, and no binding names a `ClusterRole` in its `roleRef`.
  A `RoleBinding` to a `ClusterRole` is rejected too, because it grants
  cluster-wide permissions and bypasses the Role checks.
- `rbac.create=false` renders no Role and no RoleBinding.

Run it with:

```bash
MAYFLY_REQUIRE_HELM=1 go test ./test/chart/...
```

The test skips when `helm` is not on `PATH`, unless `MAYFLY_REQUIRE_HELM=1`
makes the missing binary a failure. `MAYFLY_CHART_DIR` points the check at
another chart directory.

`TestRBACPrefixTemplateIsRejected` is the negative check. It copies the chart,
replaces `templates/rbac.yaml` with the pre-fix template in
`test/chart/testdata/rbac-prefix.yaml`, and asserts the check fails. Only the
all-equal partition passes; the other four partitions fail on the missing
sessions Role or on a Role in the release namespace that the control plane does
not use. The test fails if the pre-fix template passes every partition, so a
check that no longer bites is visible.

`TestRBACClusterScopedIsRejected` is the negative check for cluster-scoped
RBAC. It copies the chart and injects one extra template at a time. The first
fixture is a `ClusterRole` with `apiGroups`, `resources`, and `verbs` set to
`"*"` plus a `ClusterRoleBinding` that binds it to the control-plane
ServiceAccount. The second fixture is a `RoleBinding` whose `roleRef` names a
`ClusterRole` that the chart does not render. The check must reject each
fixture in every partition, with `rbac.create` set to true and false, and must
name the offending object.

`TestRBACDocsTableMatchesSpec` parses the RBAC table in
[deployment.md](deployment.md#rbac-per-namespace) and compares it both ways
against the Go grant sets. The sessions rows must equal the union of the
session grants, the retained `watch` verb, and the plugin grants. The plugins
rows must equal the plugin grants. A control-plane call added to one side fails
the test until the other side matches.

The check does not prove that a cluster accepts the RBAC. It reads rendered
manifests; it does not contact an API server and does not exercise the
authorization decision. The e2e suite covers the cluster side when it creates
a session. The check does not cover chart values that do not change RBAC.
