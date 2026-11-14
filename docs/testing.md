# Testing

Mayfly has three test layers. The first two run in seconds without a cluster; the third runs the real thing.

| Layer | Command | What it proves |
|---|---|---|
| Unit | `make test` | Package behavior: config, triggers, queue stores (memory, bbolt, Redis), journal stores and sequence reservation (memory, bbolt), turn identity and replay checks, pod spec, claim sizing and garbage collection, pod failure handling, restart budget, retry budgets, broker, bundle, conversation replay |
| Integration | `go test ./test/integration` | Webhook → queue → worker → real `mayfly-agent` process → MCP → WebSocket → close → artifact, in one process; plus a SIGKILL/resume test on the local backend |
| End-to-end | `make e2e` | The released images and Helm chart on a real Kubernetes cluster with an enforcing CNI |

## End-to-end suite

`make e2e` runs [hack/e2e.sh](../hack/e2e.sh), which:

1. builds the four images from the working tree,
2. creates a kind cluster (`mayfly-e2e`) with Calico instead of kind's default CNI, so NetworkPolicy is enforced,
3. loads the images, deploys the deterministic LLM and MCP stubs, and installs the local chart with [e2e/values.yaml](../e2e/values.yaml),
4. runs the Go tests in [e2e/](../e2e) (build tag `e2e`) against the release over a port-forward,
5. deletes the cluster.

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
| `TestControlPlaneAuth` | Probes are open; the admin API needs the admin token; the admin token is refused by the session-only LLM proxy |
| `TestGenericWebhookSignatures` | The generic trigger accepts an HMAC signature or the shared token, and rejects unsigned, malformed, wrong-secret, and wrong-body requests without queueing anything |
| `TestGitHubWebhook` | GitHub triggers accept only a signature (not the static token), dedupe by `X-GitHub-Delivery`, and carry the delivery to the session |
| `TestSlackWebhook` | Slack signing, the URL verification challenge, rejection of requests older than 5 minutes, and a slash command that starts a session |
| `TestDatadogWebhook` | A signed Datadog alert starts a titled session |
| `TestWebhookRateLimit` | Only verified requests spend a trigger's budget; over the limit answers 429 with `Retry-After` |

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

Requirements: Docker, kind, kubectl, Helm 3, Go, and openssl. The kind cluster uses about 3 GB of memory.

### Against a real cluster

Set `E2E_KUBECONTEXT` to skip kind. The script pushes the images to `E2E_REGISTRY`, deploys the stubs and the chart into the `mayfly-e2e` namespace, runs the same suite, then removes the release, the namespace, and the cluster-scoped admission policy it added.

```bash
E2E_KUBECONTEXT=my-cluster E2E_REGISTRY=ghcr.io/hayfever/e2e make e2e
```

The cluster needs Kubernetes 1.30+ (for `ValidatingAdmissionPolicy`), a CNI that enforces NetworkPolicy (Calico or Cilium), and a default StorageClass. The registry must be pullable by the cluster. The suite reaches the control plane through `kubectl port-forward`; set `MAYFLY_E2E_URL` to use an ingress instead. To build and push the images on another machine, set the same `E2E_REGISTRY` and `E2E_TAG` there and run `E2E_SKIP_BUILD=1` where the tests run.

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

[.github/workflows/e2e.yml](../.github/workflows/e2e.yml) runs `make e2e` on kind for pushes to `main`, pull requests, and manual dispatch, once per queue backend (bbolt and Redis). Diagnostics are uploaded when it fails.
