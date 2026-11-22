----------------------------- MODULE HubRetention -----------------------------
\* One session's in-memory event stream, as internal/hub retains it.
\*
\* The model covers the parts of the stream that bound memory and order:
\*   - one event's size is capped at MaxEvent (CapEvents);
\*   - the retained thread keeps at most MaxEvents events and at most Budget
\*     accounted bytes, and drops the oldest event first;
\*   - a human message also enters the inbox, which runtime appends never
\*     evict (ExemptInbox), which keeps at most MaxInbox entries (CapInbox),
\*     and whose oldest message the runtime consumes;
\*   - a dedup key is remembered only while its event stays retained
\*     (EvictKeys). A retry of a remembered key changes nothing; a retry after
\*     eviction can append again;
\*   - the sequence number increases on every append.
\*
\* Sizes are abstract bytes. A runtime append offers a size from Sizes; the
\* positive model truncates it to MaxEvent, which is the behavior of
\* session.Manager.EmitRuntimeEvents. A restored append offers the same sizes
\* and applies the same cap, which is the behavior of
\* session.Manager.restoreJournalThread. A human message is small and is
\* modeled as one byte because sessions.Manager.PostMessage bounds it.
\*
\* Keys is a finite set of abstract step keys. NoKey marks an event without a
\* dedup key.
\*
\* The flags model implemented guarantees. Set one to FALSE in a *_buggy.cfg
\* to remove that guarantee and show the invariant it protects.
EXTENDS Naturals, Sequences

CONSTANTS MaxEvents, MaxEvent, Budget, MaxAppends, Sizes, MaxInbox, Keys,
          CapEvents, ExemptInbox, CapInbox, EvictKeys

NoKey == "no-key"

VARIABLES seq, thread, inbox, appends, assigned, runtimeDropped, keys

vars == <<seq, thread, inbox, appends, assigned, runtimeDropped, keys>>

RECURSIVE SumBytes(_)
SumBytes(t) ==
  IF Len(t) = 0 THEN 0 ELSE t[1].bytes + SumBytes(Tail(t))

RECURSIVE SeqSet(_)
SeqSet(s) ==
  IF Len(s) = 0 THEN {} ELSE {Head(s)} \cup SeqSet(Tail(s))

RECURSIVE KeySet(_)
KeySet(t) ==
  IF Len(t) = 0 THEN {}
  ELSE IF Head(t).key = NoKey THEN KeySet(Tail(t))
  ELSE {Head(t).key} \cup KeySet(Tail(t))

\* KeepLast keeps the newest n entries, which is how the inbox count cap
\* drops the oldest entries first.
RECURSIVE KeepLast(_, _)
KeepLast(s, n) ==
  IF Len(s) <= n THEN s ELSE KeepLast(Tail(s), n)

\* DropInbox removes the oldest inbox entry when it is the head of the inbox.
\* Inbox sequences increase, and eviction removes the oldest thread entry, so
\* a match can only be at the head.
DropInbox(inb, s) ==
  IF Len(inb) > 0 /\ Head(inb) = s THEN Tail(inb) ELSE inb

\* Trim drops the oldest thread events until both retention caps hold. The
\* inbox is a separate sequence. With ExemptInbox, trimming the thread never
\* removes an inbox entry; without it, eviction also consumes the matching
\* inbox entry, the bug the InboxNeverEvictedByRuntime invariant rejects.
\* EvictKeys removes the key of every evicted event, so the key table never
\* outlives an event.
RECURSIVE Trim(_, _, _)
Trim(t, inb, ks) ==
  IF Len(t) = 0 THEN <<t, inb, ks>>
  ELSE IF Len(t) <= MaxEvents /\ SumBytes(t) <= Budget THEN <<t, inb, ks>>
  ELSE LET h == Head(t)
           ks2 == IF EvictKeys /\ h.key # NoKey THEN ks \ {h.key} ELSE ks
       IN Trim(Tail(t),
               IF ExemptInbox THEN inb ELSE DropInbox(inb, h.seq),
               ks2)

Init ==
  /\ seq = 0
  /\ thread = <<>>
  /\ inbox = <<>>
  /\ appends = 0
  /\ assigned = <<>>
  /\ runtimeDropped = {}
  /\ keys = {}

RuntimeAppend ==
  /\ appends < MaxAppends
  /\ \E b \in Sizes, k \in Keys \cup {NoKey}:
       (k = NoKey \/ k \notin keys) /\
       LET new == seq + 1
           stored == IF CapEvents /\ b > MaxEvent THEN MaxEvent ELSE b
           withKey == IF k = NoKey THEN keys ELSE keys \cup {k}
           r == Trim(Append(thread, [seq |-> new, bytes |-> stored, key |-> k]),
                     inbox, withKey)
       IN /\ seq' = new
          /\ thread' = r[1]
          /\ inbox' = r[2]
          /\ keys' = r[3]
          /\ appends' = appends + 1
          /\ assigned' = Append(assigned, new)
          /\ runtimeDropped' = runtimeDropped \cup
               (SeqSet(inbox) \ SeqSet(r[2]))

\* RestoreAppend models hub.Restore as session.Manager.restoreJournalThread
\* calls it: the event carries the step key the journal publication uses, the
\* per-event cap applies first, and the event never enters the inbox.
RestoreAppend ==
  /\ appends < MaxAppends
  /\ \E b \in Sizes, k \in Keys:
       k \notin keys /\
       LET new == seq + 1
           stored == IF CapEvents /\ b > MaxEvent THEN MaxEvent ELSE b
           r == Trim(Append(thread, [seq |-> new, bytes |-> stored, key |-> k]),
                     inbox, keys \cup {k})
       IN /\ seq' = new
          /\ thread' = r[1]
          /\ inbox' = r[2]
          /\ keys' = r[3]
          /\ appends' = appends + 1
          /\ assigned' = Append(assigned, new)
          /\ UNCHANGED runtimeDropped

HumanPost ==
  /\ appends < MaxAppends
  /\ LET new == seq + 1
         r == Trim(Append(thread, [seq |-> new, bytes |-> 1, key |-> NoKey]),
                   inbox, keys)
         posted == Append(r[2], new)
     IN /\ seq' = new
        /\ thread' = r[1]
        /\ inbox' = IF CapInbox THEN KeepLast(posted, MaxInbox) ELSE posted
        /\ keys' = r[3]
        /\ appends' = appends + 1
        /\ assigned' = Append(assigned, new)
        /\ UNCHANGED runtimeDropped

\* A retry of a remembered key changes no stream state. It consumes an append
\* step so it cannot repeat forever. A retry whose event was evicted is
\* RuntimeAppend or RestoreAppend again.
DedupRetry ==
  /\ appends < MaxAppends
  /\ keys # {}
  /\ appends' = appends + 1
  /\ UNCHANGED <<seq, thread, inbox, assigned, runtimeDropped, keys>>

\* The runtime reads the oldest unconsumed human message. Consumption removes
\* only the inbox copy; the thread copy stays until retention drops it.
Consume ==
  /\ Len(inbox) > 0
  /\ inbox' = Tail(inbox)
  /\ UNCHANGED <<seq, thread, appends, assigned, runtimeDropped, keys>>

Stutter == UNCHANGED vars

Next == RuntimeAppend \/ RestoreAppend \/ HumanPost \/ DedupRetry \/ Consume
        \/ Stutter

Spec == Init /\ [][Next]_vars

TypeOK ==
  /\ seq \in Nat
  /\ thread \in Seq([seq : Nat, bytes : Nat, key : Keys \cup {NoKey}])
  /\ inbox \in Seq(Nat)
  /\ appends \in Nat
  /\ assigned \in Seq(Nat)
  /\ runtimeDropped \subseteq Nat
  /\ keys \subseteq Keys

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

\* The inbox count cap holds: a human post beyond MaxInbox drops the oldest
\* entry.
InboxWithinCap == Len(inbox) <= MaxInbox

\* A remembered dedup key always belongs to a retained event, so eviction
\* frees the key.
KeysMatchRetained == keys \subseteq KeySet(thread)

\* Two retained events never share a dedup key: a retry of a remembered key
\* is rejected.
NoDuplicateRetainedKey ==
  \A i, j \in DOMAIN thread:
    i < j => thread[i].key = NoKey \/ thread[i].key # thread[j].key

\* Assigned sequence numbers strictly increase.
MonotonicSeq ==
  \A i, j \in DOMAIN assigned: i < j => assigned[i] < assigned[j]

=============================================================================
