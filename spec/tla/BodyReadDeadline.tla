----------------------------- MODULE BodyReadDeadline -----------------------------
\* A handler that reads a request body after the headers arrive.
\*
\* The client sends the headers, then stalls the body. With ApplyDeadline the
\* server bounds the read with server.body_read_timeout: when the stall
\* reaches BodyReadTimeout, DeadlineAborts releases the handler. Without it the
\* handler can wait forever, so NoUnboundedBodyRead fails.
\*
\* Time is an abstract tick, not a clock. Stall models time passing with no body
\* byte. A server with a deadline cannot wait past it; MaxTicks keeps the
\* negative run finite. Artifact uploads and streamed routes are outside this
\* model: they either make progress or set their own deadline.
EXTENDS Naturals

CONSTANTS BodyReadTimeout, MaxTicks, ApplyDeadline

VARIABLES phase, waited

vars == <<phase, waited>>

Init ==
  /\ phase = "idle"
  /\ waited = 0

\* The client completes the headers. The body is still missing.
HeadersSent ==
  /\ phase = "idle"
  /\ phase' = "reading"
  /\ waited' = 0

BodyArrives ==
  /\ phase = "reading"
  /\ phase' = "released"
  /\ UNCHANGED waited

\* Time passes with no body byte. A server with a deadline cannot wait past
\* it; a server without one keeps waiting up to MaxTicks.
Stall ==
  /\ phase = "reading"
  /\ waited < MaxTicks
  /\ (ApplyDeadline = FALSE \/ waited < BodyReadTimeout)
  /\ waited' = waited + 1
  /\ UNCHANGED phase

\* The read deadline fires and the handler returns its 408.
DeadlineAborts ==
  /\ ApplyDeadline
  /\ phase = "reading"
  /\ waited = BodyReadTimeout
  /\ phase' = "released"
  /\ UNCHANGED waited

Stutter == UNCHANGED vars

Next == HeadersSent \/ BodyArrives \/ Stall \/ DeadlineAborts \/ Stutter

Spec == Init /\ [][Next]_vars

TypeOK ==
  /\ phase \in {"idle", "reading", "released"}
  /\ waited \in Nat

\* A handler blocked on a request body never waits past the deadline.
NoUnboundedBodyRead ==
  phase = "reading" => waited <= BodyReadTimeout

=============================================================================
