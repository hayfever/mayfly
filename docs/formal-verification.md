# Formal verification

Two checks cover behavior that unit tests can only sample: a TLA+ model of the
session lifecycle and resume protocol, and native Go fuzz targets. Each has a
script and a CI job.

| Check | Script | CI job |
|---|---|---|
| TLC on the session model | `hack/tlc.sh` | `tlc` in [verify.yml](../.github/workflows/verify.yml), on pull requests |
| TLC with two sessions | `hack/tlc.sh --deep` | `tlc-deep` in verify.yml, nightly and on demand |
| Every fuzz target | `hack/fuzz.sh` | `fuzz-nightly` in verify.yml |

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

The model is checked against the design, not against the Go code. Nothing in
the repository shows that a Go execution is a behavior of the model, and the
reverse direction (a model behavior that the Go code permits) is also
unchecked. Trace conformance, for example by recording a Go test's journal and
ledger operations and replaying them through the model, is future work.

A fuzz target checks the input handling of the package that adds it; the
targets do not check the lifecycle. `hack/fuzz.sh` discovers targets
automatically; a branch with no fuzz target exits 0 and says so.
