# Formal verification

Two checks cover behavior that unit tests can only sample: TLA+ models of the
session lifecycle, hub retention, and queue poison recovery, and native Go fuzz
targets. A third check covers the chart RBAC over its whole namespace
configuration space. Each has a script or command and a CI job.

| Check | Script | CI job |
|---|---|---|
| TLC on `SessionResume.tla`, `HubRetention.tla`, and `QueuePoison.tla` | `hack/tlc.sh` | `tlc` in [verify.yml](../.github/workflows/verify.yml), on pull requests |
| TLC with two sessions | `hack/tlc.sh --deep` | `tlc-deep` in verify.yml, nightly and on demand |
| Every fuzz target | `hack/fuzz.sh` | `fuzz-nightly` in verify.yml |
| Chart RBAC property check | `MAYFLY_REQUIRE_HELM=1 go test ./test/chart/...` | `chart` step of the `test` job in [ci.yml](../.github/workflows/ci.yml) |

`hack/tlc.sh` needs a JDK 17 or newer (`java` on `PATH`, or `JAVA`). It
downloads the pinned, checksummed `tla2tools.jar` once into
`${XDG_CACHE_HOME:-$HOME/.cache}/mayfly/tla`. `hack/fuzz.sh` needs only Go.

## What is verified

The model is `spec/tla/SessionResume.tla`. It follows the implemented protocol,
including the parts that are easy to get wrong:

- a message is written to a durable pending-inbox record before its sender is
  acknowledged; the record is keyed by the hub sequence that becomes the turn
  key and is removed when the turn's step 0 is journaled;
- a user message takes its turn key from the in-memory hub, after the journal
  has reserved that sequence block;
- the runtime journals a step before applying it to the conversation;
- the gateway records a tool call before it dispatches, and applies the resume
  table on the next begin;
- a pod death loses only in-memory state; the journal, ledger, sequence floor,
  session record, and workspace survive;
- a control-plane restart loses the hub, in-flight begins, and registered
  instances; adoption seeds the hub above the durable floor, restores the
  thread, redelivers every pending message under its reserved turn key, and
  refuses a session a DELETE removed;
- a replacement pod gets a new runtime instance and replays recorded steps
  without repeating a recorded tool result;
- finalize and cleanup run once, and cleanup removes the pod, Secret,
  ConfigMap, NetworkPolicy, PVC, journal, and ledger.

`SessionResume.cfg` checks every invariant and all liveness properties with
one session, an initial prompt, one later message, and one control-plane
restart. It leaves out pod kills and replacements to keep the liveness check
fast. `SessionResume_resume.cfg` covers the runtime-resume path with one pod
kill and one replacement, safety only. The positive run holds 34,840 distinct
states and finishes in about 12 seconds; the resume run holds 2,429 and
finishes in about a second. Sessions do not share mutable state, so
one session covers every property; a second session only multiplies the
liveness state space.

`deep/SessionResume_two_sessions.cfg` runs nightly. It checks the safety
invariants with two sessions, one turn each, and one control-plane restart
that hits both at once, without pod kills or replacements: 51,066,742 states
generated, 6,127,026 distinct, and about 10.5 minutes with `-workers auto` on
this machine. `hack/tlc.sh --deep` took 14m44s including its startup and log
handling. Liveness with two sessions is out of reach at any useful bound.

`SessionResume_buggy.cfg`, `SessionResume_interrupt_buggy.cfg`,
`SessionResume_volatile_inbox_buggy.cfg`, and
`SessionResume_redeliver_dup_buggy.cfg` are the negative checks: they must
fail on the invariant named in the cfg. See
[spec/tla/README.md](../spec/tla/README.md) for the cfg convention.

## Action to Go map

| TLA+ action | Go code it models |
|---|---|
| `Create` | `session.Manager.Create` (`internal/session/manager.go`), `persistPending` for the initial prompt (`internal/session/pending.go`), `runner.Kubernetes.Start`, `k8s.Client.CreateSession` |
| `PostMessage` | `session.Manager.PostMessage`, `reserveSequence`, and `persistPending` (`internal/session/lifecycle.go`, `internal/session/pending.go`), `journal.AddPending` (`internal/journal/pending.go`), `journal.Reserve` (`internal/journal/journal.go`) |
| `Close` | `session.Manager.Close` |
| `TTLExpire` | `session.Manager.reap` (TTL and idle close) |
| `UserDelete` | `session.Manager.Delete` (tombstone, failed with reason `deleted`) |
| `RuntimeFinalize` | `session.Manager.Complete` and `finalize` (`internal/session/lifecycle.go`) |
| `FallbackExport` | `session.Manager.fallback` and `failFallback` |
| `Cleanup` | `session.Manager.cleanup` and `deleteJournal`, `runner.Kubernetes.Stop` |
| `RuntimeStartTurn` | the runtime's inbox loop (`internal/agent/runtime.go`: `Run`) and `Manager.RuntimeConnected` |
| `JournalUserStep` | `agent.handleTurn` step 0 and `session.Manager.AppendJournal` (`internal/session/journal.go`); the journal store deletes the pending record in the same transaction (`internal/journal/bbolt.go`, `memory.go`) |
| `ModelCall` | `agent.executeModel` |
| `JournalModelStep` | `agent.handleTurn` and `runOrReplay`, then `AppendJournal` |
| `ApplyStep` | `agent.runOrReplay` applying a recorded step, and `prepareResume` truncating to the recorded `ConvLen` |
| `ToolBegin` | `gateway.Gateway.Begin`, `beginFlight`, `begin`, `resumedByAnotherInstance`, and `interrupt` (`internal/gateway/gateway.go`) |
| `ToolExecute` | the tool body: an inline or sidecar call, or `beginRemote` for a service or HTTP MCP server |
| `ToolComplete` | `gateway.Gateway.Complete` and `journal.Ledger.Complete` (`internal/journal/ledger.go`) |
| `JournalToolStep` | `agent.executeTool` returning its output, which `AppendJournal` records |
| `PodForceDelete` | `session.Manager.pollPods` and `podMissing`, and an out-of-band pod deletion |
| `PodGracefulDelete` | `session.Manager.podDeleting` (a drain, eviction, preemption, or `kubectl delete`) |
| `ControlPlaneRestart` | a control-plane process restart; no single Go function, because the process is gone. The hub and registered instances are lost; the journal, including the pending inbox, survives |
| `Adopt` | `session.Manager.adopt`, `adoptScan`, `adoptPods`, `adoptPodless`, `seedSequence`, `registerAdopted`, and `redeliverPending` (`internal/session/lifecycle.go`, `internal/session/pending.go`), `hub.Redeliver` (`internal/hub/hub.go`) |
| `ReplacementPodResume` | `session.Manager.restartSoon` and `restart` (`internal/session/restart.go`), `restoreJournalThread`, and the runtime's `prepareResume` and `armResume` |
| `Stutter` | no Go function; a terminal session with nothing left to do |

The model's fixed turn script is user, model, idempotent tool, non-idempotent
tool, assistant. `ToolAt` names the tool of a step, so the call key is the same
`<session>/<turn>/<step>` the gateway derives.

## Invariants and liveness

| Property | Meaning | Tests that sample it |
|---|---|---|
| `TypeOK` | Every variable stays in its declared domain | none; model well-formedness |
| `UserMessageAtMostOnce` | A message is started under one turn key, a turn key is issued once, and a journaled step is applied to the conversation once, even across a restart | `TestControlPlaneRestartContinuesTurn`, `internal/journal` turn tests, `internal/session` journal tests |
| `AcceptedNotLost` | Every acknowledged message is still pending, has a journaled step, or belongs to a session that ended | `TestPostMessageSurvivesRestartBeforeTurn`, `TestJournaledStepZeroDropsPendingMessage`, `TestCreatePersistsInitialPrompt`, `TestControlPlaneRestartKeepsQueuedMessage` |
| `TurnKeyReserved` | Every issued turn key is at or below the durable sequence floor | `internal/journal` `Reserve`/`HighWater` tests, `TestAdoptResumesPodlessSession` |
| `NonIdempotentAtMostOnce` | A non-idempotent tool body runs at most once per call key; a resume gets the interrupted result | `TestGatewayResumeTable`, `test/integration` resume cases, `TestSessionResumesAfterPodDeletion` |
| `CompletedReplays` | A `done` ledger record replays and never executes again | `TestGatewayResumeTable` (`done replays the result`), `TestSessionResumesAfterGracefulPodDeletion` |
| `IdempotentSameKey` | An idempotent call is keyed by the turn the control plane issued, so a re-dispatch reuses the key | `TestGatewayResumeTable` (`pending idempotent dispatches again`), `test/integration` idempotent resume |
| `NoResurrection` | A deleted session is never registered again by adoption | `TestDeleteRefusesLateAdoptionRegistration`, `TestRegisterAdoptedKeepsLiveSession` |
| `JournalPrefix` | A turn's journal is a prefix: no step is journaled before its predecessor | `verifyTurn` gap test, `internal/agent` replay tests |
| `ConversationBounded` | The conversation never runs ahead of the journal and trails it by at most the step being applied | `prepareResume` tests, `internal/agent` conversation replay tests |
| `Termination` | Every created session reaches `completed`, `expired`, or `failed` | `TestSessionTTLExpiry`, `TestDeleteSession`, `TestControlPlaneRestartContinuesTurn` |
| `AcceptedHandled` | Every acknowledged message in a live session is eventually started, or the session ends first | `TestPostMessageSurvivesRestartBeforeTurn`, `TestControlPlaneRestartKeepsQueuedMessage` |
| `Reclamation` | Every created session eventually has no pod, Secret, ConfigMap, NetworkPolicy, PVC, journal, or ledger | `TestShutdownWaitsForCleanup`, `TestDeleteSession` |

Liveness assumes weak fairness on the runtime's step actions and on the
control-plane maintenance actions: `Adopt`, `ReplacementPodResume`,
`TTLExpire`, `RuntimeFinalize`, `FallbackExport`, and `Cleanup`. It assumes no
fairness for user actions (`PostMessage`, `Close`, `UserDelete`) or for the
faults, which are bounded.

## Negative check

The model has four fault switches. The positive cfg uses the safe value of each:

- `LedgerBeforeDispatch`: record the call as pending before dispatching it.
- `InterruptOnResume`: turn a pending non-idempotent call from another runtime
  instance into a recorded interrupted result instead of a second dispatch.
- `DurableInbox`: write the pending record before acknowledging a message.
- `RedeliverNewKey`: adoption starts a pending message under a new turn key
  instead of the reserved one.

`SessionResume_buggy.cfg` sets `LedgerBeforeDispatch = FALSE`. A pod kill
between the tool body and the result record then loses the pending record, and
the replacement dispatches the call again. TLC reports
`NonIdempotentAtMostOnce`. `SessionResume_interrupt_buggy.cfg` sets
`InterruptOnResume = FALSE` and must also report the same invariant.
`SessionResume_volatile_inbox_buggy.cfg` sets `DurableInbox = FALSE`: a
control-plane restart before the runtime starts the turn loses an acknowledged
message, and TLC must report `AcceptedNotLost` (2 states generated, 2 distinct).
`SessionResume_redeliver_dup_buggy.cfg` sets `RedeliverNewKey = TRUE`: adoption
starts the same accepted message as a second turn, and TLC must report
`UserMessageAtMostOnce` (222 states generated, 111 distinct). A negative cfg fails the script if TLC passes it, and
fails it if TLC reports a different error, so an unrelated parse error cannot
pass as the expected failure.

The check exists to show the positive run is not vacuous: the same model,
with one implemented guarantee removed, exhibits the bug the guarantee
prevents.

## Hub retention model

`spec/tla/HubRetention.tla` models one session stream in `internal/hub` and the
per-event cap in `internal/session`. It covers the memory and ordering behavior
that a hostile runtime can drive: one event's size, the retained thread's byte
budget, the thread and inbox count caps, the inbox exemption, the dedup-key
table, and the sequence contract.

| TLA+ action | Go code it models |
|---|---|
| `RuntimeAppend` | `session.Manager.EmitRuntimeEvents` and `boundRuntimeEvent` (`internal/session/lifecycle.go`), then `hub.Hub.Append` and `hub.stream.append` (`internal/hub/hub.go`) |
| `RestoreAppend` | `session.Manager.restoreJournalThread` (`internal/session/restart.go`) and `hub.Hub.Restore` (`internal/hub/hub.go`); the per-event cap applies before the append |
| `HumanPost` | `session.Manager.PostMessage` (`internal/session/lifecycle.go`) and `hub.Hub.Append` |
| `DedupRetry` | the reject branch of `hub.stream.append`: an event whose `StepKey` is remembered returns `Seq` 0 and changes no state |
| `Consume` | the runtime's inbox cursor (`platformclient.Client.Inbox`, `internal/agent/runtime.go`); the hub keeps the inbox copy until its count cap drops it |
| `Trim` | `hub.stream.trimLocked` and `evictOldestLocked`: drop the oldest event while over `sessions.event_buffer` or `sessions.event_buffer_bytes` |
| `Stutter` | no Go function; a stream with no pending work |

| Property | Meaning | Tests that sample it |
|---|---|---|
| `TypeOK` | Every variable stays in its declared domain | none; model well-formedness |
| `MonotonicSeq` | Every append assigns a larger sequence number | `FuzzSequenceInvariants`, `FuzzHubAppendBudget` |
| `EventCapEnforced` | No retained event exceeds `sessions.max_event_bytes`, whether it came from the runtime or the journal | `TestEmitRuntimeEventsTruncatesOversizedFields`, `TestRestoreJournalThreadBoundsOversizedEvents`, `FuzzEventBound` |
| `RetainedWithinBudget` | Retained thread bytes never exceed `sessions.event_buffer_bytes` | `TestByteBudgetEvictsOldestEvents`, `FuzzHubAppendBudget` |
| `ContiguousSuffix` | Retained sequences are a contiguous suffix ending at the latest sequence, so a replay shows a gap instead of a hole | `TestByteBudgetEvictsOldestEvents`, `FuzzHubAppendBudget`, `FuzzSequenceInvariants` |
| `InboxNeverEvictedByRuntime` | A runtime append never removes an unconsumed human message from the inbox | `TestInboxSurvivesRuntimeEventFlood`, `FuzzHubAppendBudget` |
| `InboxWithinCap` | The inbox holds at most `sessions.event_buffer` entries, and a later human post drops the oldest | `TestInboxCountCapDropsOldest` |
| `KeysMatchRetained` | A remembered `StepKey` belongs to a retained event, so eviction frees the key | `TestEvictedStepKeyCanBeAppendedAgain` |
| `NoDuplicateRetainedKey` | Two retained events never share a `StepKey` | `TestAppendDropsDuplicateStepKey` |

The positive cfg uses eight appends over sizes `{1, 2, 3}`, a two-event thread
cap, a four-byte budget, a two-entry inbox cap, and one dedup key: 19126 states
generated, 2458 distinct, about a second.
`HubRetention_evict_inbox_buggy.cfg` sets `ExemptInbox = FALSE`, so trimming the
thread also consumes the matching inbox entry; TLC must report
`InboxNeverEvictedByRuntime` (538 states generated, 112 distinct).
`HubRetention_no_cap_buggy.cfg` sets `CapEvents = FALSE`, so an event of size
`MaxEvent+1` is retained; TLC must report `EventCapEnforced` (43 states
generated, 24 distinct). `HubRetention_inbox_overflow_buggy.cfg` sets
`CapInbox = FALSE`, so two human posts exceed a one-entry inbox; TLC must
report `InboxWithinCap` (283 states generated, 94 distinct).
`HubRetention_key_leak_buggy.cfg` sets `EvictKeys = FALSE`, so a key outlives
its evicted event; TLC must report `KeysMatchRetained` (40 states generated, 21
distinct).

The model abstracts these details:

- Event sizes are abstract bytes from `Sizes`, not JSON or Go string lengths.
- The inbox is a sequence of sequence numbers, not a separate slice copy. A
  human message is one byte; `Manager.PostMessage` caps the text at 32 KiB.
  `Consume` removes the oldest message when the runtime reads it, while the
  real hub keeps the copy until a later post drops it or eviction removes the
  thread copy.
- `MaxInbox` is a constant separate from `MaxEvents`, so a cfg can shrink one
  cap. The Go hub uses `sessions.event_buffer` for both.
- `Keys` is a finite set of abstract step keys, and no event holds more than
  one. A real `StepKey` is a string such as `11/1/assistant`.
- `DedupRetry` spends one append step from `MaxAppends`, while the Go reject
  branch costs nothing. The step only keeps TLC's state space finite.
- The journal write path is outside the model. `RestoreAppend` models the
  capped event that reaches `hub.Restore`, not the journal entry itself.
- Subscribers and live fan-out are outside the model; it checks retention, not
  channel delivery.

## Queue poison-job model

`spec/tla/QueuePoison.tla` models one queue with a poison job and a healthy
job. It covers the crash loop that a panic in a queue handler used to cause:
the worker catches the panic and Nacks the job, a crash keeps a counted
attempt, the lease expires, the worker restarts, and a job that used all its
attempts is buried in the dead set.

| TLA+ action | Go code it models |
|---|---|
| `Claim` | `queue.Worker.Run` and `Store.Claim` (`internal/queue/worker.go`, `memory.go`, `bbolt.go`, `redis.go`). The claim increments `Job.Attempts`; a job at `queue.max_attempts` is written to the dead set instead of leased |
| `Handle` | `queue.Worker.invoke` and `queue.Worker.reportPanic` (`internal/queue/worker.go`), then `Store.Nack`. A recovered panic leaves the attempt counted, logs the stack, and increments `mayfly_panics_total{component="queue"}` |
| `ExpireLease` | the visibility timeout (`queue.visibility_timeout`). The next `Claim` returns the job to a worker. The model does not count an attempt here, because `Claim` does |
| `Restart` | a control-plane process restart after a crash; no single Go function, because the process is gone |
| `Stutter` | no Go function; a queue with no ready job |

`RecoverPanics` and `CountAttemptOnClaim` are the fault switches. Both are
`TRUE` in the positive cfg. `RecoverPanics = FALSE` models a handler panic that
ends the process, which is the pre-fix `Worker.handle`. `CountAttemptOnClaim =
FALSE` models the old counter, which incremented only on `Nack`; a reclaim after
a crash then kept the same attempt count forever.

| Property | Meaning | Tests that sample it |
|---|---|---|
| `TypeOK` | Every variable stays in its declared domain | none; model well-formedness |
| `AttemptsWithinMax` | No job exceeds `queue.max_attempts` handler attempts | `TestStoreRetryThenDead`, `TestStoreExpiredFinalAttemptIsBuried` |
| `NoLostJob` | Every job stays in exactly one of `ready`, `leased`, `done`, or `dead` | `TestStoreRetryThenDead`, `TestStoreRecordsAndDeadLetters`, `TestStoreExpiredFinalAttemptIsBuried` |
| `NoPanicCrash` | A handler panic never crashes the worker | `TestWorkerRecoversHandlerPanicAndBuriesPoisonJob`, `TestDispatchRecoversPanicAndReleasesShutdown` |
| `CrashesBounded` | A poison job crashes the process at most `MaxAttempts` times, even when the panic is not recoverable | `TestStoreExpiredFinalAttemptIsBuried` |
| `HealthyJobsProgress` | A healthy job is eventually done while a poison job retries or is buried | `TestWorkerRecoversHandlerPanicAndBuriesPoisonJob` |

`QueuePoison.cfg` uses `MaxAttempts = 2`, one poison job, and one healthy job.
It checks every invariant and `HealthyJobsProgress`: 38 states generated, 15
distinct, under one second.

`QueuePoison_no_recover_buggy.cfg` sets `RecoverPanics = FALSE` and must report
`NoPanicCrash` (13 states generated, 9 distinct). The panic ends the process,
so every other job stops with it. `QueuePoison_no_claim_count_buggy.cfg` sets
`RecoverPanics = FALSE` and `CountAttemptOnClaim = FALSE` and must report
`CrashesBounded` (79 states generated, 33 distinct). A reclaim after a crash
keeps the same attempt count, so the poison job crashes without bound. That cfg
omits `NoPanicCrash`, because the first unrecovered panic violates it before the
crash bound is reachable.

The model abstracts these details:

- Time, backoff, and the visibility timeout are actions, not durations. A
  reclaim can happen immediately after a crash, and this only makes the
  negative cfg easier to reach.
- The model has one worker but permits several leased jobs. `Claim(j)`
  fairness is per job, so `HealthyJobsProgress` does not rely on a schedule.
- A job payload is its identity and its attempt count. The panic value, the
  stack, and the dead-letter error string are outside the model.
- One poison kind and one handler. The real queue has session and source jobs;
  both reach `Worker.invoke` and the same recover.
- The memory, bbolt, and Redis stores have one model. The conformance suite
  runs `TestStoreExpiredFinalAttemptIsBuried` against each backend that the
  test environment provides.
- `Retry` and `Delete` on the dead-letter admin API are outside the model.

## Fuzz targets for the bounds

- `FuzzHubAppendBudget` (`internal/hub`) drives appends, restores, and replays,
  and asserts the byte budget, the count cap, the contiguous suffix, and that
  the inbox keeps its human message.
- `FuzzPendingJSON` (`internal/journal`) decodes the durable pending-inbox
  record that adoption reads after a restart. Arbitrary bytes must not panic, a
  record that decodes must re-encode stably, and the seeds cover a valid
  record, a sequence mismatch, a non-message type, a missing time, `null`, and
  truncated JSON. A 60-second run completed 4,663,166 executions.
- `FuzzEventBound` (`internal/session`) asserts that a shortened text or
  payload string stays within the cap, keeps a prefix of the input, and stays
  valid UTF-8 when the input was valid. Seeds sit on the cap and on a
  multibyte rune that straddles it.
- `FuzzGitHubParse`, `FuzzGenericParse`, and `FuzzDatadogParse` assert that a
  derived title stays within `session.MaxTitleBytes` and is valid UTF-8, with
  boundary seeds at the cap, one byte over, and a straddling rune.
  `FuzzGitHubParse` applies the UTF-8 assertion to every input, and one seed
  carries a non-UTF-8 `X-GitHub-Event` header.
- `FuzzWorkerHandlerPanic` (`internal/queue`) runs a handler that panics on
  fuzz-chosen bytes. It asserts that the worker recovers, counts
  `max_attempts` attempts, buries the poison job, and still completes a healthy
  job. Seeds cover an empty value, a normal value, and non-UTF-8 bytes. A
  60-second run made 13,622 executions.
- `FuzzHTTPJSONProjection` (`internal/sources`) decodes arbitrary upstream JSON
  and projects it with arbitrary configured paths. It asserts that a bad path or
  an unexpected shape returns an error or an error document, and that the error
  and diff bounds hold. A 60-second run made 1,428,289 executions.
- `FuzzRunArtifactDecode` (`internal/workflow`) feeds arbitrary bytes to the
  source-run resume path. A torn, wrong-run, unknown-field, or newer-version
  artifact must return an error, never a panic. A 60-second run made 44,824
  executions.

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
- The pending-inbox write is modeled as part of the atomic `PostMessage` and
  `Create` actions. The model does not represent the short interval between the
  hub append and the journal write; a crash there loses a message the sender
  was not yet acknowledged.
- The runtime's in-memory message queue is not modeled. The model assumes the
  runtime eventually reads a redelivered pending message; the Go runtime
  applies backpressure instead of dropping it.
- The thread's display copy of a redelivered message gets a new sequence in the
  code. The model tracks only the reserved turn key.
- `started` counts the turn keys an accepted message was started under. The
  `RedeliverNewKey` fault increments it to model reissuing a key; the model does
  not explore the second turn's full script.
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
