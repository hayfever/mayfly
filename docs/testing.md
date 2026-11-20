# Testing

Mayfly has three test layers. The first two run in seconds without a cluster; the third runs the real thing.

| Layer | Command | What it proves |
|---|---|---|
| Unit | `make test` | Package behavior: config, triggers, queue stores (memory, bbolt, Redis), journal stores and sequence reservation (memory, bbolt), turn identity and replay checks, pod spec, claim sizing and garbage collection, pod failure handling, restart budget, retry budgets, broker, bundle, conversation replay, plugin manifests and names, the tool gateway resume table and ledger, the inline plugin host, the shim, and service reconcile |
| Integration | `go test ./test/integration` | Webhook → queue → worker → real `mayfly-agent` process → MCP → WebSocket → close → artifact, in one process; plus a SIGKILL/resume test on the local backend |
| End-to-end | `make e2e` | The released images and Helm chart on a real Kubernetes cluster with an enforcing CNI |

`go test ./test/compat` checks that the current build reads each committed persisted-format fixture set: queue jobs on bbolt and redis, journal steps, the schedule watermark, session records, and the artifact index. [development.md](development.md#persisted-formats-and-migrations) describes the fixture sets and the migration path.

Plugin behavior is covered in the first two layers:

- `internal/plugin` checks every manifest in [plugins.md](plugins.md), the defaults, the placement blocks, the names, and the loader paths.
- `internal/gateway` checks the resume table, key reuse, rate limits, the in-flight cap, turn and step bounds, the grant, the audit fields, call tokens, and remote dispatch against an in-process MCP server.
- `internal/pluginhost` runs a fixture plugin through start, verification, restart, crash, timeout, `_meta`, stderr capping, truncation, and close. The sidecar client is checked against a streamable HTTP server.
- `internal/pluginsvc` checks the rendered Deployment, Service, NetworkPolicy, key Secret, prune, the HTTP transport, and a local shim child against a fake clientset.
- `cmd/mayfly-plugin-shim` checks `install-self`, `copy`, `serve` (discovery and call tokens, replay dedupe, multiplexing, child restart, environment), and `probe`.
- `test/integration` drives a local plugin session, a non-idempotent resume (not repeated), an idempotent resume (repeated with the same key), a completed call (replayed), and a stdio MCP server through the gateway.

## End-to-end suite

`make e2e` runs [hack/e2e.sh](../hack/e2e.sh), which:

1. builds the four images from the working tree,
2. creates a k3d cluster (`mayfly-e2e`) on k3s, whose built-in kube-router controller enforces NetworkPolicy,
3. loads the images, deploys the deterministic LLM and MCP stubs, and installs the local chart with [e2e/values.yaml](../e2e/values.yaml),
4. runs the Go tests in [e2e/](../e2e) (build tag `e2e`) against the release over a port-forward,
5. deletes the cluster.

Step 4 keeps one `kubectl port-forward` to the control plane Service. The forward pins the pod it resolved at startup, so the suite starts a new forward after a control-plane restart and replaces one whose kubectl process exited before each request. A failed restart does not leave a broken tunnel for the tests that follow.

No API keys are needed: `llmstub` answers every model call. A full run takes about 8 minutes from scratch, about 2 of them in tests.

### Scenarios

| Test | What it proves |
|---|---|
| `TestWebhookSessionLifecycle` | A signed webhook is queued (202 + `job_id`), a redelivery is deduplicated, the worker starts a session pod, the agent writes a file and calls MCP through the control-plane proxies, a WebSocket attach replays the thread, a human follow-up gets an answer, close exports a complete bundle (report, traces, manifest), and the pod, Secret, ConfigMap, and NetworkPolicy are deleted |
| `TestNetworkIsolation` | From a session pod: DNS and the control plane work; the Kubernetes API, the LLM and MCP backends (bypassing the proxy), another session, cloud metadata, and the internet do not. Nothing in the namespace can reach a session. Each blocked path has a control showing it works without the policy |
| `TestQueueSurvivesRestart` | With `sessions.max_concurrent` full, a webhook waits without spending attempts. After the control plane pod is replaced, the waiting job, a finished job's record, and the running sessions (adopted) are all still there, and the job runs when a slot frees |
| `TestDeadLetter` | A job whose session cannot start (pod denied by an admission policy) is retried, buried after `queue.max_attempts`, leaves no objects behind, and can be retried and deleted through the dead-letter API |
| `TestSessionTTLExpiry` | A session past its TTL ends `expired`, exports a bundle recording the reason, and is cleaned up |
| `TestSessionResumesAfterPodDeletion` | Force-deleting a running session pod mid-turn starts a replacement with the same workspace, the turn completes, the user message appears once, and a completed MCP tool call runs once |
| `TestSessionResumesAfterGracefulPodDeletion` | The same with the default grace period: SIGTERM does not finalize the session, the runtime exits non-zero, and a replacement resumes the turn |
| `TestControlPlaneRestartContinuesTurn` | Replacing `mayflyd` while the final model call is in flight adopts the pod, restores the journaled thread, finishes the turn, and does not repeat the tool call or the user message |
| `TestSessionImagePullFailure` | A session whose image cannot be pulled fails with the reason instead of waiting for its TTL |
| `TestDeleteSession` | `DELETE` stops a running session at once |
| `TestControlPlaneAuth` | Probes are open; the admin API needs an API token; an admin-scoped token is refused by the session-only LLM proxy |
| `TestGenericWebhookSignatures` | The generic trigger accepts an HMAC signature or the shared token, and rejects unsigned, malformed, wrong-secret, and wrong-body requests without queueing anything |
| `TestGitHubWebhook` | GitHub triggers accept only a signature (not the static token), dedupe by `X-GitHub-Delivery`, and carry the delivery to the session |
| `TestSlackWebhook` | Slack signing, the URL verification challenge, rejection of requests older than 5 minutes, and a slash command that starts a session |
| `TestDatadogWebhook` | A signed Datadog alert starts a titled session |
| `TestWebhookRateLimit` | Only verified requests spend a trigger's budget; over the limit answers 429 with `Retry-After` |
| `TestUpgradeFromLastRelease` | The last released chart and images are upgraded to the chart and images built from HEAD; the old sessions stay readable and listed, a queued job survives, and the upgraded control plane runs it |

After the tests pass, the suite checks that no session pods, Secrets, ConfigMaps, or NetworkPolicies are left in the namespace.

### Run it

```bash
make e2e                      # create, test, delete
E2E_KEEP=1 make e2e           # keep the cluster afterwards
E2E_QUEUE=redis make e2e      # Redis queue backend instead of bbolt
```

While iterating, keep the cluster and rerun only the tests:

```bash
make e2e-up                                            # build, create or reuse, deploy
make e2e-test                                          # run the suite again
E2E_GO_TEST_FLAGS="-run TestNetworkIsolation" make e2e-test
make e2e-down
```

`make e2e-up` rebuilds the images and restarts the control plane, so code changes are picked up. Logs and, on failure, cluster diagnostics go to `.e2e/` (`E2E_ARTIFACTS` to change).

Requirements: Docker, k3d, kubectl, Helm 3, Go, and openssl. The k3d cluster uses about 3 GB of memory.

### Upgrade test

`TestUpgradeFromLastRelease` proves that a live install survives an upgrade. It installs the chart and images of a released version in the `mayfly-upgrade` namespace, with its own copy of the stubs, fills the session slots, and queues a webhook job. It then runs `helm upgrade` to the local chart with the images built from HEAD and checks three things:

1. the old sessions are still readable and listed, with their titles and the adopted trigger,
2. the queued job record is still present,
3. the upgraded control plane runs that job when a slot frees.

The test skips unless `MAYFLY_E2E_UPGRADE_FROM` names a released chart version. The release packages are private, so Helm and the cluster need credentials:

```bash
helm registry login ghcr.io -u <github-user>

kubectl create namespace mayfly-upgrade
kubectl -n mayfly-upgrade create secret docker-registry ghcr-pull \
  --docker-server=ghcr.io --docker-username=<github-user> --docker-password=<token>

make e2e-up
MAYFLY_E2E_UPGRADE_FROM=0.1.0-rc.2 MAYFLY_E2E_PULL_SECRET=ghcr-pull \
  E2E_GO_TEST_FLAGS="-run TestUpgradeFromLastRelease" make e2e-test
make e2e-down
```

`MAYFLY_E2E_CHART_REPOSITORY` and `MAYFLY_E2E_IMAGE_PREFIX` override the default `ghcr.io/hayfever` package locations. `MAYFLY_E2E_UPGRADE_NAMESPACE` overrides the namespace. The test deletes the namespace when it passes and keeps it for inspection when it fails. The test installs both releases with the bbolt queue from [e2e/upgrade-values.yaml](../e2e/upgrade-values.yaml) and the single-agent [e2e/upgrade-agents.yaml](../e2e/upgrade-agents.yaml); both files must stay valid for every release under test. The Redis e2e variant does not run it. A run takes a few minutes because the released images are pulled from the registry.

### Against a real cluster

Set `E2E_KUBECONTEXT` to skip k3d. The script pushes the images to `E2E_REGISTRY`, deploys the stubs and the chart into the `mayfly-e2e` namespace, runs the same suite, then removes the release, the namespace, and the cluster-scoped admission policy it added.

```bash
E2E_KUBECONTEXT=my-cluster E2E_REGISTRY=ghcr.io/hayfever/e2e make e2e
```

The cluster needs Kubernetes 1.30+ (for `ValidatingAdmissionPolicy`), a CNI that enforces NetworkPolicy, and a default StorageClass. The isolation test fails on a CNI that accepts the policy but ignores it. The registry must be pullable by the cluster. The suite reaches the control plane through `kubectl port-forward`; set `MAYFLY_E2E_URL` to use an ingress instead. To build and push the images on another machine, set the same `E2E_REGISTRY` and `E2E_TAG` there and run `E2E_SKIP_BUILD=1` where the tests run.

#### Civo

[deploy/terraform/civo](../deploy/terraform/civo) creates a one-node cluster that fits: `g4s.kube.medium` (2 vCPU, 4 GB) in NYC1, k3s 1.35 with Cilium, and Civo's default `civo-volume` StorageClass. The node costs $0.03 an hour ($21.73 a month); hosting Mayfly on it adds a load balancer ($10.86) and the 2 GB volume ($0.22).

```bash
export CIVO_TOKEN=...                      # Civo dashboard → Settings → Profile → Security
cd deploy/terraform/civo
terraform init && terraform apply
civo kubernetes config mayfly --region NYC1 --save   # context: mayfly

cd ../../..
E2E_KUBECONTEXT=mayfly E2E_REGISTRY=ttl.sh/mayfly-e2e-$(openssl rand -hex 8) E2E_TAG=2h make e2e
```

`ttl.sh` is an anonymous registry whose images expire after the tag's duration; use a private registry for anything you keep. The suite leaves the cluster in place and removes only what it installed. `terraform destroy` deletes the cluster, its firewall, and its network. Keep the state file out of the repository; `.gitignore` covers it.

### CI

[.github/workflows/e2e.yml](../.github/workflows/e2e.yml) runs `make e2e` on k3d for pushes to `main`, pull requests, and manual dispatch, once per queue backend (bbolt and Redis).

The `upgrade` job resolves the last `v*` tag, logs in to `ghcr.io` with the workflow token, creates a pull secret for the released images, and runs only `TestUpgradeFromLastRelease` from that release to HEAD. Diagnostics are uploaded when either job fails.

## Stability gate

[.github/workflows/e2e-stability.yml](../.github/workflows/e2e-stability.yml) proves one commit is stable, not only correct. It runs the e2e suite 20 times and the unit stress once, all against the same commit.

| Job | What it runs |
|---|---|
| `plan` | Builds the run matrix: entry `1` to `runs`, alternating `bbolt` and `redis` |
| `e2e` | One `make e2e` per matrix entry, 10 runs at a time, 45 minute limit per run |
| `unit-stress` | `./hack/stress.sh 20` |
| `gate` | Fails unless every e2e run and the unit stress pass, and writes a pass or fail row per run to the run summary |

An odd `runs` value gives the bbolt queue one more run. A Redis-only and a bbolt-only failure both appear in the gate table.

### Trigger it

The gate runs weekly on Sunday at 06:00 UTC. Start it by hand before a release:

```bash
gh workflow run e2e-stability.yml -f runs=20 -f ref=main
```

`make e2e-stability` prints that command and does not run it. `runs` accepts 1 to 100. `ref` accepts any branch, tag, or commit.

### Unit stress

[hack/stress.sh](../hack/stress.sh) runs `go test -race -count=<count>` over the packages whose tests use real processes, sockets, and stores:

```bash
./hack/stress.sh                  # count 20, default packages
./hack/stress.sh 50               # 50 iterations
make stress                       # same as ./hack/stress.sh
./hack/stress.sh 20 ./internal/pluginhost   # one package
```

The default packages are `internal/pluginhost`, `internal/queue`, `internal/session`, `internal/httpapi`, `internal/gateway`, `internal/pluginsvc`, `hack/loadtest`, and `test/integration`. The script stops at the first failing package and prints the shuffle seed. Rerun the printed command to reproduce the test order.

### Read a failure

1. Open the run summary. The `gate` job lists one row per e2e run with `pass` or `fail`.
2. Open the failed `e2e (<run>/<queue>)` job. Its log shows the failing test and the last cluster state it polled.
3. Download the `e2e-stability-<run>-<queue>` artifact. It holds the Go test log, the control plane log, pod descriptions, and events.
4. For a `unit-stress` failure, use the rerun command in the log. The seed reproduces the test order that failed.

A failure that does not reproduce is still a release blocker: it shows a timing dependency.

### What passing means for a release

The gate must pass on the release commit before you tag a release candidate or a final release. A red gate blocks the tag. A run of the gate on a later commit does not cover an earlier one, so tag the commit the gate tested.

A green unit stress shows that the packages that fork processes and open sockets pass 20 times each under `-race`; a green e2e run shows that one queue backend survives the full suite. The gate needs 20 of 20 e2e runs green, because a flake in any run is a defect that a user can meet in one run.

