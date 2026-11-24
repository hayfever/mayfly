----------------------------- MODULE ReleaseGating -----------------------------
\* The release pipeline for one v* tag, as .github/workflows/release.yml runs
\* it for #50.
\*
\* The gate job proves that the tagged commit has a green ci run and a
\* successful 20-run e2e-stability gate. GoReleaser builds the CLI archives and
\* checksums.txt and creates the GitHub release; the images are built, scanned,
\* signed, and only then tagged; the chart is pushed and signed; and a final
\* publish job makes the draft public.
\*
\* Fault switches:
\*   RequireGate        the gate must pass before a release artifact is public
\*   DraftUntilComplete the GitHub release is a draft until every artifact exists
\*   SignBeforeTag      an image tag is created only for a signed digest
\*
\* The model abstracts each job as an action. A job that fails before its
\* action is enabled simply leaves the state unchanged; only the three
\* guarantees above decide what may become public.
EXTENDS Naturals

CONSTANTS RequireGate, DraftUntilComplete, SignBeforeTag

VARIABLES gate, checksums, digest, imageSigned, imageTag, chart, release

vars == <<gate, checksums, digest, imageSigned, imageTag, chart, release>>

\* The gate job succeeded, or the pipeline has no gate to enforce.
GateOK == RequireGate = FALSE \/ gate = "pass"

Init ==
  /\ gate = "pending"
  /\ checksums = "missing"
  /\ digest = "none"
  /\ imageSigned = FALSE
  /\ imageTag = FALSE
  /\ chart = FALSE
  /\ release = "none"

\* The gate job decides from the tagged SHA's workflow history.
GatePass ==
  /\ gate = "pending"
  /\ gate' = "pass"
  /\ UNCHANGED <<checksums, digest, imageSigned, imageTag, chart, release>>

GateFail ==
  /\ gate = "pending"
  /\ gate' = "fail"
  /\ UNCHANGED <<checksums, digest, imageSigned, imageTag, chart, release>>

\* GoReleaser builds the archives and checksums.txt and creates the GitHub
\* release. With DraftUntilComplete the release is a draft; without it,
\* release.draft: false makes the release public at this point.
BuildCLI ==
  /\ GateOK
  /\ release = "none"
  /\ checksums = "missing"
  /\ checksums' = "built"
  /\ release' = IF DraftUntilComplete THEN "draft" ELSE "public"
  /\ UNCHANGED <<gate, digest, imageSigned, imageTag, chart>>

\* cosign sign-blob --bundle checksums.txt.
SignChecksums ==
  /\ checksums = "built"
  /\ checksums' = "signed"
  /\ UNCHANGED <<gate, digest, imageSigned, imageTag, chart, release>>

\* Buildx pushes the multi-arch manifest by digest, without a tag.
BuildImage ==
  /\ digest = "none"
  /\ digest' = "built"
  /\ UNCHANGED <<gate, checksums, imageSigned, imageTag, chart, release>>

\* Trivy scans the digest; a finding stops the job here.
ScanImage ==
  /\ digest = "built"
  /\ digest' = "scanned"
  /\ UNCHANGED <<gate, checksums, imageSigned, imageTag, chart, release>>

\* cosign sign signs the digest.
SignImage ==
  /\ digest = "scanned"
  /\ ~imageSigned
  /\ imageSigned' = TRUE
  /\ UNCHANGED <<gate, checksums, digest, imageTag, chart, release>>

\* docker buildx imagetools create makes the version tags.
TagImage ==
  /\ GateOK
  /\ digest = "scanned"
  /\ ~imageTag
  /\ (SignBeforeTag = FALSE \/ imageSigned)
  /\ imageTag' = TRUE
  /\ UNCHANGED <<gate, checksums, digest, imageSigned, chart, release>>

\* The chart job pushes and signs the chart after the images. The push and the
\* signature are one action: the job fails as a unit, and a failed job leaves
\* the release a draft.
Chart ==
  /\ GateOK
  /\ release # "none"
  /\ imageTag
  /\ ~chart
  /\ chart' = TRUE
  /\ UNCHANGED <<gate, checksums, digest, imageSigned, imageTag, release>>

\* The publish job flips the draft public.
Publish ==
  /\ GateOK
  /\ release = "draft"
  /\ checksums = "signed"
  /\ imageSigned
  /\ imageTag
  /\ chart
  /\ release' = "public"
  /\ UNCHANGED <<gate, checksums, digest, imageSigned, imageTag, chart>>

Stutter == UNCHANGED vars

Next ==
  \/ GatePass
  \/ GateFail
  \/ BuildCLI
  \/ SignChecksums
  \/ BuildImage
  \/ ScanImage
  \/ SignImage
  \/ TagImage
  \/ Chart
  \/ Publish
  \/ Stutter

Spec == Init /\ [][Next]_vars

TypeOK ==
  /\ gate \in {"pending", "pass", "fail"}
  /\ checksums \in {"missing", "built", "signed"}
  /\ digest \in {"none", "built", "scanned"}
  /\ imageSigned \in BOOLEAN
  /\ imageTag \in BOOLEAN
  /\ chart \in BOOLEAN
  /\ release \in {"none", "draft", "public"}

\* Nothing is public without a successful gate.
NoPublishWithoutGate ==
  release = "public" => gate = "pass"

\* A public release has every artifact: signed checksums, a signed and tagged
\* image, and a signed chart.
PublicReleaseComplete ==
  release = "public" =>
    /\ checksums = "signed"
    /\ imageSigned
    /\ imageTag
    /\ chart

\* An image tag never names an unsigned digest.
TagOnlySignedDigests ==
  imageTag => imageSigned

=============================================================================
