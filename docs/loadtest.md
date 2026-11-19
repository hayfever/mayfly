# Load and soak test

`hack/loadtest` drives concurrent sessions and webhook deliveries against a Mayfly release. It reports request latency percentiles, error rate, session throughput, and goroutine and memory growth over the run window. It uses HTTP only and needs no external service, no model, and no API key.

The release under test must send model calls to `llmstub`. The local k3d overlay and the e2e values file already do that, so a run costs nothing and every session follows the same deterministic path.

## What a run does

One worker runs this loop until the window closes:

1. `sessions` mode creates a session, waits for a terminal state, and deletes the session.
2. `webhooks` mode sends a signed webhook to `/hooks/{trigger}`, waits for the job to create a session, waits for that session, and deletes it.
3. `both` mode does the same with half the workers on sessions and half on webhooks.

A worker holds at most one session. Keep `-n` below `sessions.max_concurrent`, or creates return `429 rate_limited`. The harness never creates a Kubernetes object directly. It deletes every session it starts, and the session TTL bounds the rest.

## Local k3d

```bash
make cluster-up                                  # images, k3d cluster, stub-backed install
export MAYFLY_API_TOKEN="$(cat .mayfly/admin-token)"
make loadtest LOADTEST_ARGS="-context k3d-mayfly -n 10 -duration 10m"
```

`make loadtest` defaults to `-mode sessions -n 5 -duration 2m`. The token file comes from `hack/k3d-up.sh`; the harness reads the value from the environment and never prints it. The context is `k3d-mayfly` because `hack/k3d-up.sh` does not change your current context.

For webhook load, use the local generic trigger. It is unsigned in the local overlay, so no secret is needed:

```bash
make loadtest LOADTEST_ARGS="-context k3d-mayfly -mode webhooks -n 10 -duration 5m"
```

The default trigger budget is 30 requests per minute with a burst of 10. Raise it before a longer webhook run. Add `rate_limit` to the `generic` entry in the triggers block of `deploy/k8s/overlays/local/kustomization.yaml`, then run `make cluster-up` again:

```yaml
  - name: generic
    type: generic
    allow_unsigned: true
    rate_limit: {requests: 6000, interval: 1m, burst: 600}
```

## Any kubeconfig

Pass the kubeconfig, context, namespace, and Service name. The harness starts `kubectl port-forward svc/<service> :8080` and checks `/healthz` before load starts:

```bash
export MAYFLY_API_TOKEN=...                     # token with the admin scope
make loadtest LOADTEST_ARGS="-kubeconfig ~/.kube/config -context my-cluster -namespace mayfly -service mayfly -n 20 -duration 30m -mode both"
```

Use `-url` to skip `kubectl` and call an ingress or a port-forward you started yourself:

```bash
make loadtest LOADTEST_ARGS="-url https://mayfly.example.com -n 20 -duration 30m"
```

For a signed `generic` trigger, put the shared secret in `MAYFLY_WEBHOOK_SECRET` (or the variable named by `-webhook-secret-env`). The harness signs each delivery with HMAC-SHA256 in `X-Mayfly-Signature-256`. If the trigger has `allow_unsigned: true`, leave the variable unset.

The install must use the stub backend:

- `config.llm.base_url` points at the `llmstub` Service, for example `http://llmstub.mayfly.svc.cluster.local:9099/v1`.
- `config.llm.model` is `stub`.
- The `llmstub` Deployment runs the `llmstub` image from the Dockerfile.
- A `generic` trigger exists for webhook mode.

`deploy/k8s/overlays/local` and `e2e/values.yaml` are working examples. The Helm chart replaces the `triggers` list as a whole, so write the complete list in your values file.

## Flags

| Flag | Default | Meaning |
|---|---|---|
| `-mode` | `sessions` | `sessions`, `webhooks`, or `both` |
| `-n` | `5` | Concurrent workers |
| `-duration` | `2m` | Load and soak window |
| `-iterations` | `0` | Operations per worker; `0` runs the full window |
| `-kubeconfig` | empty | Kubeconfig file for `kubectl`; empty uses `KUBECONFIG` |
| `-context` | empty | `kubectl` context; empty uses the current context |
| `-namespace` | `mayfly` | Namespace of the control-plane Service |
| `-service` | `mayfly` | Name of the control-plane Service |
| `-url` | empty | Base URL of the control plane; skips `kubectl` |
| `-token-env` | `MAYFLY_API_TOKEN` | Environment variable that holds the API token |
| `-prompt` | `loadtest session` | Prompt sent with every session |
| `-ttl` | `10m` | Session TTL sent with every create |
| `-session-timeout` | `5m` | Wait for one session to reach a terminal state |
| `-poll-interval` | `500ms` | Poll interval for session and job state |
| `-sample-interval` | `5s` | Interval between runtime samples |
| `-webhook-trigger` | `generic` | `generic` trigger name under `/hooks/{name}` |
| `-webhook-secret-env` | `MAYFLY_WEBHOOK_SECRET` | Environment variable that holds the signing secret |
| `-metrics-url` | empty | Metrics endpoint; empty uses `<url>/metrics` |
| `-dry-run` | `false` | Print the resolved plan and exit without contacting the target |

The token needs the `admin` scope, because the harness reads `/metrics` on the main port. If `server.metrics.address` moves metrics to another listener, forward that port and pass `-metrics-url`. A separate listener needs no token unless `server.metrics.require_auth` asks for one.

## Output

The report has three sections:

```text
requests
  operation       requests  errors  p50     p95     p99     max
  session create  120       0       12ms    31ms    48ms    52ms
  webhook delivery 118      2       3ms     9ms     14ms    16ms
error rate: 0.84% (2 of 238)
errors: 429 rate_limited x2

sessions
  created 238, finalized 236, throughput 0.98/s
  outcomes: completed 233, failed 3, expired 0
  lifecycle: p50 8.2s, p95 12.0s, p99 12.9s

control plane runtime (24 samples)
  go_goroutines: start 142, end 151, peak 163, change +9
  resident memory: start 84.0 MiB, end 91.2 MiB, peak 96.4 MiB, change +7.2 MiB
```

The request table covers the primary requests only: `session create` and `webhook delivery`. Percentiles use the nearest rank, so each value is one observed request. Error rate is failed primary requests divided by attempted primary requests. Session throughput is finalized sessions per second over the actual window. Session lifecycle runs from the create request, or from the webhook delivery, to the terminal state.

The runtime block reads `go_goroutines` and `process_resident_memory_bytes` from the Prometheus endpoint. Samples that fail are skipped for the series and counted at the bottom of the block.

Exit codes:

- `0`: the run produced a report. Request and session failures appear in the report.
- `1`: the target was unreachable, or the port-forward exited during the run.
- `2`: bad flags, or the token variable is empty.

## Soak runs

Use a long window and a sparse sample interval:

```bash
make loadtest LOADTEST_ARGS="-context k3d-mayfly -mode both -n 20 -duration 2h -sample-interval 30s"
```

A flat goroutine line and a bounded resident set indicate no leak. The Go heap does not fall after every collection, so read the start, end, and peak values together. Compare two runs with the same flags for a regression check.

Run this against a test install. A soak run holds session slots for the full window and creates real session pods.

## Verification without a cluster

`go test ./hack/loadtest` runs the reporter tests and drives the full harness against an in-process fake control plane. The fake checks the bearer token and the webhook signature and serves the same response shapes. No cluster is needed.
