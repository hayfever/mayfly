# TLA+ lifecycle, retention, and queue models

`SessionResume.tla` models the Mayfly session lifecycle and resume protocol as
implemented. `HubRetention.tla` models the in-memory hub: the per-event cap,
the retained thread byte budget, the thread and inbox count caps, the inbox
exemption, the dedup-key table, and the restore path. `QueuePoison.tla` models
a queue with a poison job: claim, handler panic, Nack, lease expiry, restart,
and burial at `max_attempts`. A cfg runs the module named by its basename up to
the first underscore, so `HubRetention_evict_inbox_buggy.cfg` runs
`HubRetention.tla`. [formal-verification.md](../../docs/formal-verification.md)
maps each action to the Go code it models, lists the invariants, and states
what the models abstract away.

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

- `SessionResume.cfg`, `HubRetention.cfg`, and `QueuePoison.cfg` are the positive
  configurations. Each must pass with no invariant violation;
  `SessionResume.cfg` and `QueuePoison.cfg` must also prove their liveness
  properties.
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
| `HubRetention.cfg` | Hub byte budget, event cap, contiguous suffix, inbox exemption, inbox cap, dedup keys, positive |
| `HubRetention_evict_inbox_buggy.cfg` | `ExemptInbox = FALSE`: a runtime flood drops an unconsumed human message |
| `HubRetention_no_cap_buggy.cfg` | `CapEvents = FALSE`: a retained event exceeds the per-event cap |
| `HubRetention_inbox_overflow_buggy.cfg` | `CapInbox = FALSE`: the inbox grows past its count cap |
| `HubRetention_key_leak_buggy.cfg` | `EvictKeys = FALSE`: a dedup key outlives its evicted event |
| `QueuePoison.cfg` | Claim, recovered panic, Nack, burial, and healthy progress, positive |
| `QueuePoison_no_recover_buggy.cfg` | `RecoverPanics = FALSE`: a handler panic crashes the worker |
| `QueuePoison_no_claim_count_buggy.cfg` | `RecoverPanics = FALSE` and `CountAttemptOnClaim = FALSE`: a crash loop never reaches `max_attempts` |
| `deep/SessionResume_two_sessions.cfg` | Nightly only: two sessions, one turn each, one shared control-plane restart; safety invariants (6,127,026 distinct states, about 10.5 minutes with `-workers auto`) |

`HubRetention.cfg` runs eight appends over sizes `{1, 2, 3}` with a two-event
count cap, a four-byte budget, a two-entry inbox cap, and one dedup key, so
both eviction paths, the event cap, the inbox cap, and key eviction are
reachable (2458 distinct states).

`QueuePoison.cfg` uses two jobs, one poison, and `MaxAttempts = 2`, so the
positive run explores 38 states (15 distinct) and proves healthy progress.

`SessionResume.cfg` holds 34,840 distinct states and finishes in about 12

`SessionResume.cfg` holds 34,840 distinct states and finishes in about 18
seconds; `SessionResume_resume.cfg` holds 2,429 and finishes in about a
second. The two new negative cfgs stop after 2 and 111 distinct states, which
is enough to show the invariant they target bites.

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
