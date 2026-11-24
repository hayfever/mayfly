----------------------------- MODULE CronTimezone -----------------------------
\* A schedule whose cron fields are read in one civil time zone, on a day with
\* a daylight-saving transition.
\*
\* The model keeps one scheduled civil minute per civil day, Days days around
\* the transition on TransitionDay. The cron expression is abstracted to the
\* civil minute ScheduledCivil. A slot key is a UTC minute:
\*
\*     key = civil day * 1440 + civil minute + offset
\*
\* with the offset in minutes west of UTC (the value TLC's config grammar can
\* hold as a natural). Other days use offset 0. On the transition day the zone
\* has an offset before and after the transition, so the model can express the
\* Vixie cron rules from docs/sources.md:
\*
\*   Spring forward. Civil minutes in [GapStart, GapEnd) do not exist. The
\*   schedule fires once at the transition instant, the first valid local time
\*   after the gap, whose key is GapEnd plus the new offset. With
\*   FireGapAtTransition = FALSE the schedule skips the day, and
\*   GapFiresAtTransition fails.
\*
\*   Fall back. Civil minutes in [AmbStart, AmbEnd) exist twice. The schedule
\*   fires once, at the first occurrence, whose key is ScheduledCivil plus the
\*   pre-overlap offset. With DoubleFallback = TRUE it fires at both
\*   occurrences, and FallbackFiresOnce fails. Two firings are a fault here,
\*   not the specified behavior.
\*
\* An expression whose hour field selects every hour is outside this model. It
\* runs on real elapsed time, so a repeated hour fires twice and a skipped hour
\* does not fire.
\*
\* The model abstracts the cron grammar, the number of firings per day, and
\* the scheduler loop. It checks which UTC instants a day's civil time
\* produces, which is where the transition rules live. See
\* docs/formal-verification.md.
EXTENDS Naturals

CONSTANTS Mode,           \* "spring" or "fall"
          ScheduledCivil, \* minute of day the cron expression selects
          GapStart, GapEnd,       \* spring-forward gap, [GapStart, GapEnd)
          AmbStart, AmbEnd,       \* fall-back overlap, [AmbStart, AmbEnd)
          OffsetBefore,           \* minutes west of UTC before the transition
          OffsetAfter,            \* minutes west of UTC after the transition
          Days,                   \* civil days modelled, 0..Days
          FireGapAtTransition,    \* fire once at the spring-forward transition
          DoubleFallback          \* fire both occurrences of a repeated time

TransitionDay == 1

\* A civil minute exists on a day unless a spring-forward gap removes it.
Exists(d, m) ==
  ~(Mode = "spring" /\ d = TransitionDay /\ m >= GapStart /\ m < GapEnd)

\* The offsets that a civil minute uses on a day. A normal day has one.
Offsets(d, m) ==
  IF d # TransitionDay THEN {0}
  ELSE IF Mode = "spring" THEN
    IF m < GapStart THEN {OffsetBefore} ELSE {OffsetAfter}
  ELSE
    IF m < AmbStart THEN {OffsetAfter}
    ELSE IF m < AmbEnd THEN {OffsetAfter, OffsetBefore}
    ELSE {OffsetBefore}

Keys(d, m) == { (d * 1440 + m) + o : o \in Offsets(d, m) }

\* The instants of a day's scheduled civil time when it exists.
ScheduledKeys(d) ==
  IF Exists(d, ScheduledCivil) THEN Keys(d, ScheduledCivil) ELSE {}

\* The first fall-back occurrence uses the pre-overlap offset, OffsetAfter.
FirstKey(d) == (d * 1440 + ScheduledCivil) + OffsetAfter

\* The second fall-back occurrence uses the post-overlap offset.
SecondKey(d) == (d * 1440 + ScheduledCivil) + OffsetBefore

\* A civil time inside the gap fires at the transition: the first valid local
\* time, GapEnd, at the new offset.
TransitionKey(d) == (d * 1440 + GapEnd) + OffsetAfter

Repeated(d) ==
  Mode = "fall" /\ d = TransitionDay
    /\ ScheduledCivil >= AmbStart /\ ScheduledCivil < AmbEnd

\* The keys an implementation emits for a day. The fault switches only change
\* this operator and GapDay.
EmitKeys(d) ==
  IF ~Exists(d, ScheduledCivil) THEN {}
  ELSE IF Repeated(d) THEN
    IF DoubleFallback THEN {FirstKey(d), SecondKey(d)} ELSE {FirstKey(d)}
  ELSE ScheduledKeys(d)

\* The keys a correct implementation may emit, with the switches applied, so
\* ValidSlotKeys catches keys that neither the calendar nor a fault explains.
ExpectedKeys(d) ==
  IF Exists(d, ScheduledCivil) THEN EmitKeys(d)
  ELSE IF Mode = "spring" /\ d = TransitionDay
  THEN (IF FireGapAtTransition THEN {TransitionKey(d)} ELSE {})
  ELSE {}

VARIABLES day, slots

vars == <<day, slots>>

Init ==
  /\ day = 0
  /\ slots = {}

\* The civil time exists: emit its key or keys, then consider the next day.
FireDay ==
  /\ day <= Days
  /\ Exists(day, ScheduledCivil)
  /\ slots' = slots \cup EmitKeys(day)
  /\ day' = day + 1

\* The civil time does not exist. A correct implementation fires once at the
\* transition; the switch models skipping the day instead.
GapDay ==
  /\ day <= Days
  /\ ~Exists(day, ScheduledCivil)
  /\ slots' = IF FireGapAtTransition THEN slots \cup {TransitionKey(day)} ELSE slots
  /\ day' = day + 1

Stutter == UNCHANGED vars

Next == FireDay \/ GapDay \/ Stutter

Spec == Init /\ [][Next]_vars

TypeOK ==
  /\ day \in 0 .. Days + 1
  /\ slots \subseteq Nat

\* Every emitted slot is a UTC instant that the calendar or an enabled fault
\* explains.
ValidSlotKeys ==
  slots \subseteq UNION { ExpectedKeys(d) : d \in 0 .. Days }

\* A selected civil time in the spring-forward gap fires once, at the
\* transition instant. Skipping it violates this.
GapFiresAtTransition ==
  (Mode # "spring") \/ (day <= TransitionDay) \/ (TransitionKey(TransitionDay) \in slots)

\* A selected civil time in the fall-back overlap fires once, at the first
\* occurrence. The second occurrence never becomes a slot key.
FallbackFiresOnce ==
  (Mode # "fall") \/ (day <= TransitionDay) \/ (SecondKey(TransitionDay) \notin slots)

=============================================================================
