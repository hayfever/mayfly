# Formal verification

Two checks cover behavior that unit tests can only sample: TLA+ models of the
session lifecycle, hub retention, request-body deadlines, queue poison
recovery, schedule time zones, release gating, and LLM token budgets, plus
native Go fuzz targets. A third check covers
the chart RBAC over its whole namespace configuration space. A fourth check
covers the k3d image-name matcher with fixture JSON. A fifth check covers the
image scan policy across every workflow. Each has a script or command and a CI
job.

| Check | Script | CI job |
|---|---|---|
| TLC on `SessionResume.tla`, `HubRetention.tla`, `BodyReadDeadline.tla`, `QueuePoison.tla`, `AuthHardening.tla`, `CronTimezone.tla`, `ReleaseGating.tla`, and `TokenBudget.tla` | `hack/tlc.sh` | `tlc` in [verify.yml](../.github/workflows/verify.yml), on pull requests |
| TLC with two sessions and three budget callers | `hack/tlc.sh --deep` | `tlc-deep` in verify.yml, nightly and on demand |
| Every fuzz target | `hack/fuzz.sh` | `fuzz-nightly` in verify.yml |
| Chart RBAC property check | `MAYFLY_REQUIRE_HELM=1 go test ./test/chart/...` | `chart` step of the `test` job in [ci.yml](../.github/workflows/ci.yml) |
| k3d image-name matcher fixtures | `hack/test-k3d-images.sh` | `shell` job in [ci.yml](../.github/workflows/ci.yml) |
| Workflow and image scan policy conformance | `go test ./test/imagepolicy` | `test` job in ci.yml (`make test`) |
| Image scan exit codes with a fake trivy | `hack/test-image-scan.sh` | `shell` job in ci.yml |

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
states and finishes in about 18 seconds; the resume run holds 2,429 and
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

The [token budget model](#token-budget-model) maps its own actions and
invariants to `internal/session/budget.go` and `internal/broker` in its section
below.

## Invariants and liveness

| Property | Meaning | Tests that sample it |
|---|---|---|
| `TypeOK` | Every variable stays in its declared domain | none; model well-formedness |
| `UserMessageAtMostOnce` | A message is started under one turn key, a turn key is issued once, and a journaled step is applied to the conversation once, even across a restart | `TestControlPlaneRestartContinuesTurn`, `internal/journal` turn tests, `internal/session` journal tests |
| `AcceptedNotLost` | Every acknowledged message is still on the live inbox, durable in the pending inbox, has a journaled step, or belongs to a session that ended | `TestPostMessageSurvivesRestartBeforeTurn`, `TestJournaledStepZeroDropsPendingMessage`, `TestCreatePersistsInitialPrompt`, `TestAdoptResumesPodlessSession`, `TestAdoptKeepsArtifactsAgentAndIdempotencyKey`, `TestControlPlaneRestartKeepsQueuedMessage` |
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
message, and TLC must report `AcceptedNotLost` with a counterexample that runs
`Create` then `ControlPlaneRestart`. The invariant counts the live inbox, so a
missing pending record alone is not a violation; only the restart that drops it is.
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
budget, the inbox's byte and count budgets, the inbox exemption, the dedup-key
table, and the sequence contract.

| TLA+ action | Go code it models |
|---|---|
| `RuntimeAppend` | `session.Manager.EmitRuntimeEvents` and `boundRuntimeEvent` (`internal/session/lifecycle.go`), then `hub.Hub.Append` and `hub.stream.append` (`internal/hub/hub.go`) |
| `RestoreAppend` | `session.Manager.restoreJournalThread` (`internal/session/restart.go`) and `hub.Hub.Restore` (`internal/hub/hub.go`); the per-event cap applies before the append |
| `HumanPost` | `session.Manager.PostMessage` (`internal/session/lifecycle.go`), `hub.Hub.Append`, and `hub.stream.trimInboxLocked`: the count cap and `sessions.inbox_buffer_bytes` drop the oldest inbox entries |
| `DedupRetry` | the reject branch of `hub.stream.append`: an event whose `StepKey` is remembered returns `Seq` 0 and changes no state |
| `Consume` | the runtime's inbox cursor (`platformclient.Client.Inbox`, `internal/agent/runtime.go`); the hub keeps the inbox copy until its byte or count cap drops it |
| `Trim` | `hub.stream.trimLocked` and `evictOldestLocked`: drop the oldest event while over `sessions.event_buffer` or `sessions.event_buffer_bytes` |
| `Stutter` | no Go function; a stream with no pending work |

| Property | Meaning | Tests that sample it |
|---|---|---|
| `TypeOK` | Every variable stays in its declared domain | none; model well-formedness |
| `MonotonicSeq` | Every append assigns a larger sequence number | `FuzzSequenceInvariants`, `FuzzHubAppendBudget` |
| `EventCapEnforced` | No retained event exceeds `sessions.max_event_bytes`, whether it came from the runtime or the journal | `TestEmitRuntimeEventsTruncatesOversizedFields`, `TestRestoreJournalThreadBoundsOversizedEvents`, `FuzzEventBound` |
| `RetainedWithinBudget` | Retained thread bytes never exceed `sessions.event_buffer_bytes` | `TestByteBudgetEvictsOldestEvents`, `FuzzHubAppendBudget` |
| `ContiguousSuffix` | Retained sequences are a contiguous suffix ending at the latest sequence, so a replay shows a gap instead of a hole | `TestByteBudgetEvictsOldestEvents`, `FuzzHubAppendBudget`, `FuzzSequenceInvariants` |
| `InboxNeverEvictedByRuntime` | A runtime append never removes an unconsumed human message from the inbox | `TestInboxSurvivesRuntimeEventFlood`, `FuzzHubAppendBudget`, `FuzzHubInboxBudget` |
| `InboxWithinCap` | The inbox holds at most `sessions.event_buffer` entries, and a later human post drops the oldest | `TestInboxCountCapDropsOldest`, `FuzzHubInboxBudget` |
| `InboxWithinBudget` | Retained inbox bytes never exceed `sessions.inbox_buffer_bytes`, and a later human post drops the oldest | `TestInboxByteBudgetDropsOldest`, `TestRedeliverRespectsInboxByteBudget`, `FuzzHubInboxBudget` |
| `KeysMatchRetained` | A remembered `StepKey` belongs to a retained event, so eviction frees the key | `TestEvictedStepKeyCanBeAppendedAgain` |
| `NoDuplicateRetainedKey` | Two retained events never share a `StepKey` | `TestAppendDropsDuplicateStepKey` |

The positive cfg uses eight appends over sizes `{1, 2, 3}`, human posts over
`{1, 2}`, a two-event thread cap, a four-byte thread budget, a two-entry inbox
cap, a three-byte inbox budget, and one dedup key: 48404 states generated, 5859
distinct, about two seconds including JVM startup.
`HubRetention_evict_inbox_buggy.cfg` sets `ExemptInbox = FALSE`, so trimming the
thread also consumes the matching inbox entry; TLC must report
`InboxNeverEvictedByRuntime` (498 states generated, 104 distinct).
`HubRetention_no_cap_buggy.cfg` sets `CapEvents = FALSE`, so an event of size
`MaxEvent+1` is retained; TLC must report `EventCapEnforced` (66 states
generated, 31 distinct). `HubRetention_inbox_overflow_buggy.cfg` sets
`CapInbox = FALSE`, so two human posts exceed a one-entry inbox; TLC must
report `InboxWithinCap` (224 states generated, 80 distinct).
`HubRetention_inbox_no_byte_budget_buggy.cfg` sets `CapInboxBytes = FALSE`, so
two human messages of two bytes exceed a two-byte inbox budget; TLC must report
`InboxWithinBudget` (239 states generated, 81 distinct).
`HubRetention_key_leak_buggy.cfg` sets `EvictKeys = FALSE`, so a key outlives
its evicted event; TLC must report `KeysMatchRetained` (41 states generated, 22
distinct).

The model abstracts these details:

- Event sizes are abstract bytes from `Sizes`, not JSON or Go string lengths.
- The inbox is a sequence of sequence-number and size records, not a second
  slice copy. A human post offers a size from `HumanSizes`;
  `Manager.PostMessage` caps the real text at 32 KiB, and the hub never
  truncates a human message. The positive cfg keeps `HumanSizes` within
  `MaxEvent` so `EventCapEnforced` stays meaningful for the thread copy.
  `Consume` removes the oldest inbox entry when the runtime reads it. The
  real hub has no consume path: it keeps the inbox copy until a later human
  post exceeds the inbox byte or count cap and drops the oldest inbox entry.
  Evicting an event from the thread never removes an unconsumed inbox
  message.
- `MaxInbox` is a constant separate from `MaxEvents`, so a cfg can shrink one
  cap. The Go hub uses `sessions.event_buffer` for both.
- `InboxBudget` is separate from `Budget`, because the two Go budgets are
  configured separately. `TrimInboxBytes` drops entries from the head, which is
  the behavior of `stream.trimInboxLocked`.
- `Keys` is a finite set of abstract step keys, and no event holds more than
  one. A real `StepKey` is a string such as `11/1/assistant`.
- `DedupRetry` spends one append step from `MaxAppends`, while the Go reject
  branch costs nothing. The step only keeps TLC's state space finite.
- The journal write path is outside the model. `RestoreAppend` models the
  capped event that reaches `hub.Restore`, not the journal entry itself.
- Subscribers and live fan-out are outside the model; it checks retention, not
  channel delivery.

## Body-read deadline model

`spec/tla/BodyReadDeadline.tla` models a client that sends request headers and
then stalls the body, and a handler that reads the body. With `ApplyDeadline`
the server sets the read deadline in `readBody`; when the stall reaches the
deadline, the handler returns. Without it the handler can wait forever.

| TLA+ action | Go code it models |
|---|---|
| `HeadersSent` | the server has read the headers and dispatched the handler; `cmd/mayflyd/main.go` bounds this phase with `ReadHeaderTimeout` |
| `BodyArrives` | a body read completes before the deadline |
| `Stall` | no body byte arrives while the handler waits in `io.ReadAll` inside `httpapi.readBody` (`internal/httpapi/body.go`) |
| `DeadlineAborts` | `http.NewResponseController(w).SetReadDeadline` fires and the handler writes `408 request_timeout` |
| `Stutter` | no request in flight |

| Property | Meaning | Tests that sample it |
|---|---|---|
| `TypeOK` | The phase and clock stay in their domains | none; model well-formedness |
| `NoUnboundedBodyRead` | A handler blocked on a request body never waits past `server.body_read_timeout` | `TestStalledWebhookBodyTimesOut`, `TestStalledAPIBodyTimesOut` |

`BodyReadDeadline.cfg` uses a two-tick deadline and a five-tick bound: 15 states
generated, seven distinct, under a second.
`BodyReadDeadline_no_deadline_buggy.cfg` sets `ApplyDeadline = FALSE`, so the
stall runs past the deadline; TLC must report `NoUnboundedBodyRead` (16 states
generated, eight distinct).

The model abstracts these details:

- Time is an action, not a clock. `Stall` advances one abstract tick, and the
  negative cfg bounds the run with `MaxTicks`.
- A body that arrives is one action with no partial-progress state. Artifact
  uploads make progress and keep their own behavior; they are outside the
  model.
- The model covers the read that a JSON, webhook, or internal handler performs.
  WebSocket attach, event and inbox streams, and the LLM proxy set no body
  deadline and are outside the model.
- The model does not model connection close, keep-alive, or the 408 body. The
  unit test accepts a 408 or a closed connection, as the issue allows.

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

## Auth hardening model

`spec/tla/AuthHardening.tla` models the public authentication boundary in
`internal/httpapi` for #46 and #47. It covers a refused credential, a valid
credential, an authenticated mutating request, and a token loaded with a short
secret. The model is small: two client addresses, a burst of two failures, and
a map cap of one.

| TLA+ action | Go code it models |
|---|---|
| `FailedAuth` | `authorizedRoute` when `authenticateRequest` fails, `refuseAuth`, `authFailureLimiter.fail`, and the failed branch of `createWebSession` (`internal/httpapi/auth.go`, `websession.go`, `authlimiter.go`). The action writes an audit line when `AuditFailedAuth` is on and counts `mayfly_auth_failures_total{via}` |
| `Success` | a request whose bearer token or session cookie resolves. `authorizedRoute` authenticates before it consults the limiter, so a valid credential always proceeds |
| `Mutating` | a `POST`, `PUT`, `PATCH`, or `DELETE` after authentication, and the `s.writeAudit` call in `authorizedRoute`. The audit logger is independent of `MAYFLY_LOG_LEVEL` |
| `LoadShortToken` | `Config.resolveSecrets` and `validateTokenSecret` in `internal/config`. The action is disabled when `RejectShortTokens` is on |
| `Stutter` | no Go function; no further request at the modeled bound |

| Property | Meaning | Tests that sample it |
|---|---|---|
| `TypeOK` | Every variable stays in its declared domain | none; model well-formedness |
| `AuditAlwaysEmitted` | A mutating request is audited even when the process level is `warn` or `error` | `TestAuditLineIndependentOfProcessLogLevel`, existing audit tests |
| `FailedAuthAudited` | A refused authentication writes an audit line with `token_name` `unknown` | `TestFailedBearerAuthAuditsAndCounts`, `TestWebLoginAudit`, `TestFailedWebSignInLimitedAndCounted` |
| `SuccessfulAuthNeverLimited` | A valid credential is never refused by the failure limiter | `TestSuccessfulAuthNeverLimited` |
| `LimiterBounded` | The per-client map never exceeds its key cap | `TestAuthFailureLimiterMapBounded` |
| `ShortTokenRejected` | A resolved secret shorter than 32 characters is refused at load | `TestLoadRejectsShortTokenSecret`, `TestLoadRejectsShortAdminTokenSecret`, `TestLoadAcceptsMinimumLengthTokenSecret`, `FuzzTokenSecretValidation` |
| `FailedAuthBounded` | Without a success, one address cannot spend more than one burst of failed attempts | `TestFailedAuthLimiterBlocksAfterBurst`, `TestFailedAuthLimiterSeparatesIPs` |

`AuthHardening.cfg` sets `IPs = {"a", "b"}`, `Burst = 2`, `MaxOps = 4`, and
`MaxKeys = 1`, and checks every invariant. It holds 732 generated states, 204
distinct, and finishes in under one second.

The negative cfgs each remove one guarantee:

| Cfg | Constant | Expected violation |
|---|---|---|
| `AuthHardening_level_drops_audit_buggy.cfg` | `AuditIndependentOfLevel = FALSE` | `AuditAlwaysEmitted` |
| `AuthHardening_no_fail_audit_buggy.cfg` | `AuditFailedAuth = FALSE` | `FailedAuthAudited` |
| `AuthHardening_no_limiter_buggy.cfg` | `LimitFailedAuth = FALSE` | `FailedAuthBounded` |
| `AuthHardening_short_token_buggy.cfg` | `RejectShortTokens = FALSE` | `ShortTokenRejected` |
| `AuthHardening_limit_success_buggy.cfg` | `LimitSuccessBeforeAuth = TRUE` | `SuccessfulAuthNeverLimited` |

Each cfg must print the named invariant, not merely fail to parse. The runs
stop quickly: level-drops-audit 29 generated states (25 distinct),
no-fail-audit 6 (6), no-limiter 134 (60), short-token 18 (18), and
limit-success 138 (57). The last cfg models the bug of checking the failure
bucket before authentication: a client over budget is refused even with a
valid credential.

The model abstracts these details:

- Time is an action, not a duration. `Burst` is the whole modeled failure
  window; refill over a minute, the 10-minute idle TTL, and wall-clock expiry
  are not modeled. `Track` keeps the map cap but ignores recency order.
- Two addresses and a cap of one key exercise eviction. Real limits are 4096
  keys and one bucket per source.
- No HTTP stack, headers, cookies, or token bytes. Credential validity is a
  nondeterministic action. `via` and the response text are outside the model.
- An audit record is one boolean per request, not a JSON line. Metrics are
  folded into the failure action; the model does not check Prometheus output.
- A success does not reset the failure budget. The real limiter does not reset
  on success either, so this is faithful; the model simply cannot show refill.
- The 429 response is modeled as a refused attempt. The `Retry-After` value is
  outside the model.

## Cron time-zone model

`spec/tla/CronTimezone.tla` models a schedule whose cron fields are read in one
civil time zone, across a daylight-saving transition. It covers the Vixie cron
rules that `nextWallClockSlot` implements: a selected civil time in the
spring-forward gap fires once at the transition instant, and a selected civil
time in the fall-back overlap fires once at its first occurrence. A slot key is
a UTC instant.

| TLA+ action | Go code it models |
|---|---|
| `FireDay` | `zonedSchedule.Next` and `nextWallClockSlot` (`internal/workflow/scheduler.go`) when the selected civil time exists. `civilInstants` returns one instant normally and two in the overlap; the first instant after the input becomes the slot |
| `GapDay` | the empty result from `civilInstants` and `gapInstant`, which binary-searches the transition instant. With `FireGapAtTransition = TRUE` the transition key becomes the slot; `FALSE` models skipping the day |
| `Stutter` | no Go function; no further day at the modeled bound |

`DoubleFallback` is the fault switch for the overlap. `TRUE` emits both UTC
keys for the repeated civil time and models a scheduler that fires twice, which
the Vixie rule forbids.

| Property | Meaning | Tests that sample it |
|---|---|---|
| `TypeOK` | The day index and slot set stay in their domains | none; model well-formedness |
| `ValidSlotKeys` | Every slot is a UTC instant the calendar or an enabled fault explains | `TestChicagoWallClockAcrossDST`, `TestNewYorkSpringForwardFiresAtTransition`, `TestLordHoweHalfHourShift` |
| `GapFiresAtTransition` | A selected civil time in the spring-forward gap fires once at the transition instant, not skipped and not at a wall time that does not exist | `TestNewYorkSpringForwardFiresAtTransition`, `TestMostRecentSlotUsesGapTransition`, `TestLordHoweHalfHourShift`, `FuzzCronTZNext` |
| `FallbackFiresOnce` | A selected civil time in the fall-back overlap fires once, at the first occurrence; the second UTC key never becomes a slot | `TestNewYorkFallBackFiresOnce`, `TestMostRecentSlotUsesFirstFallBack`, `TestLordHoweHalfHourShift`, `FuzzCronTZNext` |

`CronTimezone.cfg` models the spring transition and
`CronTimezone_fall.cfg` the fall transition; both check every invariant. Each
holds 8 generated states (4 distinct) and finishes in under a second.
`CronTimezone_skips_gap_buggy.cfg` sets `FireGapAtTransition = FALSE` and must
report `GapFiresAtTransition` (4 generated, 3 distinct).
`CronTimezone_double_fallback_buggy.cfg` sets `DoubleFallback = TRUE` and must
report `FallbackFiresOnce` (4 generated, 3 distinct). Two fall-back firings are
the fault here, not the specified behavior.

The model abstracts these details:

- One scheduled civil minute per civil day. The cron grammar, the day-of-month
  and day-of-week fields, and the number of selected minutes per day are
  outside the model; one transition day is enough to check the rules.
- Offsets are minutes west of UTC. Days other than the transition day use
  offset 0, so the model checks key arithmetic and the skip and overlap rules,
  not the real zone database.
- A fixed-hour expression that selects several minutes inside the gap, such as
  `* 2`, fires once at the transition in the Go code. The model has one
  scheduled civil minute and cannot show an accumulated run per minute. Vixie
  cron may run each accumulated job at the transition; Mayfly starts one
  session per slot key, and a key is the transition instant, so the first fire
  is the defined behavior.
- The real-time path for an expression whose hour field selects every hour is
  outside the model. The Go code delegates that path to `robfig/cron`, and the
  unit tests cover the repeated and skipped hours.
- The cron timer, the catch-up loop, and the queue delivery key are outside the
  model. The slot is the UTC instant the model emits; `mostRecentSlot` and
  `acceptSlot` use the same instant as the key.

## Release gating model

`spec/tla/ReleaseGating.tla` models the release pipeline in
[.github/workflows/release.yml](../.github/workflows/release.yml) for #50: the
gate job, the GoReleaser draft release, the image digest, scan, signature, and
tag order, the chart, the final publish, and the `latest` marker that an RC
never takes. The gate decision itself is Go
(`internal/releasegate`), not TLA+. The model checks the ordering guarantees
that the workflow jobs must keep.

| TLA+ action | Go code it models |
|---|---|
| `GatePass` | the `gate` job in `release.yml` and `releasegate.Decide` (`internal/releasegate/releasegate.go`), which `hack/release-gate` runs over the `gh api` payloads |
| `GateFail` | the same job when `Decide` returns a reason |
| `BuildCLI` | the `goreleaser` job: archives, `checksums.txt`, and the GitHub release, which `release.draft: true` creates as a draft |
| `SignChecksums` | the `signs:` entry in `.goreleaser.yaml`: `cosign sign-blob --bundle` over `checksums.txt` |
| `BuildImage` | the `images` job build step, `docker/build-push-action` with `push-by-digest` |
| `ScanImage` | `trivy image` over the digest; a finding stops the job before the next action |
| `SignImage` | `cosign sign` over `IMAGE@DIGEST` |
| `TagImage` | `docker buildx imagetools create` over the signed digest |
| `Chart` | the `chart` job: `helm push`, `gh release upload`, and `cosign sign` as one unit |
| `Publish` | the `publish` job: `gh release edit <tag> --draft=false --latest=true\|false` |
| `Stutter` | no Go function; no further job at the model's bound |

| Property | Meaning | Tests that sample it |
|---|---|---|
| `TypeOK` | Every variable stays in its declared domain | none; model well-formedness |
| `NoPublishWithoutGate` | The GitHub release is never public without a successful gate | `TestDecide` (missing, failed, cancelled, and short stability runs), `FuzzDecide` |
| `PublicReleaseComplete` | A public release has signed checksums, a signed and tagged image, and a signed chart | `TestDecide` (the passing cases), `FuzzDecide`; the job graph in `release.yml` |
| `TagOnlySignedDigests` | An image tag exists only after the digest signature | the step order in the `images` job |
| `RCNeverLatest` | A release candidate never carries the `latest` marker | the `make_latest` template in `.goreleaser.yaml` and the `publish` job's explicit `--latest` |

`ReleaseGating.cfg` sets `RequireGate = TRUE`, `DraftUntilComplete = TRUE`,
`SignBeforeTag = TRUE`, and `LatestPrerelease = FALSE`, and checks all five
invariants: 134 states generated, 52 distinct, under one second. `prerelease`
ranges over both values in `Init`, so one run covers a final tag and a release
candidate. The negative cfgs each flip one switch:

| Cfg | Constant | Expected violation |
|---|---|---|
| `ReleaseGating_no_gate_buggy.cfg` | `RequireGate = FALSE` | `NoPublishWithoutGate` |
| `ReleaseGating_draft_false_buggy.cfg` | `DraftUntilComplete = FALSE` | `PublicReleaseComplete` |
| `ReleaseGating_tag_before_sign_buggy.cfg` | `SignBeforeTag = FALSE` | `TagOnlySignedDigests` |
| `ReleaseGating_rc_latest_buggy.cfg` | `LatestPrerelease = TRUE` | `RCNeverLatest` |

The runs stop quickly: no-gate 348 states (108 distinct), draft-false 47 (29),
tag-before-sign 111 (50), and RC-latest 132 (52).

The model abstracts these details:

- Each workflow job is one action. A job that fails is an action that never
  becomes enabled. Job retries, queue time, and a partially completed matrix
  are outside the model.
- The gate is one decision per tag. `releasegate.Decide` reads workflow-run and
  job payloads; the model does not represent their JSON shape, pagination, or
  the API's rate limits.
- `RequireGate` guards the actions that publish an artifact: the draft release,
  an image tag, the chart, and the public release. The workflow also needs the
  gate for every build job. An untagged image manifest is not a published
  artifact.
- The chart push and its signature are one action, because the job creates the
  OCI tag, signs the digest, and fails as a unit. A transient unsigned chart
  tag is outside the model.
- The model covers one tag. Two tags at the same time are two separate release
  runs.
- The tag's kind is nondeterministic in `Init`: `prerelease` is a BOOLEAN for a
  release candidate or a final release. `publish` sets `latest` from it, so one
  model run covers both. `LatestPrerelease` is the fault switch: TRUE models a
  publish that passes `--latest=true` for an RC. The `make_latest` template in
  `.goreleaser.yaml` is the production reason the switch is FALSE.

## Token budget model (#49)

`spec/tla/TokenBudget.tla` models the per-session LLM token budget from #49:
the admit check before the upstream request, the in-flight cap, the durable
reservation per call, the charge that lands in the counter and in the session
record, the control-plane crash that loses memory only, the recovery that seeds
the counter from the record plus its outstanding reservation, and the close
that follows exhaustion. The flags tie to the exhaustion and concurrency
decisions: a session at or over its budget refuses further calls and closes,
and a session with a budget allows one call in flight with a durable
reservation behind it.

`Observe` and `Charge` are separate steps. `Observe` is the client's read of
the reply finishing: for a stream that is `data: [DONE]`, which the agent's
client stops at. `Charge` is the broker reaching upstream EOF, which settles
the ledger and releases the slot. The reply is therefore complete on the wire
while the call still holds the session's only slot, and a caller that treats
`[DONE]` as a settled call is refused `llm_busy`. `AttemptBusy` is that
refusal, and `WaitForSettle` is the caller's policy: on, the fixed caller waits
for the settle; off, it waits only for the reply, which is the pre-fix
assumption of the e2e overlap test. `Retry` is the fixed caller's retry of a
refused call.

| TLA+ action | Go code it models |
|---|---|
| `Admit` | the budget hook: the per-session ledger admit under `llmMu` in `internal/session/budget.go`, called from `ServeLLM`'s `admit` hook (`internal/broker/broker.go`). The in-flight check, the `max_llm_tokens` and `max_llm_requests` checks, the request increment, the reservation, and the synchronous record write |
| `Upstream` | `broker.ServeLLM` after `client.Do` and the usage scan in `internal/broker/usage.go`. The charge is the reported usage or the byte estimate `req_bytes/4 + resp_bytes/4`, whose completion part stops at the call's `max_tokens` |
| `Charge` | the charge-after path of the ledger (`internal/session/budget.go`): usage into the counter and the record, reservation released, token metric updated |
| `Observe` | `readStream` in `internal/llm/client.go` returning at `data: [DONE]`, and the assistant message the runtime publishes from it. The slot, the reservation, and the ledger do not move |
| `AttemptBusy` | `admitLLM` (`internal/session/budget.go`) refusing with `llm_busy`, 429 plus `Retry-After`, while `inflight >= 1`. It changes no usage and does not settle the running call |
| `Retry` | the retry loop in `executeModel` (`internal/agent/runtime.go`) after `retryableModelError` accepts a 429 `llm_busy`, and the e2e test's `proxyBusyRetry`. One retry is one admission |
| `Refuse` | `broker.Refusal` written by `writeRefusal`: `llm_busy` is 429 with `Retry-After`, `llm_budget_exhausted` is 429 without it, and `llm_persist_failed` is 503. A refusal is not forwarded and changes no counter. The model represents the exhausted refusal as `Refuse`, the busy refusal as `AttemptBusy`, and `llm_persist_failed` as an `Admit` that does not happen |
| `Crash` | a control-plane process restart; no single Go function, because the process is gone. Memory is lost; the record keeps the charge and the reservation |
| `Recover` | `session.Manager.Start` listing records before `adopt`, and the seeding in `adoptPods` and `adoptPodless` (`internal/session/manager.go`, `lifecycle.go`): `spent = record.spent + record.pending` |
| `Exhaust` | the `EventError` and `Close(id, "llm_budget_exhausted")` in the charge path, with `sess.Error` set |
| none (metrics) | `Metrics.LLMRequest` and `Metrics.LLMTokens` (`internal/metrics/metrics.go`), recorded by `internalLLM` for every call, including a panic after admit. The model has no metric state |
| `Stutter` | no Go function; no further call at the modeled bound |

| Property | Meaning | Tests that sample it |
|---|---|---|
| `TypeOK` | Every variable stays in its declared domain | none; model well-formedness |
| `BudgetBound` | Billed tokens never exceed one call past the limit, and the request count never exceeds its cap | `FuzzTokenLedger`, `FuzzServeLLMBudget`, `TestLLMOvershootAtMostOneResponse`, `TestLLMRequestCapOvershootsByZero`, e2e `TestSessionBudgetExhaustion` |
| `NoChargeLost` | A charged call always settles: the in-memory ledger and the durable record never fall below the billed tokens, and a refusal never releases a running call's slot | `FuzzTokenLedger`, `FuzzServeLLMBudget`, `TestLLMAdmitCharge`, `TestLLMChargeKeepsMemoryWhenPersistFails`, `TestLLMAdmitRollsBackOnPersistPanic`, `TestLLMAdmitFailsClosedWhenReservationIsNotDurable`, `TestInternalLLMBusyDoesNotCharge`, `TestInternalLLMPanicChargesReservation`, e2e `TestSessionLLMBusyDoesNotCharge`, `TestSessionBudgetSurvivesRestart` |
| `NoAdmitWhenExhausted` | An exhausted session refuses every call, and a refusal changes no ledger state | `FuzzTokenLedger`, `FuzzServeLLMBudget`, `TestInternalLLMExhaustedBudget`, `TestInternalLLMPersistFailureRefusesAndLabels`, `TestLLMRefusedAdmitThenChargeChangesNothing`, `TestAdoptClosesExhaustedSession`, e2e `TestSessionBudgetExhaustion` |
| `ClosedAfterExhaust` | A charge that reaches a limit appends one error event, closes with reason `llm_budget_exhausted`, and ends `completed` | `TestLLMExhaustionClosesSession`, `TestLLMExhaustionSurvivesEmptyFinalize`, `TestInternalLLMExhaustedBudget`, `TestAdoptClosesExhaustedSession`, `TestAdoptClosesExhaustedPodlessSession`, e2e `TestSessionBudgetExhaustion` |
| `ObserveKeepsSlot` | An observed call is still in flight: the reply is complete on the wire, and the call still holds the slot and has not settled | `TestInternalLLMBusyUntilUpstreamEOF`, `TestInternalLLMBusyDoesNotCharge`, `FuzzServeLLMBudget` (the delayed-EOF op), e2e `TestSessionLLMBusyDoesNotCharge` |
| `NoBusyAfterSettled` | A caller that waits for the earlier call to settle after it reads the reply is never refused busy. The ghost counts a refusal taken while the held reply was already visible | `TestInternalLLMBusyUntilUpstreamEOF`, e2e `TestSessionLLMBusyDoesNotCharge` |
| `BusyRetryAdmitted` | A call refused busy is eventually admitted, or the session closes first. It holds under weak fairness on `Upstream`, `Observe`, `Charge`, and `Retry` | `TestTurnRetriesBusyModelError`, `TestInternalLLMBusyUntilUpstreamEOF`, `FuzzServeLLMBudget` (the delayed-EOF op), e2e `TestSessionLLMBusyDoesNotCharge` |

The e2e cases in `e2e/budget_test.go` run against the deployed release. They
cover exhaustion, two overlapping calls with `llm_busy`, the session fields and
the CLI tokens line, the token and estimate counters, usage across a
control-plane restart with bbolt, a per-agent override, and the defaults-off
case. The exhaustion case spends the request cap through the proxy and then
accepts either outcome for the next call: the `429 llm_budget_exhausted`
refusal while the session is still open, or a `401` once the charge has closed
the session and removed its scoped token. The usage must not change in either
case.

The overlap case encodes the `Observe`/`Charge` split. The agent's reply is
visible at `data: [DONE]`, so the test first probes the proxy until a call is
admitted: an admitted probe proves the earlier call released the slot, and the
usage read after it is the settled baseline. It then starts a slow call and
retries it while the answer is `llm_busy`, the same retry the runtime applies,
and it retries the overlapping probe until the control plane refuses it busy,
so the refusal belongs to the slow call and not to an agent call that settled
in between. The request count is asserted as a lower bound, because the
runtime's own retry and a redelivered prompt can add calls of their own.

The suite reaches the control plane through a `kubectl port-forward`, which can
drop a call that never left the tunnel; Go does not replay a POST with a body.
`tunnelRetryAfter` (`e2e/forward.go`) repeats a call only after a transport
error `forwardBroken` accepts while the session's admitted request count did not
move. A dropped call that moved the count was admitted, so its missing response
fails the test instead of repeating a billed call. `TestTunnelRetryAfter` and
`FuzzTunnelRetryDecision` cover the decision without a cluster.

The dead-letter case checks the objects a failed session start leaves behind. A
delete only sets `metadata.deletionTimestamp`: the
`kubernetes.io/pvc-protection` finalizer keeps a claim listed until its
controller clears it, and `kubectl get -o name` prints a terminating object like
any other. `TestDeadLetter` therefore lists the session objects with
`kubectl get ... -o jsonpath` as `kind/name<TAB>deletionTimestamp`, fails at
once when `unterminatedLeftovers` (`e2e/leftovers.go`) finds an object without a
timestamp (no delete was accepted for it, so it is a real leak), and then waits
up to 30 seconds for `waitNoSessionObjects` to see the terminating objects gone.
`TestParseLeftovers`, `TestUnterminatedLeftovers`, and `FuzzLeftoverParse` cover
the decision without a cluster; the fuzz target checks that a parsed listing
keeps one object per non-blank line, in order, and marks an object terminating
only when its timestamp is present. `CreateSession`'s rollback logs every delete
error that is not `NotFound` with the session ID and `kind/name`, and still runs
the remaining deletes; `TestCreateSessionRollbackLogsFailedDeletes` covers that
with a fake clientset.

`TokenBudget.cfg` sets `Budget = 3`, `MaxCall = 2`, two callers, no crash, an
in-flight cap of one, the check, charge, and reservation writes on, and
`WaitForSettle = TRUE` for the fixed caller. It checks every invariant and
proves `ClosedAfterExhaust` and `BusyRetryAdmitted` under weak fairness on
`Exhaust`, `Recover`, `Upstream`, `Observe`, `Charge`, and `Retry`: 310 states
generated, 101 distinct, under one second. `TokenBudget_crash.cfg` adds two
control-plane crashes: 2,284 generated, 703 distinct. The negative cfgs each
flip one switch:

| Cfg | Constant | Expected violation |
|---|---|---|
| `TokenBudget_no_inflight_cap_buggy.cfg` | `InflightCap = 2` | `BudgetBound` |
| `TokenBudget_check_after_buggy.cfg` | `CheckBefore = FALSE` | `BudgetBound` |
| `TokenBudget_volatile_counter_buggy.cfg` | `PersistCharge = FALSE` | `NoChargeLost` |
| `TokenBudget_no_pending_buggy.cfg` | `DurablePending = FALSE` | `NoChargeLost` |
| `TokenBudget_observe_implies_settled_buggy.cfg` | `WaitForSettle = FALSE` | `NoBusyAfterSettled` |

The runs stop quickly: no-in-flight-cap 431 states generated (133 distinct),
check-after 314 (119), volatile-counter 108 (64), and no-pending 14 (11). With
`InflightCap = 2` two callers are admitted at `Budget - 1` and both responses
charge `MaxCall`, so the provider's total passes the one-response allowance.
With `CheckBefore = FALSE` the counter is tested only after the charge, so a
call is admitted with the counter already at the budget. With
`PersistCharge = FALSE` the record never holds a settled charge and a restart
loses it. With `DurablePending = FALSE` the reservation never reaches the
record, so a charge has no durable coverage while it is in flight.
`TokenBudget_observe_implies_settled_buggy.cfg` turns `WaitForSettle` off: the
caller treats `data: [DONE]` as a settled call, so its next call arrives while
the observed call still holds the slot and is refused busy. TLC stops after 109
states (62 distinct) on `NoBusyAfterSettled`; the counterexample is `Admit`,
`Upstream`, `Observe`, then `AttemptBusy`. The positive cfgs reach the same
`AttemptBusy` step when the holder's reply is not visible, so the liveness
check is not vacuous: the refusal is legitimate there, and `Retry` admits the
caller once `Charge` frees the slot.
`deep/TokenBudget_three_callers.cfg` runs nightly with three callers, budget
16, and two crashes: 105,108 states generated, 30,961 distinct, about eight
seconds.

The check-after cfg omits `NoAdmitWhenExhausted`, because admitting past the
limit is that cfg's modeled bug; `BudgetBound` is the invariant that must bite.

The model abstracts these details:

- One call's charge is a nondeterministic value from 1 to `MaxCall`, and the
  reservation is `MaxCall`. The model assumes that the estimate is an upper
  bound. The Go reservation is `req_bytes/4` plus `max_tokens` (4096 by
  default), which is a heuristic; the estimated completion stops at
  `max_tokens`, so an estimated charge stays inside the reservation, while a
  reported usage is charged as reported.
- Tokens only. `max_llm_requests` is a second counter with no reservation and
  no overshoot; it is outside the model.
- `Observe` and `Charge` are single steps. Provider latency, SSE chunk
  boundaries, and the gap between the last reply byte and EOF are outside the
  model, as are the retry backoff (1s doubling to 5s) and its 5-minute budget:
  `Retry` is one admission. A JSON reply reaches the client and EOF together;
  the model covers the streamed case where `[DONE]` arrives first.
- The caller policy is one switch. A real session can have several callers,
  and the runtime retries a busy refusal while the e2e test waits for the
  settle; `WaitForSettle` selects one policy for every caller in a cfg.
- One session. The resolved per-session limit is the constant `Budget`;
  per-agent overrides resolve before the ledger sees them.
- `Exhaust` is the close decision. Session finalize, final status, and artifact
  export after the close are modeled by `SessionResume.tla`.
- bbolt writes are atomic, as in the other models.

## Fuzz targets for the bounds

- `FuzzTokenLedger` (`internal/session`) drives the real ledger with random
  admit, charge, crash and recover, refusal, save-failure, and concurrent-admit
  sequences. It asserts `BudgetBound`, `NoChargeLost`, and
  `NoAdmitWhenExhausted` against a ghost total of every charge the provider
  billed, so it is the Action-to-Go bridge for the
  [token budget model](#token-budget-model). Each charge must also equal the
  amount the harness expects, which fails a size estimate that bills past the
  call's `max_tokens` cap. A 90-second run made 499,297 executions.
- `FuzzServeLLMBudget` (`internal/httpapi`) drives the internal LLM route with
  random budgets and responses: reported usage, a 2xx without usage, a 2xx
  whose body is far past the completion cap, an upstream 5xx, a forwarded 4xx,
  a transport failure, two overlapping calls, a panic after admit, a persist
  failure, a probe at the limit, an overlap while the provider holds the stream
  open after `data: [DONE]`, and an unbudgeted session. It asserts that a
  refusal changes no usage and settles no running call, that the open call
  keeps its slot and charges nothing until EOF, that the retry after EOF is
  admitted, that no in-flight slot leaks, and `BudgetBound`. A 90-second run
  made 39,053 executions.
- `FuzzHubAppendBudget` (`internal/hub`) drives appends, restores, and replays,
  and asserts the byte budget, the count cap, the contiguous suffix, and that
  the inbox keeps its human message.
- `FuzzHubInboxBudget` (`internal/hub`) drives human posts, redeliveries, and
  runtime events, and asserts the inbox byte budget, the count cap, the byte
  accounting, and that a runtime append or restore never changes the inbox.
  The seeds include a message larger than the whole budget and a post count
  past the count cap.
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
- `FuzzParseUntrusted` asserts the delivery-key rule across every trigger kind:
  a key from `X-GitHub-Delivery`, Slack `trigger_id`, or `Idempotency-Key` is
  accepted only when it is valid UTF-8, and an accepted key is used verbatim,
  so two different raw keys can never share one dedupe identity. The seed
  `testdata/fuzz/FuzzParseUntrusted/085a2b2b0c4ae5ad` carries a lone `0xB0`
  byte in `X-GitHub-Delivery` and is the regression from the scheduled verify
  run that sanitized the key to U+FFFD.
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
- `FuzzCronTZNext` (`internal/workflow`) drives `Next()` for an expression, a
  zone, and a start instant. It asserts strictly increasing UTC slots on whole
  minutes, stable results, and the Vixie rules: a fixed-hour expression fires
  only at a selected civil time or at a zone transition, never twice at the
  same civil minute, and, when it selects every day, at most 26 hours apart, so
  the spring-forward day is not skipped; an all-hours expression matches its
  minute field and may fire twice in a repeated hour. Seeds cover America/Chicago,
  America/New_York, Australia/Lord_Howe, Europe/Berlin, UTC, the 2026
  transitions, and daily, gap, overlap, every-minute, and hourly expressions.
  A 60-second run made 289,281 executions.
- `FuzzTokenSecretValidation` (`internal/config`) feeds arbitrary name, scope,
  and secret bytes to the token rules. It asserts that a secret shorter than
  `MinTokenSecretLength` is rejected with the fixed error text, that a longer
  secret is accepted, and that the error never varies with the secret. Seeds
  sit at zero, 31, and 32 characters, and on a long value. A 60-second run made
  5,038,767 executions.
- `FuzzToolResultContent` (`internal/mcp`) feeds arbitrary `tools/call`
  content arrays to the MCP parser with small image limits. It asserts no
  panic, that every accepted image has an allowed MIME type and stays within
  the per-image and per-result limits, and that the text rendering used by
  events, traces, and text-only callers never contains an accepted image's
  base64 or a data URI. Seeds cover an empty array, text only, an allowed
  image, a mixed text and image result, a disallowed MIME type, invalid
  base64, an oversize image, audio, a resource block, and malformed JSON.
- `FuzzDecodeSSE` (`internal/mcp`) feeds arbitrary bytes to the Streamable
  HTTP `text/event-stream` decoder. It asserts no panic, that a returned
  message matches the requested id, is valid JSON, and is at most
  `maxResponseBytes` (8 MiB, the bound shared with the HTTP JSON body and the
  stdio line), and that content in a returned `tools/call` result keeps the
  parser's image limits and byte-free text rendering. Seeds cover an empty
  stream, a plain frame, a legal image, an oversize image, an unrelated id
  before the matching one, a message split across data lines, malformed JSON,
  an unterminated frame, and a comment line. What is not verified: SSE fields
  the client ignores (`event`, `id`, `retry`), the server's conformance, and
  the gateway, journal, and provider hops after the decoded message. A
  60-second run made 3,390,248 executions.
- `FuzzAuthLimiterKey` (`internal/httpapi`) feeds arbitrary `RemoteAddr` and
  `X-Forwarded-For` bytes with both trust settings. It asserts that the limiter
  key is stable and bounded, that trust off keys on the peer address, and that
  trust on takes the first forwarded hop. A seed from the corpus carries an
  overlong IPv6 host; the fuzz run found the missing length cap before commit,
  and the seed stays as the regression. A 60-second run made 526,765
  executions.
- `FuzzDecide` (`internal/releasegate`) feeds arbitrary workflow-run and job
  payloads to `Decide`. A PASS must satisfy an independent, more permissive
  search: a green `ci` run and a successful `e2e-stability` run with at least
  20 e2e jobs and a green stability gate job. The independent search matches a
  stability run by `head_sha` or an exact `sha=<sha>` title token, never by a
  substring. Seeds cover a passing history, a stability run with five runs, an
  empty jobs list, invalid JSON, a dispatch on another commit whose title names
  the target, and titles with a short, longer, unbounded, or prefix-sharing
  SHA. A 60-second run made 6,493,973 executions.
- `FuzzSSEUsage` (`internal/broker`) feeds SSE bytes split at fuzzer-chosen
  boundaries to the usage scan. It asserts that the scan matches the
  single-chunk result, that a line over the line cap is skipped without growing
  memory, that the forwarded bytes are unchanged, and that nothing panics.
- `FuzzUsageJSON` (`internal/broker`) decodes arbitrary usage payloads. Tokens
  never go negative, a huge or float value saturates instead of wrapping, and a
  non-object is ignored.
- `FuzzInjectStreamOptions` (`internal/broker`) rewrites streamed request
  bodies. The output is valid JSON, `model` and every other key survive,
  `include_usage` is true, existing `stream_options` keys are kept, and a
  non-streamed body is unchanged.

## What is abstracted away

- The e2e tunnel retry treats a dropped call as one error from the set
  `forwardBroken` accepts: `io.EOF`, `ECONNRESET`, `ECONNREFUSED`, and
  `net.ErrClosed`. A real port-forward can drop a call at any byte, and the
  session's admitted request count is the only signal that the control plane
  saw the call. Capture of `kubectl port-forward` stderr under
  `E2E_ARTIFACTS` is not modeled.
- The e2e dead-letter cleanup check treats a leftover object as removed once it
  carries a `deletionTimestamp` and `waitNoSessionObjects` no longer lists it
  within 30 seconds. The finalizer controller's own timing is not modeled, and
  the listing spread over five `kubectl get` calls is not atomic; the wait that
  follows covers an object created between the calls.
- The token budget model abstracts one call's charge as a value from 1 to
  `MaxCall` and assumes the reservation bounds it. The Go code keeps an
  estimated charge inside that bound: the estimate's completion part stops at
  the call's `max_tokens`. Byte counting, streaming, retries, the separate
  request counter, and the finalize after the close are not modeled.
- The release gating model abstracts each workflow job as an action. Job
  retries, runner availability, and the GitHub API's pagination and rate
  limits are not modeled. The model has one tag at a time.
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

The token budget model is checked against the design. `FuzzTokenLedger` and
`FuzzServeLLMBudget` sample random action sequences against the Go ledger and
route, and the Go tests cover the named paths; neither proves every
interleaving of the Go code. The e2e budget cases run against `llmstub`, a fake
provider, not a real one. The stub reports usage unless the prompt carries its
`no-usage` directive, which the metrics case uses to stay on the size estimate.
The e2e suite is not run on the development machine for this change (Docker is
unavailable); CI runs it against the release.

The e2e tunnel retry is checked against synthetic transport errors and request
counts. A live `kubectl port-forward` drop is not part of these checks, and the
`E2E_ARTIFACTS` stderr capture is checked only by `TestForwardLogFile`, which
writes to a temporary directory.

The e2e dead-letter cleanup check is not run on the development machine (Docker
and k3d are unavailable); CI runs it against the release. The terminating-object
path needs a real claim held by the `pvc-protection` finalizer, so the local
checks cover the parser, the decision, and the rollback logging only.

The release gate reads synthetic GitHub API payloads. Its Go tests and
`FuzzDecide` cover run and job selection, but the live API's field names and
pagination are not verified. A change on the GitHub side fails the gate closed:
an unreadable history is a failed gate. A real `v*` tag and a real release run
are outside every check in this repository.

The workflow policy check reads files; it does not run GitHub Actions. It
cannot prove that a job is triggered, that a QEMU build succeeds, that a
scanner's database download works, or that GHCR serves a tag. The ci
`image-scan`, the nightly, the stability preflight, and the release scan are
the proof. The check also cannot see a CVE published, or a fix released, after
the last preflight run; the release scan catches that finding and leaves the
release a draft.

The k3d image fixtures check the matcher, not the cluster. They do not run
`k3d image import`, do not read a live `crictl` inventory, and do not check node
discovery from Docker labels. The retry backoff and the per-node report run only
in `hack/e2e.sh` and `hack/k3d-up.sh`. The 20-run stability gate is the only
check of the live import path.

The token budget model assumes the estimate bounds the charge. The Go estimate
is a heuristic: `bytes/4` under-counts images and CJK text, so a provider can
charge more for a call than the estimate. The ledger never bills an estimated
call past its reservation, because the completion part stops at the call's
`max_tokens`; the real cost can still exceed what the counter shows. A provider
that reports usage is charged what it reports, and a reported prompt count can
exceed `req_bytes/4`, so a reported charge can pass the reservation and the
one-response overshoot remains possible. A provider
that omits usage is charged the estimate, and whether each provider honors
`stream_options.include_usage` is unverified: the broker injects it and accepts
a missing object, but no live provider call is part of these checks. A
third-party harness may reject the injected usage-only SSE chunk with an empty
`choices` array; the built-in runtime ignores it. Cost in dollars, the
gateway and MCP spend, and cross-session or per-project aggregate budgets are
outside the model and the implementation.

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

## Image import verification

`hack/lib/k3d-images.sh` holds the import and verification that `hack/e2e.sh`
and `hack/k3d-up.sh` share. `k3d_image_present <repo:tag> <crictl-json>` is a
pure jq filter over `crictl images -o json`. It removes one leading
`docker.io/` and one leading `library/` from the requested name and from every
`repoTags` entry, then compares `repo:tag`. Digests are ignored. The helper
returns 0 for present, 1 for absent, and 2 when the JSON cannot be read, so a
broken inventory is not reported as a missing image.

`hack/test-k3d-images.sh` feeds fixture JSON to the matcher and needs bash and
jq only. The fixtures cover a short name, `docker.io/library/` normalization in
both directions, a namespaced registry, a registry with a port, a tag that does
not match, an empty and a null image list, a digest-only entry, another
namespace, a node pair where one node has the image and the other does not, the
requested image as the second entry, and invalid JSON. The script exits nonzero
if any check fails.

There is no TLA+ model for this check. The import path has no concurrent
actors, message protocol, or shared state machine to explore. It is a bounded
loop over deterministic predicates: run the import, read each node's inventory,
and retry the names that are missing, at most three times. TLC would restate
that loop. The part that can be wrong and is not visible by inspection is name
matching across containerd's normalization and the per-node inventory, and the
fixture set enumerates those cases exactly. A model would not add an invariant
that the fixtures cannot check.

Run it with:

```bash
make test-k3d-images
```

The `shell` job in [ci.yml](../.github/workflows/ci.yml) runs the fixtures and
`shellcheck` on the helper and both callers on every push and pull request.

## Workflow and image scan policy

`hack/image-scan.sh` is the one image scan. The `image-scan` job in ci, the
nightly `image-scan` workflow, the stability gate's `preflight-scan` job, and
the release `images` job all call it, and no workflow invokes trivy directly.
The script declares the whole policy once:
`--scanners vuln --severity HIGH,CRITICAL --ignore-unfixed --exit-code 1`, for
`linux/amd64` and `linux/arm64`. Trivy's version is in
`.github/trivy-version`.

`test/imagepolicy` is a test-only package. It parses every file under
`.github/workflows` into jobs and steps, reads `hack/image-scan.sh`, and checks:

- no workflow run step invokes trivy, and a workflow that calls the script
  installs trivy;
- every `aquasecurity/setup-trivy` step passes
  `${{ steps.trivy-version.outputs.value }}`, and its job reads
  `.github/trivy-version` into that step;
- the script declares each policy flag once with the required value, declares
  both platforms, and sets `set -euo pipefail`;
- the release `images` job builds, scans, signs, and tags in that order, and
  the scan step does not set `continue-on-error`;
- `ci.yml` has a `changes` job that uses `dorny/paths-filter` and an
  `image-scan` job that needs it and builds both images;
- `e2e-stability.yml` has `preflight-scan`, checks out the gated SHA, and the
  `gate` job needs it;
- `image-scan.yml` runs on `schedule` and `workflow_dispatch`, builds both
  images, and scans both images of the latest published release;
- every third-party `uses:` in every workflow is pinned to a 40-hex commit.

`TestWorkflowPolicyRejectsBrokenCopies` is the negative check. It loads the
repository, applies one mutation in memory, and requires the check to report
it. The mutations lower the severity to `CRITICAL` only, remove
`--ignore-unfixed`, drop a platform, add an inline `trivy image` step, drift
setup-trivy's version, pin an action by tag, sign before the scan, make the
release scan non-blocking, remove `preflight-scan` from the gate's `needs`,
scan `main` instead of the gated SHA, delete the nightly workflow, and remove
its schedule. Every mutation is rejected.

`hack/test-image-scan.sh` runs the script with a fake trivy on `PATH`, without
a network. It proves both platforms always run with the policy flags, a
finding on one platform fails the whole scan after both run, a trivy error
fails the scan, `--dir` scans both archives, a missing archive fails, the
step summary lists both platforms, and a missing trivy fails.

There is no fuzz target. The script does no text parsing: it builds an
argument vector, reads an exit status, and writes a summary. The version file
is read by a `cat` step in the workflow, and the policy check compares the
value. There is no parser to fuzz.
