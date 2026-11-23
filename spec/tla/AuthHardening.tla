---------------------------- MODULE AuthHardening ----------------------------
\* The public API authentication boundary, as internal/httpapi implements it.
\*
\* One request at a time presents a credential or mutates state:
\*   - FailedAuth is a refused bearer or web-session credential. It writes an
\*     audit line when AuditFailedAuth is on, counts the failure, and spends a
\*     token from the per-IP bucket when LimitFailedAuth is on.
\*   - Mutating is an authenticated POST, PUT, PATCH, or DELETE. It writes one
\*     audit line when AuditIndependentOfLevel is on, or when the process log
\*     is at info.
\*   - Success is a valid credential. It always proceeds. LimitSuccessBeforeAuth
\*     models the bug of checking the failure limiter before authentication.
\*   - LoadShortToken models a resolved secret shorter than 32 characters. The
\*     load accepts it only when RejectShortTokens is off.
\*
\* The positive cfg sets every guarantee on. A *_buggy.cfg turns one constant
\* off and must violate the invariant named by its expect-violation line.
EXTENDS Naturals, FiniteSets

CONSTANTS
    \* Client addresses the limiter can key on.
    IPs,
    \* Failed attempts one client may spend in the modeled window.
    Burst,
    \* Bound on total modeled actions, so the state space is finite.
    MaxOps,
    \* Cap on tracked client keys.
    MaxKeys,
    \* TRUE: the audit logger ignores the process log level (#46 fix).
    AuditIndependentOfLevel,
    \* TRUE: a refused authentication writes an audit line (#47 fix).
    AuditFailedAuth,
    \* TRUE: failed authentication spends a per-IP bucket (#47 fix).
    LimitFailedAuth,
    \* TRUE: a short resolved token secret is rejected at load (#47 fix).
    RejectShortTokens,
    \* TRUE: models the bug of applying the limiter before authentication.
    LimitSuccessBeforeAuth

VARIABLES
    level,
    step,
    attempts,
    tracked,
    mutationHappened,
    mutationAudited,
    failureHappened,
    failureAudited,
    successHappened,
    successLimited,
    shortTokenAccepted

vars ==
    <<level, step, attempts, tracked, mutationHappened, mutationAudited,
      failureHappened, failureAudited, successHappened, successLimited,
      shortTokenAccepted>>

\* An audit record survives a warn or error process level only when the audit
\* logger is independent, or when the process level happens to be info.
AuditPasses == AuditIndependentOfLevel \/ level = "info"

Init ==
    /\ level \in {"info", "warn", "error"}
    /\ step = 0
    /\ attempts = [ip \in IPs |-> 0]
    /\ tracked = {}
    /\ mutationHappened = FALSE
    /\ mutationAudited = FALSE
    /\ failureHappened = FALSE
    /\ failureAudited = FALSE
    /\ successHappened = FALSE
    /\ successLimited = FALSE
    /\ shortTokenAccepted = FALSE

\* The failure map keeps at most MaxKeys entries. A new key replaces the least
\* recently seen one when the map is full; the model does not record order, so
\* any resident key may be the victim.
Track(ip) ==
    IF ip \in tracked
    THEN tracked
    ELSE IF Cardinality(tracked) < MaxKeys
         THEN tracked \cup {ip}
         ELSE (tracked \ {CHOOSE victim \in tracked: TRUE}) \cup {ip}

\* A failed authentication is allowed while the bucket has a token. Without a
\* limiter every attempt is allowed, so attempts grow to MaxOps.
FailedAuth(ip) ==
    /\ step < MaxOps
    /\ (~LimitFailedAuth \/ attempts[ip] < Burst)
    /\ attempts' = [attempts EXCEPT ![ip] = attempts[ip] + 1]
    /\ failureHappened' = TRUE
    /\ failureAudited' = (failureAudited \/ (AuditFailedAuth /\ AuditPasses))
    /\ tracked' = IF LimitFailedAuth THEN Track(ip) ELSE tracked
    /\ step' = step + 1
    /\ UNCHANGED
        <<level, mutationHappened, mutationAudited, successHappened,
          successLimited, shortTokenAccepted>>

\* A valid credential always proceeds. The buggy variant consults the failure
\* bucket first, so an over-budget client is refused even with a good token.
Success(ip) ==
    /\ step < MaxOps
    /\ IF LimitSuccessBeforeAuth /\ LimitFailedAuth /\ attempts[ip] >= Burst
       THEN /\ successLimited' = TRUE
            /\ step' = step + 1
            /\ UNCHANGED
                <<level, attempts, tracked, mutationHappened, mutationAudited,
                  failureHappened, failureAudited, successHappened,
                  shortTokenAccepted>>
       ELSE /\ successHappened' = TRUE
            /\ step' = step + 1
            /\ UNCHANGED
                <<level, attempts, tracked, mutationHappened, mutationAudited,
                  failureHappened, failureAudited, successLimited,
                  shortTokenAccepted>>

\* An authenticated mutating request writes one audit line. The buggy variant
\* routes it through the level-filtered process logger.
Mutating ==
    /\ step < MaxOps
    /\ mutationHappened' = TRUE
    /\ mutationAudited' = (mutationAudited \/ AuditPasses)
    /\ step' = step + 1
    /\ UNCHANGED
        <<level, attempts, tracked, failureHappened, failureAudited,
          successHappened, successLimited, shortTokenAccepted>>

\* A short secret is accepted only when the load rule is off.
LoadShortToken ==
    /\ ~RejectShortTokens
    /\ ~shortTokenAccepted
    /\ shortTokenAccepted' = TRUE
    /\ UNCHANGED
        <<level, step, attempts, tracked, mutationHappened, mutationAudited,
          failureHappened, failureAudited, successHappened, successLimited>>

Stutter == UNCHANGED vars

Next ==
    \/ \E ip \in IPs: FailedAuth(ip)
    \/ \E ip \in IPs: Success(ip)
    \/ Mutating
    \/ LoadShortToken
    \/ Stutter

Spec == Init /\ [][Next]_vars

TypeOK ==
    /\ level \in {"info", "warn", "error"}
    /\ step \in 0..MaxOps
    /\ attempts \in [IPs -> 0..MaxOps]
    /\ tracked \in SUBSET IPs
    /\ mutationHappened \in BOOLEAN
    /\ mutationAudited \in BOOLEAN
    /\ failureHappened \in BOOLEAN
    /\ failureAudited \in BOOLEAN
    /\ successHappened \in BOOLEAN
    /\ successLimited \in BOOLEAN
    /\ shortTokenAccepted \in BOOLEAN

\* #46: a mutating request is audited even when the process level is warn or
\* error.
AuditAlwaysEmitted == mutationHappened => mutationAudited

\* #47: a refused authentication is audited.
FailedAuthAudited == failureHappened => failureAudited

\* #47: a valid credential is never limited.
SuccessfulAuthNeverLimited == successLimited = FALSE

\* #47: the per-IP failure map stays within its cap.
LimiterBounded == Cardinality(tracked) <= MaxKeys

\* #47: short resolved secrets are refused at load.
ShortTokenRejected == ~shortTokenAccepted

\* #47: without a success, one address cannot spend more than one burst of
\* failed attempts. Refill over time is abstracted; Burst is the modeled window.
FailedAuthBounded == \A ip \in IPs: attempts[ip] <= Burst

=============================================================================
