----------------------------- MODULE QueuePoison -----------------------------
\* One queue with a poison job and a healthy job, as internal/queue settles it.
\*
\* A worker claims a job, runs a handler, and settles the attempt:
\*   - Claim leases a ready job. With CountAttemptOnClaim, the claim adds the
\*     attempt before the handler runs; a job that already used MaxAttempts is
\*     buried instead of claimed.
\*   - Handle runs the handler. The healthy job completes. The poison job
\*     panics: with RecoverPanics the worker catches the panic and Nacks the
\*     job, otherwise the process crashes and the lease stays until it expires.
\*   - ExpireLease returns a crashed worker's lease to the queue after the
\*     visibility timeout. Restart brings the worker back.
\*
\* RecoverPanics and CountAttemptOnClaim model implemented guarantees. Set one
\* to FALSE in a *_buggy.cfg to remove that guarantee and show the invariant it
\* protects.
EXTENDS Naturals

CONSTANTS Jobs, Poison, MaxAttempts, RecoverPanics, CountAttemptOnClaim

VARIABLES phase, attempts, crashes, crashed

vars == <<phase, attempts, crashes, crashed>>

\* The worker is down after a handler panic. No job can be claimed until a
\* restart and a lease expiry.
Init ==
    /\ phase = [j \in Jobs |-> "ready"]
    /\ attempts = [j \in Jobs |-> 0]
    /\ crashes = [j \in Jobs |-> 0]
    /\ crashed = FALSE

\* CountAttemptOnClaim makes every claim consume one attempt, including a claim
\* that follows a crashed process. Without it, only a Nack consumes an attempt,
\* so a job whose worker always crashes is claimed forever.
ClaimedAttempts(j) ==
    IF CountAttemptOnClaim THEN attempts[j] + 1 ELSE attempts[j]

\* A job with no attempt left is buried on the next claim instead of running
\* again. This is how an unrecoverable poison job leaves the queue.
Spent(j) ==
    CountAttemptOnClaim /\ MaxAttempts > 0 /\ attempts[j] >= MaxAttempts

Claim(j) ==
    /\ ~crashed
    /\ phase[j] = "ready"
    /\ IF Spent(j)
         THEN /\ phase' = [phase EXCEPT ![j] = "dead"]
              /\ UNCHANGED <<attempts, crashes, crashed>>
         ELSE /\ phase' = [phase EXCEPT ![j] = "leased"]
              /\ attempts' = [attempts EXCEPT ![j] = ClaimedAttempts(j)]
              /\ UNCHANGED <<crashes, crashed>>

\* The handler of a poisoned job panics. A recovered panic becomes a Nack: the
\* attempt is settled, and the job is buried when it has no attempt left. An
\* unrecovered panic crashes the process and increments the crash count.
Handle(j) ==
    /\ ~crashed
    /\ phase[j] = "leased"
    /\ IF j \in Poison
         THEN IF RecoverPanics
                THEN LET a == IF CountAttemptOnClaim THEN attempts[j] ELSE attempts[j] + 1
                     IN IF MaxAttempts > 0 /\ a >= MaxAttempts
                          THEN /\ phase' = [phase EXCEPT ![j] = "dead"]
                               /\ attempts' = [attempts EXCEPT ![j] = a]
                               /\ UNCHANGED <<crashes, crashed>>
                          ELSE /\ phase' = [phase EXCEPT ![j] = "ready"]
                               /\ attempts' = [attempts EXCEPT ![j] = a]
                               /\ UNCHANGED <<crashes, crashed>>
                ELSE /\ crashed' = TRUE
                     /\ crashes' = [crashes EXCEPT ![j] = crashes[j] + 1]
                     /\ UNCHANGED <<phase, attempts>>
         ELSE /\ phase' = [phase EXCEPT ![j] = "done"]
              /\ UNCHANGED <<attempts, crashes, crashed>>

\* The visibility timeout returns a crashed worker's lease to the queue. The
\* reclaim itself does not count an attempt: Claim does.
ExpireLease(j) ==
    /\ crashed
    /\ phase[j] = "leased"
    /\ phase' = [phase EXCEPT ![j] = "ready"]
    /\ UNCHANGED <<attempts, crashes, crashed>>

Restart ==
    /\ crashed
    /\ crashed' = FALSE
    /\ UNCHANGED <<phase, attempts, crashes>>

Stutter == UNCHANGED vars

Next ==
    \/ \E j \in Jobs : Claim(j)
    \/ \E j \in Jobs : Handle(j)
    \/ \E j \in Jobs : ExpireLease(j)
    \/ Restart
    \/ Stutter

\* Fairness is the worker's own progress. Faults get no fairness: liveness must
\* not depend on a crash or on the bounded retries of a poison job.
Fairness ==
    /\ \A j \in Jobs : WF_vars(Claim(j))
    /\ \A j \in Jobs : WF_vars(Handle(j))
    /\ \A j \in Jobs : WF_vars(ExpireLease(j))
    /\ WF_vars(Restart)

Spec == Init /\ [][Next]_vars /\ Fairness

\* ------------------------------------------------------------
(* Safety *)
\* ------------------------------------------------------------

TypeOK ==
    /\ phase \in [Jobs -> {"ready", "leased", "done", "dead"}]
    /\ attempts \in [Jobs -> 0..MaxAttempts]
    /\ crashes \in [Jobs -> Nat]
    /\ crashed \in BOOLEAN

\* No job ever runs more attempts than the configured limit. The counter stops
\* at MaxAttempts because the next claim buries the job.
AttemptsWithinMax ==
    \A j \in Jobs : attempts[j] <= MaxAttempts

\* Every job is in exactly one lifecycle state. A job is never dropped between
\* the queue and the dead set.
NoLostJob ==
    \A j \in Jobs : phase[j] \in {"ready", "leased", "done", "dead"}

\* A handler panic never crashes the worker. RecoverPanics is the guarantee.
NoPanicCrash ==
    \A j \in Jobs : crashes[j] = 0

\* A poison job can crash the process at most MaxAttempts times, even when the
\* panic is not recoverable, because every claim consumes an attempt.
CrashesBounded ==
    \A j \in Jobs : crashes[j] <= MaxAttempts

\* ------------------------------------------------------------
(* Liveness *)
\* ------------------------------------------------------------

\* A healthy job is never stuck behind a poison job: it is eventually done
\* while the poison job retries or is buried.
HealthyJobsProgress ==
    <>(\A j \in Jobs \ Poison : phase[j] = "done")

=============================================================================
