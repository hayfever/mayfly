------------------------------ MODULE TokenBudget ------------------------------
\* A per-session LLM token budget: admit, upstream, charge, crash, recover.
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
\*
\* Three actions follow one call: Admit reserves, Upstream bills and charges
\* memory, Charge settles the record. Crash and Recover model a control-plane
\* restart. Exhaust models the close after exhaustion. Refuse is the admit hook
\* refusing over the budget; it changes no state. The in-flight-cap refusal is
\* an Admit that stays disabled, not an action.
\*
\* The checks are BudgetBound (the spend overshoots by at most one response) and
\* NoChargeLost (the record and its reservation always cover what the provider
\* charged). See docs/formal-verification.md.
EXTENDS Naturals, FiniteSets

CONSTANTS Budget,
          MaxCall,
          Callers,
          MaxCrashes,
          InflightCap,
          CheckBefore,
          PersistCharge,
          DurablePending

VARIABLES spent,      \* control-plane token counter, memory only
          durable,    \* token charge written to the session record
          pending,    \* durable reservation per caller, 0 when not in flight
          inflight,   \* callers with an admitted call
          usage,      \* tokens the in-flight call billed, 0 before the response
          billed,     \* ghost: tokens the provider charged
          up,         \* control plane alive
          closed,     \* session closed after exhaustion
          crashes,    \* control-plane crashes taken
          overBudgetAdmit  \* ghost: a call was admitted at or over Budget

vars == <<spent, durable, pending, inflight, usage, billed,
          up, closed, crashes, overBudgetAdmit>>

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

\* Admit a call. The in-flight cap and the budget check both run before the
\* request. CheckBefore is the implemented order; with FALSE the counter is
\* tested only after the charge, so no admission is prevented. The reservation
\* is MaxCall, the estimate the record holds until the response settles.
Admit(c) ==
  /\ up
  /\ ~closed
  /\ c \notin inflight
  /\ Cardinality(inflight) < InflightCap
  /\ (CheckBefore => spent < Budget)
  /\ pending' = [pending EXCEPT ![c] = IF DurablePending THEN MaxCall ELSE 0]
  /\ inflight' = inflight \cup {c}
  /\ usage' = [usage EXCEPT ![c] = 0]
  /\ overBudgetAdmit' = (overBudgetAdmit \/ spent >= Budget)
  /\ UNCHANGED <<spent, durable, billed, up, closed, crashes>>

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
                 overBudgetAdmit>>

\* Settle the charge: write it to the record, release the reservation, and end
\* the call. PersistCharge is the implemented write; without it the record
\* keeps its old value and the counter lives in memory only.
Charge(c) ==
  /\ up
  /\ c \in inflight
  /\ usage[c] > 0
  /\ durable' = IF PersistCharge THEN durable + usage[c] ELSE durable
  /\ pending' = [pending EXCEPT ![c] = 0]
  /\ inflight' = inflight \ {c}
  /\ usage' = [usage EXCEPT ![c] = 0]
  /\ UNCHANGED <<spent, billed, up, closed, crashes, overBudgetAdmit>>

\* The admit hook refuses the call, so nothing reaches the provider and no
\* counter moves. The runtime's retry of a busy or exhausted refusal is outside
\* the model.
Refuse(c) ==
  /\ up
  /\ ~closed
  /\ spent >= Budget
  /\ UNCHANGED vars

\* A control-plane crash: memory is lost, the record and its reservations are
\* not, and the provider's charge stands.
Crash ==
  /\ up
  /\ crashes < MaxCrashes
  /\ up' = FALSE
  /\ crashes' = crashes + 1
  /\ inflight' = {}
  /\ usage' = [c \in Callers |-> 0]
  /\ UNCHANGED <<spent, durable, pending, billed, closed, overBudgetAdmit>>

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
  /\ UNCHANGED <<billed, closed, crashes, overBudgetAdmit>>

\* Exhaustion closes the session. Closing disables Admit; a charge already in
\* flight still settles.
Exhaust ==
  /\ up
  /\ ~closed
  /\ spent >= Budget
  /\ closed' = TRUE
  /\ UNCHANGED <<spent, durable, pending, inflight, usage, billed, up,
                 crashes, overBudgetAdmit>>

Stutter == UNCHANGED vars

Next ==
  \/ \E c \in Callers : Admit(c)
  \/ \E c \in Callers : \E k \in 1 .. MaxCall : Upstream(c, k)
  \/ \E c \in Callers : Charge(c)
  \/ \E c \in Callers : Refuse(c)
  \/ Crash
  \/ Recover
  \/ Exhaust
  \/ Stutter

\* Exhaust closes once the counter reaches Budget, and a crash is always
\* followed by a restart; without that fairness a stopped control plane would
\* never close the session.
Spec == Init /\ [][Next]_vars /\ WF_vars(Exhaust) /\ WF_vars(Recover)

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

\* Once the counter reaches Budget, the session closes. Weak fairness on
\* Exhaust supplies the step, and weak fairness on Recover makes a crashed
\* control plane come back so the close is reachable.
ClosedAfterExhaust == (spent >= Budget) ~> closed

=============================================================================
