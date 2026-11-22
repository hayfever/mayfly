----------------------------- MODULE HubRetention -----------------------------
\* One session's in-memory event stream, as internal/hub retains it.
\*
\* The model covers the parts of the stream that bound memory:
\*   - one event's size is capped at MaxEvent (CapEvents);
\*   - the retained thread keeps at most MaxEvents events and at most Budget
\*     accounted bytes, and drops the oldest event first;
\*   - a human message also enters the inbox, which runtime appends never
\*     evict (ExemptInbox) and the runtime consumes in order;
\*   - the sequence number increases on every append.
\*
\* Sizes are abstract bytes. A runtime append offers a size from Sizes; the
\* positive model truncates it to MaxEvent, which is the behavior of
\* session.Manager.EmitRuntimeEvents. A human message is small and is modeled
\* as one byte because sessions.Manager.PostMessage already bounds it.
\*
\* The flags model implemented guarantees. Set one to FALSE in a *_buggy.cfg
\* to remove that guarantee and show the invariant it protects.
EXTENDS Naturals, Sequences

CONSTANTS MaxEvents, MaxEvent, Budget, MaxAppends, Sizes, CapEvents,
          ExemptInbox

VARIABLES seq, thread, inbox, appends, assigned, runtimeDropped

vars == <<seq, thread, inbox, appends, assigned, runtimeDropped>>

RECURSIVE SumBytes(_)
SumBytes(t) ==
  IF Len(t) = 0 THEN 0 ELSE t[1].bytes + SumBytes(Tail(t))

RECURSIVE SeqSet(_)
SeqSet(s) ==
  IF Len(s) = 0 THEN {} ELSE {Head(s)} \cup SeqSet(Tail(s))

\* DropInbox removes the oldest inbox entry when it is the head of the inbox.
\* Inbox sequences increase, and eviction removes the oldest thread entry, so
\* a match can only be at the head.
DropInbox(inb, s) ==
  IF Len(inb) > 0 /\ Head(inb) = s THEN Tail(inb) ELSE inb

\* Trim drops the oldest thread events until both retention caps hold. The
\* inbox is a separate sequence. With ExemptInbox, trimming the thread never
\* removes an inbox entry; without it, eviction also consumes the matching
\* inbox entry, the bug the InboxNeverEvictedByRuntime invariant rejects.
RECURSIVE Trim(_, _)
Trim(t, inb) ==
  IF Len(t) = 0 THEN <<t, inb>>
  ELSE IF Len(t) <= MaxEvents /\ SumBytes(t) <= Budget THEN <<t, inb>>
  ELSE Trim(Tail(t),
            IF ExemptInbox THEN inb ELSE DropInbox(inb, Head(t).seq))

Init ==
  /\ seq = 0
  /\ thread = <<>>
  /\ inbox = <<>>
  /\ appends = 0
  /\ assigned = <<>>
  /\ runtimeDropped = {}

RuntimeAppend ==
  /\ appends < MaxAppends
  /\ \E b \in Sizes:
       LET new == seq + 1
           stored == IF CapEvents /\ b > MaxEvent THEN MaxEvent ELSE b
           r == Trim(Append(thread, [seq |-> new, bytes |-> stored]), inbox)
       IN /\ seq' = new
          /\ thread' = r[1]
          /\ inbox' = r[2]
          /\ appends' = appends + 1
          /\ assigned' = Append(assigned, new)
          /\ runtimeDropped' = runtimeDropped \cup
               (SeqSet(inbox) \ SeqSet(r[2]))

HumanPost ==
  /\ appends < MaxAppends
  /\ LET new == seq + 1
         r == Trim(Append(thread, [seq |-> new, bytes |-> 1]), inbox)
     IN /\ seq' = new
        /\ thread' = r[1]
        /\ inbox' = Append(r[2], new)
        /\ appends' = appends + 1
        /\ assigned' = Append(assigned, new)
        /\ UNCHANGED runtimeDropped

\* The runtime reads the oldest unconsumed human message. Consumption removes
\* only the inbox copy; the thread copy stays until retention drops it.
Consume ==
  /\ Len(inbox) > 0
  /\ inbox' = Tail(inbox)
  /\ UNCHANGED <<seq, thread, appends, assigned, runtimeDropped>>

Stutter == UNCHANGED vars

Next == RuntimeAppend \/ HumanPost \/ Consume \/ Stutter

Spec == Init /\ [][Next]_vars

TypeOK ==
  /\ seq \in Nat
  /\ thread \in Seq([seq : Nat, bytes : Nat])
  /\ inbox \in Seq(Nat)
  /\ appends \in Nat
  /\ assigned \in Seq(Nat)
  /\ runtimeDropped \subseteq Nat

\* Every retained thread event fits the per-event cap.
EventCapEnforced ==
  \A i \in DOMAIN thread: thread[i].bytes <= MaxEvent

\* Retained thread bytes, including the human messages still on the thread,
\* fit the per-session budget. The inbox has its own count cap in Go and is
\* not part of this budget.
RetainedWithinBudget == SumBytes(thread) <= Budget

\* The retained thread is a contiguous suffix of the assigned sequence, and
\* it ends at the latest sequence. An eviction opens a visible gap, never a
\* hole in the middle.
ContiguousSuffix ==
  Len(thread) = 0 \/
    (thread[Len(thread)].seq = seq /\
     \A i \in DOMAIN thread: thread[i].seq = seq - Len(thread) + i)

\* A runtime append never removes an unconsumed human message from the inbox.
InboxNeverEvictedByRuntime == runtimeDropped = {}

\* Assigned sequence numbers strictly increase.
MonotonicSeq ==
  \A i, j \in DOMAIN assigned: i < j => assigned[i] < assigned[j]

=============================================================================
