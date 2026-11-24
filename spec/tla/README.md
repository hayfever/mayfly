# TLA+ lifecycle, retention, and queue models

`SessionResume.tla` models the Mayfly session lifecycle and resume protocol as
implemented. `HubRetention.tla` models the in-memory hub: the per-event cap,
the retained thread byte budget, the thread and inbox byte and count caps, the
inbox exemption, the dedup-key table, and the restore path.
`BodyReadDeadline.tla` models a handler that reads a request body after a
stalled set of headers, with and without the server's read deadline.
`QueuePoison.tla` models a queue with a poison job: claim, handler panic, Nack,
lease expiry, restart, and burial at `max_attempts`. `AuthHardening.tla` models
API authentication: always-on audit lines, failed-auth auditing and counting,
the per-client failure limiter, and the 32-character token-secret floor.
`CronTimezone.tla` models a schedule in one civil time zone across a
daylight-saving transition: the spring-forward fire at the transition instant,
the single fall-back fire at the first occurrence, and slot keys as UTC
instants. `ReleaseGating.tla` models the release pipeline: the gate, the draft
GitHub release, the image scan, signature, and tag order, the chart, the final
publish, and the rule that an RC never takes the `latest` marker.
`TokenBudget.tla` models a per-session LLM token budget: the admit check, the
in-flight cap, the durable reservation, the charge, the exhaustion close, and
the recovery that seeds the counter from the session record. A cfg runs
the module named by its basename up to the first underscore, so
`HubRetention_evict_inbox_buggy.cfg` runs `HubRetention.tla`.
[formal-verification.md](../../docs/formal-verification.md) maps each action to
the Go code it models, lists the invariants, and states what the models
abstract away.

## Run it

```bash
hack/tlc.sh
```

The script downloads the pinned `tla2tools.jar` into
`${XDG_CACHE_HOME:-$HOME/.cache}/mayfly/tla` (override with `TLA_CACHE_DIR`),
checks its SHA-256, and runs TLC on every `.cfg` directly in this directory.
`hack/tlc.sh --deep` runs the larger-bound cfgs in `deep/` instead, and
`hack/tlc.sh <cfg>...` runs the named ones. `TLC_JAVA_OPTS` passes extra JVM
options, such as `-Xmx8g` for a deep run. It needs
`java` on `PATH`, `JAVA`, or `JAVA_HOME`. TLC state directories go under a
temporary directory, never into the repository.

## Config convention

- `SessionResume.cfg`, `HubRetention.cfg`, `BodyReadDeadline.cfg`,
  `QueuePoison.cfg`, `AuthHardening.cfg`, `CronTimezone.cfg`,
  `CronTimezone_fall.cfg`, `ReleaseGating.cfg`, `TokenBudget.cfg`, and
  `TokenBudget_crash.cfg` are the positive
  configurations.
  Each must pass with no invariant violation; `SessionResume.cfg`,
  `QueuePoison.cfg`, `TokenBudget.cfg`, and `TokenBudget_crash.cfg` must
  also prove their liveness properties.
- A configuration whose file name ends in `_buggy.cfg` is a negative check. It
  must produce an invariant violation, not a pass and not a parse error. The
  invariant name is in a comment line:

  ```
  \* expect-violation: NonIdempotentAtMostOnce
  ```

  If the line is absent the script expects `NonIdempotentAtMostOnce`.

`hack/tlc.sh` exits non-zero on any unexpected outcome, including a positive
cfg that fails or a negative cfg that reports the wrong invariant.

## Bounds

`SessionResume.cfg` uses one session, an initial prompt and one later message,
one control-plane restart, and a sequence block of two. It has no pod kill or
replacement, so the liveness check stays fast; `SessionResume_resume.cfg`
covers that path with safety only. The bounds are small on purpose: TLC runs on
every pull request that touches the session, journal, queue, gateway, or
runner packages. One session covers every property because sessions share no
mutable state; a second session multiplies the liveness state space without
adding a behavior. Keep a new cfg inside the same envelope unless it proves
something the small one cannot; put a cfg that needs more time in `deep/`,
which runs only in the nightly `tlc-deep` job.

## Configurations

| File | Purpose |
|---|---|
| `SessionResume.cfg` | All invariants and liveness, positive |
| `SessionResume_resume.cfg` | Safety only: one pod kill and one replacement reach the tool ledger and replay path |
| `SessionResume_buggy.cfg` | `LedgerBeforeDispatch = FALSE`: a lost pending record re-runs a non-idempotent tool |
| `SessionResume_interrupt_buggy.cfg` | `InterruptOnResume = FALSE`: a pending non-idempotent call is dispatched again |
| `SessionResume_volatile_inbox_buggy.cfg` | `DurableInbox = FALSE`: an acknowledged message dies with the control-plane hub |
| `SessionResume_redeliver_dup_buggy.cfg` | `RedeliverNewKey = TRUE`: adoption starts one accepted message under two turn keys |
| `HubRetention.cfg` | Hub thread and inbox byte budgets, event cap, contiguous suffix, inbox exemption and count cap, dedup keys, positive |
| `HubRetention_evict_inbox_buggy.cfg` | `ExemptInbox = FALSE`: a runtime flood drops an unconsumed human message |
| `HubRetention_no_cap_buggy.cfg` | `CapEvents = FALSE`: a retained event exceeds the per-event cap |
| `HubRetention_inbox_overflow_buggy.cfg` | `CapInbox = FALSE`: the inbox grows past its count cap |
| `HubRetention_inbox_no_byte_budget_buggy.cfg` | `CapInboxBytes = FALSE`: the inbox grows past its byte budget |
| `HubRetention_key_leak_buggy.cfg` | `EvictKeys = FALSE`: a dedup key outlives its evicted event |
| `BodyReadDeadline.cfg` | Body read deadline, positive |
| `BodyReadDeadline_no_deadline_buggy.cfg` | `ApplyDeadline = FALSE`: a stalled body read never ends |
| `QueuePoison.cfg` | Claim, recovered panic, Nack, burial, and healthy progress, positive |
| `QueuePoison_no_recover_buggy.cfg` | `RecoverPanics = FALSE`: a handler panic crashes the worker |
| `QueuePoison_no_claim_count_buggy.cfg` | `RecoverPanics = FALSE` and `CountAttemptOnClaim = FALSE`: a crash loop never reaches `max_attempts` |
| `AuthHardening.cfg` | Always-on audit, failed-auth audit and limiter, token floor, positive |
| `AuthHardening_level_drops_audit_buggy.cfg` | `AuditIndependentOfLevel = FALSE`: a warn or error process level drops the audit line |
| `AuthHardening_no_fail_audit_buggy.cfg` | `AuditFailedAuth = FALSE`: a 401 writes no audit line |
| `AuthHardening_no_limiter_buggy.cfg` | `LimitFailedAuth = FALSE`: one address guesses without bound |
| `AuthHardening_short_token_buggy.cfg` | `RejectShortTokens = FALSE`: a short secret loads |
| `AuthHardening_limit_success_buggy.cfg` | `LimitSuccessBeforeAuth = TRUE`: an over-budget client is refused with a valid credential |
| `CronTimezone.cfg` | Spring-forward transition, positive |
| `CronTimezone_fall.cfg` | Fall-back transition, positive |
| `CronTimezone_skips_gap_buggy.cfg` | `FireGapAtTransition = FALSE`: a selected time in the skipped hour never fires |
| `CronTimezone_double_fallback_buggy.cfg` | `DoubleFallback = TRUE`: a repeated civil time fires at both offsets |
| `ReleaseGating.cfg` | Gate before publish, draft until complete, sign before tag, no RC is latest, positive |
| `ReleaseGating_no_gate_buggy.cfg` | `RequireGate = FALSE`: the release is published without a successful gate |
| `ReleaseGating_draft_false_buggy.cfg` | `DraftUntilComplete = FALSE`: the release is public before the images, chart, and signatures exist |
| `ReleaseGating_tag_before_sign_buggy.cfg` | `SignBeforeTag = FALSE`: an image tag names an unsigned digest |
| `ReleaseGating_rc_latest_buggy.cfg` | `LatestPrerelease = TRUE`: publish marks a release candidate as `latest` |
| `TokenBudget.cfg` | Admit below budget, one call in flight, charges and reservations persisted; proves that reaching the budget closes the session |
| `TokenBudget_crash.cfg` | The same with two control-plane crashes, positive |
| `TokenBudget_no_inflight_cap_buggy.cfg` | `InflightCap = 2`: two calls admitted at `Budget - 1` exceed the budget by two responses |
| `TokenBudget_check_after_buggy.cfg` | `CheckBefore = FALSE`: the counter is tested after the charge, so a call is admitted over budget |
| `TokenBudget_volatile_counter_buggy.cfg` | `PersistCharge = FALSE`: a restart loses every settled charge |
| `TokenBudget_no_pending_buggy.cfg` | `DurablePending = FALSE`: a charge in flight has no durable coverage |
| `deep/TokenBudget_three_callers.cfg` | Nightly only: three callers, budget 16, two crashes; safety invariants (4,894 distinct states) |
| `deep/SessionResume_two_sessions.cfg` | Nightly only: two sessions, one turn each, one shared control-plane restart; safety invariants (6,127,026 distinct states, about 10.5 minutes with `-workers auto`) |

`HubRetention.cfg` runs eight appends over sizes `{1, 2, 3}` and human posts over
`{1, 2}`, with a two-event count cap, a four-byte thread budget, a two-entry
inbox cap, a three-byte inbox budget, and one dedup key, so both eviction paths,
the event cap, the inbox byte and count caps, and key eviction are reachable
(5859 distinct states).

`BodyReadDeadline.cfg` uses a two-tick deadline and a five-tick bound. The
positive run holds seven distinct states; the no-deadline negative cfg stops
after eight.

`QueuePoison.cfg` uses two jobs, one poison, and `MaxAttempts = 2`, so the
positive run explores 38 states (15 distinct) and proves healthy progress.

`AuthHardening.cfg` uses two addresses, a burst of two failures, four actions,
and a one-key map cap, so the limiter bound and its eviction path are reachable.
The positive run explores 732 states (204 distinct) in under one second. Each
`AuthHardening_*_buggy.cfg` removes one guarantee and stops on the invariant
named in its `expect-violation` line.

`CronTimezone.cfg` and `CronTimezone_fall.cfg` use three modeled days, a
one-hour transition, and one scheduled civil minute. Each holds 8 generated
states (4 distinct) and stops in under a second. Each negative cfg stops after
4 states (3 distinct) on the invariant named in its `expect-violation` line.

`TokenBudget.cfg` holds 31 distinct states (92 generated) in under a second
and proves `ClosedAfterExhaust`; `TokenBudget_crash.cfg` holds 249 (748
generated). The negative cfgs stop on the invariant named in their
`expect-violation` line: no-in-flight-cap 73 (222 generated), check-after 45
(123), volatile-counter 39 (76), and no-pending 11 (14). The nightly
three-caller cfg holds 4,894 (15,153 generated).

`ReleaseGating.cfg` is the smallest model: the positive run holds 134
generated states (52 distinct) in under a second. Its `prerelease` variable
ranges over both a final tag and an RC, so one run checks both `latest`
outcomes. The no-gate negative cfg stops on `NoPublishWithoutGate` (348 states,
108 distinct), the draft-false cfg on `PublicReleaseComplete` (47, 29), the
tag-before-sign cfg on `TagOnlySignedDigests` (111, 50), and the RC-latest cfg
on `RCNeverLatest` (132, 52).

`SessionResume.cfg` holds 34,840 distinct states and finishes in about 18
seconds; `SessionResume_resume.cfg` holds 2,429 and finishes in about a
second. The two new negative cfgs stop within a few hundred states: the
volatile-inbox counterexample is `Create` then `ControlPlaneRestart`, and the
duplicate-redelivery one starts the same message as a second turn.

## Terminal states

The model has an explicit `Stutter` step, so TLC checks liveness over infinite
behaviors instead of stopping at a deadlock. The positive run does not use
`-deadlock`. Every non-terminal state has a fair action enabled, so stuttering
cannot postpone progress forever; a terminal, cleaned session stutters because
the maintenance loop has no work left.

The positive run does not use `SYMMETRY`. TLC warns that symmetry reduction is
unsound for liveness, and with two interchangeable sessions it produced a
spurious `Termination` counterexample. The committed configurations do not use
symmetry.

## Changing the model

Name a new action after the Go path it models, and add it to the action table
in [formal-verification.md](../../docs/formal-verification.md). If you add a
fault switch, add a `*_buggy.cfg` that flips it and expects the invariant the
fault breaks. Keep the turn script in `ToolAt` and `CurrentStep` in step with
the runtime's step order. Keep the new cfg's module name as the prefix before
the first underscore.
