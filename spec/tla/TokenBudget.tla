------------------------------ MODULE TokenBudget ------------------------------
\* A per-session LLM token budget: admit, upstream, observe, charge, crash,
\* recover.
\*
\* The control plane counts prompt and completion tokens for one session. A call
\* is admitted only while the recorded spend is below Budget, at most
\* InflightCap calls run at once, and each call reserves its estimate in the
\* session record before the upstream request starts. The provider charges an
\* amount from 1 to MaxCall; the estimate is MaxCall, an upper bound. After the
\* response arrives the charge is applied in memory and written to the record,
\* the reservation is released, and a session at or over Budget is closed.
\*
\* A control-plane crash loses memory, not the record. Recovery seeds the
\* counter from the record plus its outstanding reservation, so no charge the
\* provider made can disappear.
\*
\* Two caller-visible steps follow one call, in this order. Observe is the
\* client's read of the reply finished: for a stream that is "data: [DONE]"
\* (internal/llm/client.go), which arrives before upstream EOF. Charge is the
\* broker reaching EOF, which settles the ledger and releases the slot
\* (internal/broker/broker.go, then the charge in internal/httpapi/internal.go).
\* The reply is therefore complete on the wire while the call still holds the
\* slot. AttemptBusy is a caller's next call arriving while another caller's
\* reply is already complete. WaitForSettle is TRUE for the fixed client, which
\* does not race a reply it has already seen; FALSE is the pre-fix e2e test
\* assumption that "data: [DONE]" means the call settled.
\*
\* Constants and switches:
\*
\*   Budget          per-session token limit, as sessions.max_llm_tokens
\*   MaxCall         largest prompt+completion charge of one call
\*   Callers         callers that can reach the model, such as {c1, c2}
\*   MaxCrashes      control-plane crashes the run may take
\*   InflightCap     concurrent calls allowed; 1 when a budget is set
\*   CheckBefore     refuse before the charge instead of after it
\*   PersistCharge   write every charge to the session record
\*   DurablePending  write the per-call reservation to the session record
\*   WaitForSettle   TRUE: the caller waits for the settle, not for [DONE]
\*
\* Three actions follow one call: Admit reserves, Upstream bills and charges
\* memory, Charge settles the record. Observe records that the client has the
\* whole reply, and AttemptBusy is the next call refused while the slot is held.
\* Crash and Recover model a control-plane restart. Exhaust models the close
\* after exhaustion. Refuse is the admit hook refusing over the budget; it
\* changes no state. Retry is the fixed client's retry of a refused call.
\*
\* The checks are BudgetBound (the spend overshoots by at most one response),
\* NoChargeLost (the record and its reservation always cover what the provider
\* charged), ObserveKeepsSlot (an observed call is still in flight), and
\* NoBusyAfterSettled (a client that waits for the settle is never refused
\* busy). BusyRetryAdmitted is the retry liveness property. See
\* docs/formal-verification.md.
EXTENDS Naturals, FiniteSets

CONSTANTS Budget,
          MaxCall,
          Callers,
          MaxCrashes,
          InflightCap,
          CheckBefore,
          PersistCharge,
          DurablePending,
          WaitForSettle

VARIABLES spent,      \* control-plane token counter, memory only
          durable,    \* token charge written to the session record
          pending,    \* durable reservation per caller, 0 when not in flight
          inflight,   \* callers with an admitted call
          usage,      \* tokens the in-flight call billed, 0 before the response
          billed,     \* ghost: tokens the provider charged
          up,         \* control plane alive
          closed,     \* session closed after exhaustion
          crashes,    \* control-plane crashes taken
          overBudgetAdmit,  \* ghost: a call was admitted at or over Budget
          observed,   \* callers whose in-flight reply the client has read
          waiting,    \* callers refused busy whose retry has not run
          surpriseBusy  \* ghost: a refusal while the reply was already visible

vars == <<spent, durable, pending, inflight, usage, billed,
          up, closed, crashes, overBudgetAdmit, observed, waiting,
          surpriseBusy>>

\* The reservations are summed per caller: two callers may hold the same value.
RECURSIVE SumReserved(_)
SumReserved(cs) ==
  IF cs = {} THEN 0
  ELSE LET c == CHOOSE d \in cs : TRUE IN pending[c] + SumReserved(cs \ {c})

PendingTotal == SumReserved(Callers)

Init ==
  /\ spent = 0
  /\ durable = 0
  /\ pending = [c \in Callers |-> 0]
  /\ inflight = {}
  /\ usage = [c \in Callers |-> 0]
  /\ billed = 0
  /\ up = TRUE
  /\ closed = FALSE
  /\ crashes = 0
  /\ overBudgetAdmit = FALSE
  /\ observed = {}
  /\ waiting = {}
  /\ surpriseBusy = 0

\* The admit hook's decision and its reservation, reached by a fresh call
\* (Admit) and by a refused caller's retry (Retry). The in-flight cap and the
\* budget check both run before the request. CheckBefore is the implemented
\* order; with FALSE the counter is tested only after the charge, so no
\* admission is prevented. The reservation is MaxCall, the estimate the record
\* holds until the response settles.
AdmitCall(c) ==
  /\ up
  /\ ~closed
  /\ c \notin inflight
  /\ Cardinality(inflight) < InflightCap
  /\ (CheckBefore => spent < Budget)
  /\ pending' = [pending EXCEPT ![c] = IF DurablePending THEN MaxCall ELSE 0]
  /\ inflight' = inflight \cup {c}
  /\ usage' = [usage EXCEPT ![c] = 0]
  /\ overBudgetAdmit' = (overBudgetAdmit \/ spent >= Budget)
  /\ observed' = observed
  /\ UNCHANGED <<spent, durable, billed, up, closed, crashes>>

\* A caller starts a call it has not attempted before.
Admit(c) ==
  /\ c \notin waiting
  /\ waiting' = waiting
  /\ surpriseBusy' = surpriseBusy
  /\ AdmitCall(c)

\* The provider answers with a charge from 1 to MaxCall. It lands in memory and
\* in the ghost at once, because the control plane has already been billed.
Upstream(c, k) ==
  /\ up
  /\ c \in inflight
  /\ usage[c] = 0
  /\ k \in 1 .. MaxCall
  /\ billed' = billed + k
  /\ spent' = spent + k
  /\ usage' = [usage EXCEPT ![c] = k]
  /\ UNCHANGED <<durable, pending, inflight, up, closed, crashes,
                 overBudgetAdmit, observed, waiting, surpriseBusy>>

\* The client's read of the reply finished: it has the whole response, and the
\* call keeps its slot. A JSON reply reaches the client and EOF together; the
\* model covers the streamed case, where [DONE] arrives before EOF.
Observe(c) ==
  /\ up
  /\ c \in inflight
  /\ usage[c] > 0
  /\ c \notin observed
  /\ observed' = observed \cup {c}
  /\ UNCHANGED <<spent, durable, pending, inflight, usage, billed, up, closed,
                 crashes, overBudgetAdmit, waiting, surpriseBusy>>

\* Settle the charge: write it to the record, release the reservation, and end
\* the call. PersistCharge is the implemented write; without it the record
\* keeps its old value and the counter lives in memory only. A settle follows
\* the client's read, because the broker reaches EOF after the last reply
\* bytes.
Charge(c) ==
  /\ up
  /\ c \in inflight
  /\ c \in observed
  /\ usage[c] > 0
  /\ durable' = IF PersistCharge THEN durable + usage[c] ELSE durable
  /\ pending' = [pending EXCEPT ![c] = 0]
  /\ inflight' = inflight \ {c}
  /\ usage' = [usage EXCEPT ![c] = 0]
  /\ observed' = observed \ {c}
  /\ UNCHANGED <<spent, billed, up, closed, crashes, overBudgetAdmit, waiting,
                 surpriseBusy>>

\* A caller's next call arrives while another caller's reply is complete, so
\* the slot is held and the call is refused busy. WaitForSettle is the client's
\* policy: the fixed client does not race a reply it has already read, and a
\* client that treats [DONE] as settled does. The ghost counts a refusal taken
\* while the held reply was already visible, which is the refusal the buggy
\* client policy meets.
AttemptBusy(c) ==
  /\ up
  /\ ~closed
  /\ c \notin inflight
  /\ c \notin waiting
  /\ Cardinality(inflight) >= InflightCap
  /\ \A h \in inflight : usage[h] > 0
  /\ (WaitForSettle => \A h \in inflight : h \notin observed)
  /\ waiting' = waiting \cup {c}
  /\ surpriseBusy' = surpriseBusy + (IF \E h \in inflight : h \in observed THEN 1 ELSE 0)
  /\ UNCHANGED <<spent, durable, pending, inflight, usage, billed, up, closed,
                 crashes, overBudgetAdmit, observed>>

\* The fixed client retries a refused call once the slot and the budget allow
\* it. It is the same admit decision as Admit; weak fairness on Retry is what
\* the BusyRetryAdmitted property checks.
Retry(c) ==
  /\ c \in waiting
  /\ waiting' = waiting \ {c}
  /\ surpriseBusy' = surpriseBusy
  /\ AdmitCall(c)

\* The admit hook refuses the call, so nothing reaches the provider and no
\* counter moves. Refuse is the exhausted refusal; the runtime's retry of a
\* busy refusal is Retry, and its retry of an exhausted one is outside the
\* model.
Refuse(c) ==
  /\ up
  /\ ~closed
  /\ spent >= Budget
  /\ UNCHANGED vars

\* A control-plane crash: memory is lost, the record and its reservations are
\* not, and the provider's charge stands. The open call is gone, so its
\* observation goes with it; a waiting client keeps its retry.
Crash ==
  /\ up
  /\ crashes < MaxCrashes
  /\ up' = FALSE
  /\ crashes' = crashes + 1
  /\ inflight' = {}
  /\ usage' = [c \in Callers |-> 0]
  /\ observed' = {}
  /\ UNCHANGED <<spent, durable, pending, billed, closed, overBudgetAdmit,
                 waiting, surpriseBusy>>

\* Recovery seeds the counter from the record plus each outstanding reservation,
\* charges an unsettled reservation at its estimate, and clears it. The seeded
\* value is written back, so the record keeps the settlement.
Recover ==
  /\ ~up
  /\ up' = TRUE
  /\ spent' = durable + PendingTotal
  /\ durable' = durable + PendingTotal
  /\ pending' = [c \in Callers |-> 0]
  /\ inflight' = {}
  /\ usage' = [c \in Callers |-> 0]
  /\ UNCHANGED <<billed, closed, crashes, overBudgetAdmit, observed, waiting,
                 surpriseBusy>>

\* Exhaustion closes the session. Closing disables Admit; a charge already in
\* flight still settles.
Exhaust ==
  /\ up
  /\ ~closed
  /\ spent >= Budget
  /\ closed' = TRUE
  /\ UNCHANGED <<spent, durable, pending, inflight, usage, billed, up,
                 crashes, overBudgetAdmit, observed, waiting, surpriseBusy>>

Stutter == UNCHANGED vars

Next ==
  \/ \E c \in Callers : Admit(c)
  \/ \E c \in Callers : \E k \in 1 .. MaxCall : Upstream(c, k)
  \/ \E c \in Callers : Observe(c)
  \/ \E c \in Callers : Charge(c)
  \/ \E c \in Callers : AttemptBusy(c)
  \/ \E c \in Callers : Retry(c)
  \/ \E c \in Callers : Refuse(c)
  \/ Crash
  \/ Recover
  \/ Exhaust
  \/ Stutter

\* Exhaust closes once the counter reaches Budget, and a crash is always
\* followed by a restart; without that fairness a stopped control plane would
\* never close the session. Upstream is fair because the provider answers,
\* Observe is fair because the answer reaches the client, and Charge is fair
\* because the broker's read ends; Retry is fair because the runtime retries a
\* busy refusal. Together they prove that a refused call is eventually admitted.
Spec == Init /\ [][Next]_vars /\ WF_vars(Exhaust) /\ WF_vars(Recover)
        /\ (\A c \in Callers : WF_vars(\E k \in 1 .. MaxCall : Upstream(c, k)))
        /\ (\A c \in Callers : WF_vars(Observe(c)))
        /\ (\A c \in Callers : WF_vars(Charge(c)))
        /\ (\A c \in Callers : WF_vars(Retry(c)))

TypeOK ==
  /\ spent \in Nat
  /\ durable \in Nat
  /\ billed \in Nat
  /\ pending \in [Callers -> 0 .. MaxCall]
  /\ inflight \subseteq Callers
  /\ usage \in [Callers -> 0 .. MaxCall]
  /\ up \in BOOLEAN
  /\ closed \in BOOLEAN
  /\ crashes \in 0 .. MaxCrashes
  /\ overBudgetAdmit \in BOOLEAN
  /\ observed \subseteq Callers
  /\ waiting \subseteq Callers
  /\ surpriseBusy \in Nat

\* A call is admitted only while the counter is below Budget, and one call can
\* add at most MaxCall. The provider's charge therefore stays within one
\* response of the limit.
BudgetBound == billed <= Budget - 1 + MaxCall

\* While the control plane is up the counter covers every charge. The record
\* plus its reservation covers every charge at all times, so a crash cannot
\* lose one.
NoChargeLost ==
  /\ up => spent >= billed
  /\ durable + PendingTotal >= billed

NoAdmitWhenExhausted == ~overBudgetAdmit

\* The client's read of the reply does not settle the call: an observed call is
\* still in flight, still holds the session's slot, and is not yet charged.
ObserveKeepsSlot == \A c \in observed : c \in inflight

\* A client that waits for the settle after a visible reply is never refused
\* busy. The ghost counts a refusal taken while the held reply was already
\* visible, so this holds exactly while WaitForSettle is on.
NoBusyAfterSettled == surpriseBusy = 0

\* Once the counter reaches Budget, the session closes. Weak fairness on
\* Exhaust supplies the step, and weak fairness on Recover makes a crashed
\* control plane come back so the close is reachable.
ClosedAfterExhaust == (spent >= Budget) ~> closed

\* A call refused busy is eventually admitted, or the session closes first. The
\* slot frees when the holder charges, and Retry admits the waiting caller
\* while the budget allows it.
BusyRetryAdmitted == (waiting /= {}) ~> (waiting = {} \/ closed)

=============================================================================
