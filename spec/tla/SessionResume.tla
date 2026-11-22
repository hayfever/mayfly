------------------------------ MODULE SessionResume ------------------------------
(*
  Mayfly session lifecycle and resume protocol, as implemented.

  The model follows the Go control plane and runtime:

    * a session is created with a Secret, ConfigMap, NetworkPolicy, PVC, and
      a Pod (internal/session/lifecycle.go: Create, internal/runner);
    * a user message takes its turn key from the in-memory hub sequence, after
      the journal has durably reserved that sequence (session.PostMessage,
      session.reserveSequence, journal.Sequencer.Reserve);
    * the runtime journals each turn step before applying it to the
      conversation, and tools go through the gateway ledger
      (agent.handleTurn, gateway.Begin);
    * a pod death loses only in-memory runtime state; the journal, ledger,
      sequence floor, and session record survive;
    * a message is written to a durable pending-inbox record before its sender
      is acknowledged, and the record is removed when the turn's step 0 is
      journaled;
    * a control-plane restart loses the hub, in-flight begins, and registered
      instances; journal, ledger, sequence floor, records, and the pending
      inbox survive;
    * adoption seeds the hub above the durable floor, redelivers every pending
      message under its reserved turn key, and refuses a session a DELETE
      removed;
    * a replacement pod resumes the last journaled turn with a new runtime
      instance and no re-execution of a recorded result.

  Turn script. Every turn has the fixed shape the tests exercise:

    step 0  user          (journal entry, conversation line)
    step 1  llm           (model response that requests tools)
    step 2  tool:<idem>   (one idempotent call)
    step 3  tool:<non>    (one non-idempotent call)
    step 4  assistant     (terminal model response)

  Every step is journaled before it is applied to the conversation, and a
  step is journaled only after the previous step is applied, so ConvLen is
  monotone and the conversation never runs ahead of the journal. Truncation
  on resume is therefore not reachable in this abstraction.

  See docs/formal-verification.md for the action-to-Go map and for what the
  model abstracts away. See spec/tla/README.md for the cfg convention.
*)

EXTENDS Naturals, FiniteSets

MaxNat(a, b) == IF a >= b THEN a ELSE b

CONSTANTS
    Sessions,             \* session identifiers
    MaxTurns,             \* user messages per session
    MaxRestarts,          \* control-plane restarts
    MaxPodKills,          \* pod deletions
    MaxReplacements,      \* replacement pods (sessions.max_restarts)
    Block,                \* journal sequence block (sequenceBlock)
    ToolIdem,             \* an idempotent tool
    ToolNonIdem,          \* a non-idempotent tool
    LedgerBeforeDispatch, \* FALSE is the negative check: record after dispatch
    InterruptOnResume,    \* FALSE is a second negative check: re-run a pending call
    DurableInbox,         \* FALSE is the negative check: no pending record
    RedeliverNewKey       \* TRUE is the negative check: redeliver under a new key

VARIABLES
    created, registered, seeded, deleted, status, final, closeReason,
    objects, pod, restarts, podKills, cpRestarts, instCounter,
    hubSeq, seqFloor, posted, pending, accepted, started, issued,
    journal, convSize, entryLen, applies,
    ledger, ledgerInst, doneExec, callState, execCount

vars == <<
    created, registered, seeded, deleted, status, final, closeReason,
    objects, pod, restarts, podKills, cpRestarts, instCounter,
    hubSeq, seqFloor, posted, pending, accepted, started, issued,
    journal, convSize, entryLen, applies,
    ledger, ledgerInst, doneExec, callState, execCount
>>

\* ------------------------------------------------------------
(* Static structure *)
\* ------------------------------------------------------------

LastStep == 4
Steps == 0..LastStep
ToolSteps == {2, 3}

\* Upper bound on a turn key. A restart seeds the hub to at most the reserved
\* floor, and each reservation adds at most Block, so no reachable key exceeds
\* MaxTurns + MaxRestarts * Block + Block.
MaxSeq == MaxTurns + MaxRestarts * Block + Block
Turns == 1..MaxSeq
Keys == Sessions \X Turns \X Steps

\* Each session gets one pod at create and at most MaxReplacements after that.
MaxInstances == Cardinality(Sessions) * (1 + MaxReplacements)
Instances == 0..MaxInstances

Resources == {"secret", "configmap", "netpol", "pvc"}

ToolAt(k) ==
    CASE k = 2 -> ToolIdem
      [] k = 3 -> ToolNonIdem
      [] OTHER -> "none"

Idem(tool) == tool = ToolIdem

Terminal(s) == final[s] /= "none"

\* The runtime can act only through a live pod that the current control-plane
\* lifetime knows about, and only before the session finalizes.
RuntimeEnabled(s) ==
    /\ pod[s] /= 0
    /\ registered[s]
    /\ final[s] = "none"

TurnsOf(s) == {t \in Turns : <<t, LastStep>> \in journal[s]}
PendingTurns(s) == posted[s] \ TurnsOf(s)
NextTurn(s) == CHOOSE t \in PendingTurns(s) : \A u \in PendingTurns(s) : t <= u
CurrentStep(s, t) == Cardinality({k \in Steps : <<t, k>> \in journal[s]})

JournalTurns(s) == {t \in Turns : \E k \in Steps : <<t, k>> \in journal[s]}
MaxJournalTurn(s) ==
    IF JournalTurns(s) = {}
    THEN 0
    ELSE CHOOSE t \in JournalTurns(s) : \A u \in JournalTurns(s) : u <= t

CanWork(s) ==
    /\ RuntimeEnabled(s)
    /\ status[s] = "running"
    /\ PendingTurns(s) /= {}

Clean(s) ==
    /\ objects[s] = {}
    /\ pod[s] = 0
    /\ journal[s] = {}
    /\ \A k \in Keys : k[1] = s => ledger[k] = "none"

\* ------------------------------------------------------------
(* Initial state *)
\* ------------------------------------------------------------

Init ==
    /\ created = [s \in Sessions |-> FALSE]
    /\ registered = [s \in Sessions |-> FALSE]
    /\ seeded = [s \in Sessions |-> FALSE]
    /\ deleted = [s \in Sessions |-> FALSE]
    /\ status = [s \in Sessions |-> "none"]
    /\ final = [s \in Sessions |-> "none"]
    /\ closeReason = [s \in Sessions |-> "none"]
    /\ objects = [s \in Sessions |-> {}]
    /\ pod = [s \in Sessions |-> 0]
    /\ restarts = [s \in Sessions |-> 0]
    /\ podKills = [s \in Sessions |-> 0]
    /\ cpRestarts = 0
    /\ instCounter = 0
    /\ hubSeq = [s \in Sessions |-> 0]
    /\ seqFloor = [s \in Sessions |-> 0]
    /\ posted = [s \in Sessions |-> {}]
    /\ pending = [s \in Sessions |-> {}]
    /\ accepted = [s \in Sessions |-> {}]
    /\ started = [k \in Sessions \X Turns |-> 0]
    /\ issued = [k \in Sessions \X Turns |-> 0]
    /\ journal = [s \in Sessions |-> {}]
    /\ convSize = [s \in Sessions |-> 0]
    /\ entryLen = [k \in Keys |-> 0]
    /\ applies = [k \in Keys |-> 0]
    /\ ledger = [k \in Keys |-> "none"]
    /\ ledgerInst = [k \in Keys |-> 0]
    /\ doneExec = [k \in Keys |-> 0]
    /\ callState = [k \in Keys |-> "idle"]
    /\ execCount = [k \in Keys |-> 0]

\* ------------------------------------------------------------
(* Session lifecycle *)
\* ------------------------------------------------------------

\* Create: session.Manager.Create. The control plane writes the record, the
\* Secret, ConfigMap, NetworkPolicy, PVC, and the pod, and registers the
\* session and its grant.
Create(s) ==
    /\ ~created[s]
    /\ created' = [created EXCEPT ![s] = TRUE]
    /\ registered' = [registered EXCEPT ![s] = TRUE]
    /\ seeded' = [seeded EXCEPT ![s] = TRUE]
    /\ status' = [status EXCEPT ![s] = "pending"]
    /\ objects' = [objects EXCEPT ![s] = Resources]
    /\ pod' = [pod EXCEPT ![s] = instCounter + 1]
    /\ instCounter' = instCounter + 1
    \* The initial prompt is the first accepted message and turn key. Create
    \* writes its pending record before it returns, exactly as PostMessage
    \* does for a later message.
    /\ hubSeq' = [hubSeq EXCEPT ![s] = 1]
    /\ seqFloor' = [seqFloor EXCEPT ![s] = Block]
    /\ posted' = [posted EXCEPT ![s] = {1}]
    /\ accepted' = [accepted EXCEPT ![s] = {1}]
    /\ pending' = [pending EXCEPT ![s] = IF DurableInbox THEN {1} ELSE {}]
    /\ started' = [started EXCEPT ![<<s, 1>>] = 1]
    /\ UNCHANGED <<deleted, final, closeReason, restarts, podKills, cpRestarts,
                   issued, journal, convSize,
                   entryLen, applies, ledger, ledgerInst, doneExec, callState,
                   execCount>>

\* PostMessage: session.PostMessage and session.reserveSequence. The journal
\* reserves the block that contains the next sequence before the hub hands it
\* out, so every turn key is below the durable floor.
PostMessage(s) ==
    LET next == hubSeq[s] + 1 IN
    LET floor2 == MaxNat(seqFloor[s], next + Block - 1) IN
    /\ created[s]
    /\ final[s] = "none"
    /\ registered[s]
    /\ seeded[s]
    /\ Cardinality(accepted[s]) < MaxTurns
    /\ next <= MaxSeq
    /\ seqFloor' = [seqFloor EXCEPT ![s] = floor2]
    /\ hubSeq' = [hubSeq EXCEPT ![s] = next]
    /\ posted' = [posted EXCEPT ![s] = @ \cup {next}]
    /\ accepted' = [accepted EXCEPT ![s] = @ \cup {next}]
    /\ pending' = [pending EXCEPT ![s] = IF DurableInbox THEN @ \cup {next} ELSE @]
    /\ started' = [started EXCEPT ![<<s, next>>] = 1]
    /\ issued' = [issued EXCEPT ![<<s, next>>] = @ + 1]
    /\ UNCHANGED <<created, registered, seeded, deleted, status, final,
                   closeReason, objects, pod, restarts, podKills, cpRestarts,
                   instCounter, journal, convSize, entryLen, applies, ledger,
                   ledgerInst, doneExec, callState, execCount>>

\* Close: POST close, idle timeout, or TTL. The status becomes closing and the
\* runtime finalizes or the control plane exports a fallback.
Close(s) ==
    /\ status[s] \in {"pending", "running"}
    /\ final[s] = "none"
    /\ status' = [status EXCEPT ![s] = "closing"]
    /\ closeReason' = [closeReason EXCEPT ![s] = "closed"]
    /\ UNCHANGED <<created, registered, seeded, deleted, final, objects, pod,
                   restarts, podKills, cpRestarts, instCounter, hubSeq,
                   seqFloor, posted, issued, journal, convSize, entryLen,
                   applies, ledger, ledgerInst, doneExec, callState, execCount, pending, accepted, started>>

\* TTLExpire: session.reap closes a session whose TTL elapsed with reason
\* expired.
TTLExpire(s) ==
    /\ status[s] \in {"pending", "running"}
    /\ final[s] = "none"
    /\ status' = [status EXCEPT ![s] = "closing"]
    /\ closeReason' = [closeReason EXCEPT ![s] = "expired"]
    /\ UNCHANGED <<created, registered, seeded, deleted, final, objects, pod,
                   restarts, podKills, cpRestarts, instCounter, hubSeq,
                   seqFloor, posted, issued, journal, convSize, entryLen,
                   applies, ledger, ledgerInst, doneExec, callState, execCount, pending, accepted, started>>

\* UserDelete: session.Manager.Delete. The session becomes failed with reason
\* "deleted", the tombstone records the deletion, and the live registration is
\* dropped. Cleanup removes the rest. The model treats the tombstone as
\* durable across a control-plane restart; see docs/formal-verification.md.
UserDelete(s) ==
    /\ created[s]
    /\ final[s] = "none"
    /\ final' = [final EXCEPT ![s] = "failed"]
    /\ deleted' = [deleted EXCEPT ![s] = TRUE]
    /\ registered' = [registered EXCEPT ![s] = FALSE]
    /\ UNCHANGED <<created, seeded, status, closeReason, objects, pod,
                   restarts, podKills, cpRestarts, instCounter, hubSeq,
                   seqFloor, posted, issued, journal, convSize, entryLen,
                   applies, ledger, ledgerInst, doneExec, callState, execCount, pending, accepted, started>>

\* RuntimeFinalize: the runtime uploads its bundle and reports the session
\* complete, or reports the TTL elapsed. A completed report becomes expired
\* when the close reason was expired (session.Manager.finalize).
RuntimeFinalize(s) ==
    /\ final[s] = "none"
    /\ pod[s] /= 0
    /\ registered[s]
    /\ status[s] \in {"running", "closing"}
    /\ convSize[s] = Cardinality(journal[s])
    /\ (status[s] = "closing" \/ PendingTurns(s) = {})
    /\ \E outcome \in {"completed", "expired"} :
         final' = [final EXCEPT ![s] =
             IF outcome = "completed" /\ closeReason[s] = "expired"
             THEN "expired"
             ELSE outcome]
    /\ UNCHANGED <<created, registered, seeded, deleted, status, closeReason,
                   objects, pod, restarts, podKills, cpRestarts, instCounter,
                   hubSeq, seqFloor, posted, issued, journal, convSize,
                   entryLen, applies, ledger, ledgerInst, doneExec, callState,
                   execCount, pending, accepted, started>>

\* FallbackExport: session.Manager.fallback. A closing session past its grace,
\* a pending session past its connect timeout, or a session with no pod and no
\* replacement attempts left is exported and ends; expired only when the close
\* reason was expired.
FallbackExport(s) ==
    /\ final[s] = "none"
    /\ \/ status[s] = "closing"
       \/ status[s] = "pending"
       \/ pod[s] = 0 /\ restarts[s] >= MaxReplacements
    /\ final' = [final EXCEPT ![s] =
         IF closeReason[s] = "expired" THEN "expired" ELSE "failed"]
    /\ UNCHANGED <<created, registered, seeded, deleted, status, closeReason,
                   objects, pod, restarts, podKills, cpRestarts, instCounter,
                   hubSeq, seqFloor, posted, issued, journal, convSize,
                   entryLen, applies, ledger, ledgerInst, doneExec, callState,
                   execCount, pending, accepted, started>>

\* Cleanup: session.Manager.cleanup. Deletes the pod, Secret, ConfigMap,
\* NetworkPolicy, and PVC, and deletes the journal, which also removes the
\* ledger.
Cleanup(s) ==
    /\ final[s] /= "none"
    /\ \/ objects[s] /= {}
       \/ pod[s] /= 0
       \/ journal[s] /= {}
       \/ \E k \in Keys : k[1] = s /\ ledger[k] /= "none"
    /\ objects' = [objects EXCEPT ![s] = {}]
    /\ pod' = [pod EXCEPT ![s] = 0]
    /\ journal' = [journal EXCEPT ![s] = {}]
    /\ ledger' = [k \in Keys |-> IF k[1] = s THEN "none" ELSE ledger[k]]
    /\ ledgerInst' = [k \in Keys |-> IF k[1] = s THEN 0 ELSE ledgerInst[k]]
    /\ doneExec' = [k \in Keys |-> IF k[1] = s THEN 0 ELSE doneExec[k]]
    /\ convSize' = [convSize EXCEPT ![s] = 0]
    /\ callState' = [k \in Keys |-> IF k[1] = s THEN "idle" ELSE callState[k]]
    /\ pending' = [pending EXCEPT ![s] = {}]
    /\ UNCHANGED <<created, registered, seeded, deleted, status, final,
                   closeReason, restarts, podKills, cpRestarts, instCounter,
                   hubSeq, seqFloor, posted, accepted, started, issued,
                   entryLen, applies, execCount>>

\* ------------------------------------------------------------
(* Runtime: journal, model, and tools *)
\* ------------------------------------------------------------

\* RuntimeStartTurn: the runtime opens the inbox (or picks the next buffered
\* message) and marks the session running. journal/ledger state is untouched.
RuntimeStartTurn(s) ==
    /\ RuntimeEnabled(s)
    /\ status[s] \in {"pending", "running"}
    /\ PendingTurns(s) /= {}
    /\ status' = [status EXCEPT ![s] = "running"]
    /\ UNCHANGED <<created, registered, seeded, deleted, final, closeReason,
                   objects, pod, restarts, podKills, cpRestarts, instCounter,
                   hubSeq, seqFloor, posted, issued, journal, convSize,
                   entryLen, applies, ledger, ledgerInst, doneExec, callState,
                   execCount, pending, accepted, started>>

\* JournalUserStep: agent.handleTurn step 0. Appends the user entry, which
\* carries the conversation length after the step. The conversation line is
\* applied separately (ApplyStep), as the runtime does.
JournalUserStep(s) ==
    LET t == NextTurn(s) IN
    LET k == 0 IN
    /\ CanWork(s)
    /\ CurrentStep(s, t) = k
    /\ convSize[s] = Cardinality(journal[s])
    /\ journal' = [journal EXCEPT ![s] = @ \cup {<<t, k>>}]
    /\ entryLen' = [entryLen EXCEPT ![<<s, t, k>>] = convSize[s] + 1]
    \* Step 0 makes the turn key durable, so the pending copy is gone.
    /\ pending' = [pending EXCEPT ![s] = @ \ {t}]
    /\ UNCHANGED <<created, registered, seeded, deleted, status, final,
                   closeReason, objects, pod, restarts, podKills, cpRestarts,
                   instCounter, hubSeq, seqFloor, posted, accepted, started,
                   issued, convSize, applies, ledger, ledgerInst, doneExec,
                   callState, execCount>>

\* ModelCall: the runtime executes a model step. A model call has no external
\* side effect that survives the process, so a crash may repeat it.
ModelCall(s) ==
    LET t == NextTurn(s) IN
    LET k == CurrentStep(s, t) IN
    LET key == <<s, t, k>> IN
    /\ CanWork(s)
    /\ k \in {1, 4}
    /\ convSize[s] = Cardinality(journal[s])
    /\ callState[key] = "idle"
    /\ callState' = [callState EXCEPT ![key] = "executed"]
    /\ UNCHANGED <<created, registered, seeded, deleted, status, final,
                   closeReason, objects, pod, restarts, podKills, cpRestarts,
                   instCounter, hubSeq, seqFloor, posted, issued, journal,
                   convSize, entryLen, applies, ledger, ledgerInst, doneExec,
                   execCount, pending, accepted, started>>

\* JournalModelStep: the assistant message is published only from the journal
\* entry, so the entry is appended before the conversation line.
JournalModelStep(s) ==
    LET t == NextTurn(s) IN
    LET k == CurrentStep(s, t) IN
    LET key == <<s, t, k>> IN
    /\ CanWork(s)
    /\ k \in {1, 4}
    /\ convSize[s] = Cardinality(journal[s])
    /\ callState[key] = "executed"
    /\ journal' = [journal EXCEPT ![s] = @ \cup {<<t, k>>}]
    /\ entryLen' = [entryLen EXCEPT ![key] = convSize[s] + 1]
    /\ callState' = [callState EXCEPT ![key] = "idle"]
    /\ UNCHANGED <<created, registered, seeded, deleted, status, final,
                   closeReason, objects, pod, restarts, podKills, cpRestarts,
                   instCounter, hubSeq, seqFloor, posted, issued, convSize,
                   applies, ledger, ledgerInst, doneExec, execCount, pending, accepted, started>>

\* ApplyStep: apply a journaled step to the conversation, but only while the
\* file is shorter than the ConvLen the entry recorded. A step that is
\* already applied cannot be applied again, which is what the runtime's size
\* check enforces.
ApplyStep(s) ==
    /\ RuntimeEnabled(s)
    /\ \E t \in posted[s] :
         \E k \in Steps :
           LET key == <<s, t, k>> IN
           /\ <<t, k>> \in journal[s]
           /\ convSize[s] = entryLen[key] - 1
           /\ applies[key] < 2
           /\ convSize' = [convSize EXCEPT ![s] = entryLen[key]]
           /\ applies' = [applies EXCEPT ![key] = @ + 1]
    /\ UNCHANGED <<created, registered, seeded, deleted, status, final,
                   closeReason, objects, pod, restarts, podKills, cpRestarts,
                   instCounter, hubSeq, seqFloor, posted, issued, journal,
                   entryLen, ledger, ledgerInst, doneExec, callState, execCount, pending, accepted, started>>

\* ToolBegin: gateway.Begin plus beginFlight. The resume table:
\*   done                    -> replay the recorded result, execute nothing
\*   pending, other instance,
\*   non-idempotent          -> interrupt: record the interrupted error result
\*   otherwise               -> record pending (LedgerBeforeDispatch) and
\*                              dispatch, or dispatch with the pending record
\*                              of an idempotent call
ToolBegin(s) ==
    LET t == NextTurn(s) IN
    LET k == CurrentStep(s, t) IN
    LET key == <<s, t, k>> IN
    LET tool == ToolAt(k) IN
    /\ CanWork(s)
    /\ k \in ToolSteps
    /\ convSize[s] = Cardinality(journal[s])
    /\ callState[key] = "idle"
    /\ \/ /\ ledger[key] = "done"
          /\ callState' = [callState EXCEPT ![key] = "replayed"]
          /\ UNCHANGED <<ledger, ledgerInst, doneExec, execCount>>
       \/ /\ ledger[key] = "pending"
          /\ ~Idem(tool)
          /\ (ledgerInst[key] = 0 \/ ledgerInst[key] /= pod[s])
          /\ InterruptOnResume
          /\ ledger' = [ledger EXCEPT ![key] = "done"]
          /\ callState' = [callState EXCEPT ![key] = "interrupted"]
          /\ doneExec' = [doneExec EXCEPT ![key] = execCount[key]]
          /\ UNCHANGED <<ledgerInst, execCount>>
       \/ /\ ledger[key] = "none"
          /\ LedgerBeforeDispatch
          /\ ledger' = [ledger EXCEPT ![key] = "pending"]
          /\ ledgerInst' = [ledgerInst EXCEPT ![key] = pod[s]]
          /\ callState' = [callState EXCEPT ![key] = "dispatched"]
          /\ UNCHANGED <<doneExec, execCount>>
       \/ /\ ledger[key] = "none"
          /\ ~LedgerBeforeDispatch
          /\ callState' = [callState EXCEPT ![key] = "dispatched"]
          /\ UNCHANGED <<ledger, ledgerInst, doneExec, execCount>>
       \/ /\ ledger[key] = "pending"
          /\ Idem(tool)
          /\ callState' = [callState EXCEPT ![key] = "dispatched"]
          /\ UNCHANGED <<ledger, ledgerInst, doneExec, execCount>>
       \/ /\ ledger[key] = "pending"
          /\ ~Idem(tool)
          /\ ~InterruptOnResume
          /\ callState' = [callState EXCEPT ![key] = "dispatched"]
          /\ UNCHANGED <<ledger, ledgerInst, doneExec, execCount>>
    /\ UNCHANGED <<created, registered, seeded, deleted, status, final,
                   closeReason, objects, pod, restarts, podKills, cpRestarts,
                   instCounter, hubSeq, seqFloor, posted, issued, journal,
                   convSize, entryLen, applies, pending, accepted, started>>

\* ToolExecute: the external side effect. execCount is the number of times the
\* tool body ran for this call key. The bound 2 keeps the state space finite;
\* the invariant requires at most 1 for a non-idempotent call.
ToolExecute(s) ==
    LET t == NextTurn(s) IN
    LET k == CurrentStep(s, t) IN
    LET key == <<s, t, k>> IN
    /\ CanWork(s)
    /\ k \in ToolSteps
    /\ callState[key] = "dispatched"
    /\ execCount[key] < 2
    /\ execCount' = [execCount EXCEPT ![key] = @ + 1]
    /\ callState' = [callState EXCEPT ![key] = "executed"]
    /\ UNCHANGED <<created, registered, seeded, deleted, status, final,
                   closeReason, objects, pod, restarts, podKills, cpRestarts,
                   instCounter, hubSeq, seqFloor, posted, issued, journal,
                   convSize, entryLen, applies, ledger, ledgerInst, doneExec, pending, accepted, started>>

\* ToolComplete: gateway.Complete. Records the result as done once. With
\* LedgerBeforeDispatch false the record is written here instead of at begin,
\* which is the negative check's fault.
ToolComplete(s) ==
    LET t == NextTurn(s) IN
    LET k == CurrentStep(s, t) IN
    LET key == <<s, t, k>> IN
    /\ CanWork(s)
    /\ k \in ToolSteps
    /\ callState[key] = "executed"
    /\ \/ ledger[key] = "pending"
       \/ ledger[key] = "none" /\ ~LedgerBeforeDispatch
    /\ ledger' = [ledger EXCEPT ![key] = "done"]
    /\ doneExec' = [doneExec EXCEPT ![key] = execCount[key]]
    /\ UNCHANGED <<created, registered, seeded, deleted, status, final,
                   closeReason, objects, pod, restarts, podKills, cpRestarts,
                   instCounter, hubSeq, seqFloor, posted, issued, journal,
                   convSize, entryLen, applies, ledgerInst, callState,
                   execCount, pending, accepted, started>>

\* JournalToolStep: the runtime reports the call result, which is journaled.
\* A replayed or interrupted result is journaled without an execution.
JournalToolStep(s) ==
    LET t == NextTurn(s) IN
    LET k == CurrentStep(s, t) IN
    LET key == <<s, t, k>> IN
    /\ CanWork(s)
    /\ k \in ToolSteps
    /\ convSize[s] = Cardinality(journal[s])
    /\ callState[key] \in {"executed", "replayed", "interrupted"}
    /\ journal' = [journal EXCEPT ![s] = @ \cup {<<t, k>>}]
    /\ entryLen' = [entryLen EXCEPT ![key] = convSize[s] + 1]
    /\ callState' = [callState EXCEPT ![key] = "idle"]
    /\ UNCHANGED <<created, registered, seeded, deleted, status, final,
                   closeReason, objects, pod, restarts, podKills, cpRestarts,
                   instCounter, hubSeq, seqFloor, posted, issued, convSize,
                   applies, ledger, ledgerInst, doneExec, execCount, pending, accepted, started>>

\* ------------------------------------------------------------
(* Faults and recovery *)
\* ------------------------------------------------------------

\* A killed pod loses the runtime's in-memory turn progress and call state.
\* The journal, ledger, sequence floor, conversation file, and record survive.
PodForceDelete(s) ==
    /\ pod[s] /= 0
    /\ final[s] = "none"
    /\ podKills[s] < MaxPodKills
    /\ pod' = [pod EXCEPT ![s] = 0]
    /\ podKills' = [podKills EXCEPT ![s] = @ + 1]
    /\ callState' = [k \in Keys |-> IF k[1] = s THEN "idle" ELSE callState[k]]
    /\ UNCHANGED <<created, registered, seeded, deleted, status, final,
                   closeReason, objects, restarts, cpRestarts, instCounter,
                   hubSeq, seqFloor, posted, issued, journal, convSize,
                   entryLen, applies, ledger, ledgerInst, doneExec, execCount, pending, accepted, started>>

\* A drain, eviction, or preemption is also a pod death; the runtime exits
\* without finalizing so a replacement resumes.
PodGracefulDelete(s) ==
    /\ pod[s] /= 0
    /\ final[s] = "none"
    /\ podKills[s] < MaxPodKills
    /\ pod' = [pod EXCEPT ![s] = 0]
    /\ podKills' = [podKills EXCEPT ![s] = @ + 1]
    /\ callState' = [k \in Keys |-> IF k[1] = s THEN "idle" ELSE callState[k]]
    /\ UNCHANGED <<created, registered, seeded, deleted, status, final,
                   closeReason, objects, restarts, cpRestarts, instCounter,
                   hubSeq, seqFloor, posted, issued, journal, convSize,
                   entryLen, applies, ledger, ledgerInst, doneExec, execCount, pending, accepted, started>>

\* ControlPlaneRestart: the process loses the hub, in-flight begins, registered
\* instances, and seeded hubs. Journal, ledger, sequence floor, status,
\* records, tombstones, and pods survive.
ControlPlaneRestart ==
    /\ cpRestarts < MaxRestarts
    /\ cpRestarts' = cpRestarts + 1
    /\ registered' = [s \in Sessions |-> FALSE]
    /\ seeded' = [s \in Sessions |-> FALSE]
    /\ hubSeq' = [s \in Sessions |-> 0]
    \* The in-memory inbox is gone. The durable pending record is not.
    /\ posted' = [s \in Sessions |-> {}]
    /\ UNCHANGED <<created, deleted, status, final, closeReason, objects, pod,
                   restarts, podKills, instCounter, seqFloor, pending,
                   accepted, started, issued, journal, convSize, entryLen,
                   applies, ledger, ledgerInst, doneExec, callState,
                   execCount>>

\* Adopt: session.Manager.adoptPods/adoptPodless plus seedSequence. A live
\* session is registered again and its hub is seeded above the durable floor,
\* so a new message cannot reuse a turn key. A deleted session is not
\* registered. A finalized session is left to cleanup.
Adopt(s) ==
    /\ created[s]
    /\ ~registered[s]
    /\ ~deleted[s]
    /\ final[s] = "none"
    /\ objects[s] /= {}
    /\ registered' = [registered EXCEPT ![s] = TRUE]
    /\ seeded' = [seeded EXCEPT ![s] = TRUE]
    /\ hubSeq' = [hubSeq EXCEPT ![s] = MaxNat(seqFloor[s], MaxJournalTurn(s))]
    \* Redeliver the durable pending messages under the keys they were
    \* accepted with. RedeliverNewKey models adoption that reissues a new turn
    \* key instead of keeping the reserved one.
    /\ posted' = [posted EXCEPT ![s] = @ \cup pending[s]]
    /\ started' = LET bumped ==
            [k \in Sessions \X Turns |->
                IF k[1] = s /\ k[2] \in pending[s] /\ RedeliverNewKey
                THEN 2
                ELSE started[k]]
        IN bumped
    /\ UNCHANGED <<created, deleted, status, final, closeReason, objects, pod,
                   restarts, podKills, cpRestarts, instCounter, seqFloor,
                   pending, accepted, issued, journal, convSize, entryLen,
                   applies, ledger, ledgerInst, doneExec, callState, execCount>>

\* ReplacementPodResume: session.restart. A replacement pod reuses the Secret,
\* ConfigMap, NetworkPolicy, and PVC, gets a new runtime instance, and resumes
\* the journaled turn.
ReplacementPodResume(s) ==
    /\ created[s]
    /\ registered[s]
    /\ seeded[s]
    /\ final[s] = "none"
    /\ pod[s] = 0
    /\ status[s] \in {"pending", "running"}
    /\ restarts[s] < MaxReplacements
    /\ objects[s] /= {}
    /\ restarts' = [restarts EXCEPT ![s] = @ + 1]
    /\ pod' = [pod EXCEPT ![s] = instCounter + 1]
    /\ instCounter' = instCounter + 1
    /\ UNCHANGED <<created, registered, seeded, deleted, status, final,
                   closeReason, objects, podKills, cpRestarts, hubSeq,
                   seqFloor, posted, issued, journal, convSize, entryLen,
                   applies, ledger, ledgerInst, doneExec, callState, execCount, pending, accepted, started>>

\* ------------------------------------------------------------
(* Next and fairness *)
\* ------------------------------------------------------------


\* A deadlock is not an error state in this model: every session ends in a
\* terminal status with its objects deleted, and the maintenance loop has no
\* work left. The stuttering step makes that terminal behavior explicit, so
\* TLC checks liveness over infinite behaviors instead of stopping at the
\* deadlock. Every non-terminal state has a fair action enabled, so a
\* stutter cannot postpone progress forever.
Stutter == UNCHANGED vars

Next ==
    \/ \E s \in Sessions :
         \/ Create(s)
         \/ PostMessage(s)
         \/ Close(s)
         \/ TTLExpire(s)
         \/ UserDelete(s)
         \/ RuntimeFinalize(s)
         \/ FallbackExport(s)
         \/ Cleanup(s)
         \/ RuntimeStartTurn(s)
         \/ JournalUserStep(s)
         \/ ModelCall(s)
         \/ JournalModelStep(s)
         \/ ApplyStep(s)
         \/ ToolBegin(s)
         \/ ToolExecute(s)
         \/ ToolComplete(s)
         \/ JournalToolStep(s)
         \/ PodForceDelete(s)
         \/ PodGracefulDelete(s)
         \/ Adopt(s)
         \/ ReplacementPodResume(s)
    \/ ControlPlaneRestart
    \/ Stutter

\* Fairness is the control plane's maintenance loop and the runtime's own
\* progress. User actions (PostMessage, Close, UserDelete) and the bounded
\* faults get no fairness, so liveness does not depend on them.
Fairness ==
    /\ \A s \in Sessions :
         /\ WF_vars(RuntimeStartTurn(s))
         /\ WF_vars(JournalUserStep(s))
         /\ WF_vars(ModelCall(s))
         /\ WF_vars(JournalModelStep(s))
         /\ WF_vars(ApplyStep(s))
         /\ WF_vars(ToolBegin(s))
         /\ WF_vars(ToolExecute(s))
         /\ WF_vars(ToolComplete(s))
         /\ WF_vars(JournalToolStep(s))
         /\ WF_vars(Adopt(s))
         /\ WF_vars(ReplacementPodResume(s))
         /\ WF_vars(TTLExpire(s))
         /\ WF_vars(RuntimeFinalize(s))
         /\ WF_vars(FallbackExport(s))
         /\ WF_vars(Cleanup(s))

Spec == Init /\ [][Next]_vars /\ Fairness

\* ------------------------------------------------------------
(* Safety *)
\* ------------------------------------------------------------

TypeOK ==
    /\ created \in [Sessions -> BOOLEAN]
    /\ registered \in [Sessions -> BOOLEAN]
    /\ seeded \in [Sessions -> BOOLEAN]
    /\ deleted \in [Sessions -> BOOLEAN]
    /\ status \in [Sessions -> {"none", "pending", "running", "closing"}]
    /\ final \in [Sessions -> {"none", "completed", "expired", "failed"}]
    /\ closeReason \in [Sessions -> {"none", "closed", "expired"}]
    /\ objects \in [Sessions -> SUBSET Resources]
    /\ pod \in [Sessions -> Instances]
    /\ restarts \in [Sessions -> 0..MaxReplacements]
    /\ podKills \in [Sessions -> 0..MaxPodKills]
    /\ cpRestarts \in 0..MaxRestarts
    /\ instCounter \in 0..(MaxInstances + 1)
    /\ hubSeq \in [Sessions -> 0..MaxSeq]
    /\ seqFloor \in [Sessions -> 0..(MaxSeq + Block)]
    /\ posted \in [Sessions -> SUBSET Turns]
    /\ pending \in [Sessions -> SUBSET Turns]
    /\ accepted \in [Sessions -> SUBSET Turns]
    /\ started \in [(Sessions \X Turns) -> 0..2]
    /\ issued \in [(Sessions \X Turns) -> 0..2]
    /\ journal \in [Sessions -> SUBSET (Turns \X Steps)]
    /\ convSize \in [Sessions -> 0..(MaxTurns * (LastStep + 1))]
    /\ entryLen \in [Keys -> 0..(MaxTurns * (LastStep + 1))]
    /\ applies \in [Keys -> 0..2]
    /\ ledger \in [Keys -> {"none", "pending", "done"}]
    /\ ledgerInst \in [Keys -> Instances]
    /\ doneExec \in [Keys -> 0..2]
    /\ callState \in [Keys -> {"idle", "dispatched", "executed", "replayed",
                              "interrupted"}]
    /\ execCount \in [Keys -> 0..2]

\* A turn key is handed out at most once, and a journaled step is applied to
\* the conversation at most once, even across a control-plane restart.
UserMessageAtMostOnce ==
    /\ \A s \in Sessions, t \in Turns : issued[<<s, t>>] <= 1
    /\ \A s \in Sessions, t \in Turns : started[<<s, t>>] <= 1
    /\ \A k \in Keys : applies[k] <= 1

\* Every turn key the hub handed out is inside the durable reservation, so the
\* key survives a control-plane restart as a used key.
TurnKeyReserved ==
    \A s \in Sessions :
        /\ \A t \in posted[s] : t <= seqFloor[s]
        /\ \A t \in pending[s] : t <= seqFloor[s]

\* A message the control plane acknowledged is never lost: it is either
\* still durable in the pending inbox, already started in the journal, or its
\* session has ended. This is the property #59 lacked.
AcceptedNotLost ==
    \A s \in Sessions, t \in Turns :
        t \in accepted[s] =>
            t \in pending[s] \/ t \in JournalTurns(s) \/ final[s] /= "none"

\* A non-idempotent tool body runs at most once per call key. After a crash,
\* the resumed runtime gets the interrupted result instead of a second run.
NonIdempotentAtMostOnce ==
    \A s \in Sessions, t \in Turns, k \in ToolSteps :
        ToolAt(k) = ToolNonIdem => execCount[<<s, t, k>>] <= 1

\* Once a ledger record is done, no later begin executes the tool again.
CompletedReplays ==
    \A k \in Keys : ledger[k] = "done" => execCount[k] <= doneExec[k]

\* An idempotent call is always keyed by a turn the control plane accepted, so
\* a re-dispatch after a resume reuses the same <session>/<turn>/<step> key.
\* accepted survives a restart even when the in-memory inbox does not.
IdempotentSameKey ==
    \A s \in Sessions, t \in Turns, k \in ToolSteps :
        ToolAt(k) = ToolIdem /\ callState[<<s, t, k>>] /= "idle"
            => t \in accepted[s]

\* A deleted session is never registered again by adoption.
NoResurrection ==
    \A s \in Sessions : deleted[s] => ~registered[s]

\* The journal of a turn is a prefix: no step k is journaled before step k-1.
JournalPrefix ==
    \A s \in Sessions, t \in Turns, k \in Steps :
        <<t, k>> \in journal[s] => \A j \in 0..k : <<t, j>> \in journal[s]

\* The conversation never runs ahead of the journal and trails it by at most
\* one step (the step journaled but not yet applied).
ConversationBounded ==
    \A s \in Sessions :
        /\ convSize[s] <= Cardinality(journal[s])
        /\ convSize[s] + 1 >= Cardinality(journal[s])

\* ------------------------------------------------------------
(* Liveness *)
\* ------------------------------------------------------------

\* Every session that was created reaches a terminal status.
Termination ==
    \A s \in Sessions : [](created[s] => <>(final[s] /= "none"))

\* Every session that was created eventually has no pod, no Secret, ConfigMap,
\* NetworkPolicy, or PVC, and no journal or ledger left.
Reclamation ==
    \A s \in Sessions : [](created[s] => <>[]Clean(s))


=============================================================================
