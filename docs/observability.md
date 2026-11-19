# Observability

Mayfly exposes Prometheus metrics, one structured log line per HTTP request, and a request id that ties a response to its log line.

## Metrics endpoint

The endpoint serves the Prometheus text format. It is enabled by default.

| Key | Default | Meaning |
|---|---|---|
| `server.metrics.enabled` | `true` | Serve the endpoint |
| `server.metrics.address` | empty | Serve metrics on a listener of its own, such as `:9090`. Empty serves `/metrics` on the main HTTP port |
| `server.metrics.require_auth` | `true` on the main port, `false` on a separate address | Require a token with the `admin` scope as `Authorization: Bearer <token>` |

On the main port, `/metrics` is part of the public API listener, so it requires a token with the `admin` scope by default. A missing or unknown token is `401`, and a valid token without `admin` is `403`. A separate address is meant for an in-cluster scrape and needs no token unless you set `require_auth: true`. A separate address does not serve any other path.

`server.metrics.address` must differ from `server.addr`. The control plane rejects a configuration where the two match.

## Metrics

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `mayfly_sessions` | gauge | `state` | Current sessions by state: `pending`, `running`, `closing`, `completed`, `failed`, `expired`. Includes finished sessions restored from the record store |
| `mayfly_sessions_created_total` | counter | none | Sessions this process accepted. A repeated idempotent create is not counted |
| `mayfly_sessions_finished_total` | counter | `result` | Sessions that reached a terminal state: `completed`, `failed`, `expired` |
| `mayfly_queue_jobs` | gauge | `state` | Queued jobs by state: `pending`, `leased`, `dead`. Read from the queue store at scrape time; a failed read leaves the gauge unset |
| `mayfly_queue_job_attempts_total` | counter | `result` | Job attempts this process ran: `succeeded`, `failed`, `retry_later` |
| `mayfly_http_requests_total` | counter | `route`, `method`, `code` | Requests served |
| `mayfly_http_request_duration_seconds` | histogram | `route`, `method` | Request duration |
| `mayfly_llm_requests_total` | counter | `model`, `result` | Model calls the broker proxied upstream: `ok`, `error` |
| `mayfly_llm_request_duration_seconds` | histogram | `model` | Model call duration, from the request to the last response byte |
| `mayfly_gateway_calls_total` | counter | `tool`, `result` | Tool calls the gateway finished or refused: `ok`, `error`, `interrupted`, `denied`, `rate_limited`, `busy` |
| `mayfly_build_info` | gauge | `version`, `commit`, `date`, `go_version` | Build identity; the value is always 1 |

The `route` label is the registered route template, such as `/api/v1/sessions/{id}`. A request that matches no route uses `unmatched`. No metric carries a session id, a job id, or a raw request path, so label cardinality stays bounded.

A tool call that the gateway answers from the call ledger is a replay, not a new call, and is not counted.

`mayfly_sessions_finished_total` counts the sessions this process saw finish. A session that a restart settles as failed, because its runtime never came back, appears in `mayfly_sessions` with state `failed` but not in the finished counter, because this process never created it.

The registry also includes the standard Prometheus Go and process collectors. They publish `go_goroutines`, the `go_memstats_*` and `go_gc_*` series, `process_cpu_seconds_total`, `process_resident_memory_bytes`, and the other `process_*` series for the control-plane process. [loadtest.md](loadtest.md) reads `go_goroutines` and `process_resident_memory_bytes` to report runtime growth.

## Request logs

Every request writes one line at info level with these fields:

- `request_id`
- `method`
- `route`, the route template
- `status`
- `duration_ms`
- `bytes`

The control plane accepts an incoming `X-Request-Id` header when it holds 1 to 64 characters from `A-Z a-z 0-9 . _ -`. Otherwise it generates one. It returns the id in the `X-Request-Id` response header and in `error.details.request_id` for a failed `/api/v1` or `/hooks` request.

```json
{"time":"2026-01-02T03:04:05Z","level":"INFO","msg":"http request","request_id":"7f3c9a1b2d4e5f60","method":"POST","route":"/api/v1/sessions","status":201,"duration_ms":12,"bytes":348}
```

## Helm

| Value | Default | Meaning |
|---|---|---|
| `metrics.enabled` | `true` | Serve the metrics endpoint |
| `metrics.address` | empty | Separate listen address. Must end in `:<metrics.port>` |
| `metrics.port` | `9090` | Container and Service port used when `metrics.address` is set |
| `metrics.requireAuth` | empty | Empty keeps the endpoint behavior described above |
| `metrics.prometheusAnnotations` | `false` | Add `prometheus.io/scrape`, `prometheus.io/path`, and `prometheus.io/port` to the control-plane pod |
| `metrics.serviceMonitor.enabled` | `false` | Create a Prometheus Operator `ServiceMonitor` |
| `metrics.serviceMonitor.interval` | `30s` | Scrape interval |
| `metrics.serviceMonitor.scrapeTimeout` | `10s` | Scrape timeout |
| `metrics.serviceMonitor.labels` | empty | Extra labels, such as the selector your Prometheus instance uses |
| `metrics.serviceMonitor.namespace` | empty | Create the `ServiceMonitor` in this namespace and select the Service across namespaces |
| `metrics.serviceMonitor.bearerTokenSecret.name` | empty | Secret that holds a token with the `admin` scope for an authenticated scrape |
| `metrics.serviceMonitor.bearerTokenSecret.key` | `token` | Key in that Secret |

A `ServiceMonitor` that scrapes the main port needs a token with the `admin` scope. Set `bearerTokenSecret`, or serve metrics on `metrics.address` with `metrics.requireAuth: false` and let an in-cluster scrape reach that port. The chart stops the render with an error when neither is set.

```bash
helm upgrade --install mayfly deploy/helm/mayfly \
  --namespace mayfly \
  --set existingSecret=mayfly-secrets \
  --set metrics.address=:9090 \
  --set metrics.serviceMonitor.enabled=true \
  --set metrics.serviceMonitor.bearerTokenSecret.name=mayfly-secrets \
  --set metrics.serviceMonitor.bearerTokenSecret.key=MAYFLY_API_TOKEN
```

## Scrape configuration

A plain Prometheus scrape of the main port:

```yaml
scrape_configs:
  - job_name: mayfly
    metrics_path: /metrics
    authorization:
      credentials_file: /etc/prometheus/mayfly-token
    static_configs:
      - targets: ["mayfly.mayfly.svc.cluster.local:8080"]
```

A separate address needs no token when `require_auth` is unset:

```yaml
scrape_configs:
  - job_name: mayfly
    static_configs:
      - targets: ["mayfly.mayfly.svc.cluster.local:9090"]
```

Useful queries:

```promql
# Requests that failed, by route.
sum by (route, code) (rate(mayfly_http_requests_total{code=~"5.."}[5m]))

# p95 request latency.
histogram_quantile(0.95, sum by (le, route) (rate(mayfly_http_request_duration_seconds_bucket[5m])))

# Sessions that are not finished.
sum by (state) (mayfly_sessions{state=~"pending|running|closing"})

# Dead jobs.
mayfly_queue_jobs{state="dead"}
```
