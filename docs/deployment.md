# Deployment

## k3d (included)

Requirements: Docker, k3d, kubectl, jq, openssl, Go 1.26+.

```bash
./hack/k3d-up.sh
```

The script:

1. Builds `mayflyd:dev`, `mayfly-agent:dev`, `mayfly-llmstub:dev`, `mayfly-mcpstub:dev`.
2. Creates the `mayfly` k3d cluster from [deploy/k3d.yaml](../deploy/k3d.yaml) if absent. k3s enforces NetworkPolicy; the cluster publishes NodePort 30080 as host port 8080.
3. Imports the images into the k3d node.
4. Creates the `mayfly` namespace and the `mayfly-secrets` Secret with an API token and webhook secret.
5. Applies the local overlay and waits for rollouts.

The overlay includes deterministic LLM and MCP stubs and an example agent manifest. Remove the stubs and point `llm.base_url` at a real provider for normal use.

Agent manifests are mounted from the `mayfly-agents` ConfigMap at `/etc/mayfly-agents`. The local overlay generates that ConfigMap from `examples/agents/triage.yaml`, so `make cluster-up` and `make deploy` replace it every time.

Put your own bundles, such as page-watch, in the `mayfly-bundles` ConfigMap instead. It is mounted at `/etc/mayfly-bundles`, the second `agents.dirs` entry. No overlay or chart creates it, and the volume is optional, so a redeploy leaves it alone and a cluster without it still starts:

```bash
kubectl -n mayfly create configmap mayfly-bundles \
  --from-file=page-watch.yaml --from-file=page-watch-sources.yaml \
  --dry-run=client -o yaml | kubectl apply -f -
kubectl -n mayfly rollout restart deployment/mayflyd
```

Bundles load at startup, so restart mayflyd after you change the ConfigMap. With Helm, the ConfigMap is `<chart fullname>-bundles` by default (`<release>-mayfly-bundles`); set `agents.bundlesConfigMap` to use another name.

To add a harness, build it into the agent image at `/opt/mayfly/harnesses/<name>` and declare `harness: <name>` in the manifest. The image entrypoint is `/usr/local/bin/mayfly-harness`; it falls back to `/usr/local/bin/mayfly-agent` when no harness is selected. No per-agent image is required.

The base config lists `/etc/mayfly-agents` and `/etc/mayfly-bundles` in `agents.dirs`, and both ConfigMap volumes are optional. The local overlay also generates `mayfly-agents` with one example agent.

The k3d config publishes the API on `127.0.0.1:8080` only. The local overlay turns on `allow_shell` and unsigned webhooks, so do not bind it to other interfaces.

Delete the cluster:

```bash
make cluster-down
```

To check a cluster before relying on it, run the end-to-end suite against it. It installs into its own namespace and removes itself afterwards. See [testing.md](testing.md#against-a-real-cluster).

## Helm

A tagged release publishes the chart to `oci://ghcr.io/hayfever/charts/mayfly`. The current release candidate is `v0.1.0-rc.2`, and its images and chart are private, so pulling them needs a GitHub token with `read:packages`. To install from a checkout, build the images, import them into the cluster, and set the image names and tags:

```bash
make image
k3d image import --cluster mayfly mayflyd:dev mayfly-agent:dev

kubectl create namespace mayfly
kubectl -n mayfly create secret generic mayfly-secrets \
  --from-literal=MAYFLY_API_TOKEN="$(openssl rand -hex 24)" \
  --from-literal=MAYFLY_WEBHOOK_SECRET="$(openssl rand -hex 24)" \
  --from-literal=OPENAI_API_KEY="sk-..."

helm install mayfly deploy/helm/mayfly \
  --namespace mayfly \
  --set existingSecret=mayfly-secrets \
  --set image.repository=mayflyd \
  --set image.tag=dev \
  --set agentImage.repository=mayfly-agent \
  --set agentImage.tag=dev \
  --set config.server.public_url=https://mayfly.example.com
```

To install the published chart instead:

```bash
helm install mayfly oci://ghcr.io/hayfever/charts/mayfly \
  --version 0.1.0-rc.2 \
  --namespace mayfly \
  --set existingSecret=mayfly-secrets \
  --set config.server.public_url=https://mayfly.example.com
```

The chart creates the control-plane and agent ServiceAccounts, RBAC, ConfigMaps for skills and agent manifests, Service, optional Ingress, and optional LiteLLM deployment. Set `image.digest` and `agentImage.digest` for immutable images and `imagePullSecrets` for private registries. See [../deploy/helm/mayfly/README.md](../deploy/helm/mayfly/README.md).

Service plugins need `config.plugins.namespace`. When it is empty it follows `config.sessions.namespace`. The chart renders the plugin Role and RoleBinding in that namespace when it differs from the release namespace: create, get, list, update, and delete on Secrets, Services, Deployments, and NetworkPolicies. Set `config.agents.allowed_images` to allow the plugin images, and `config.plugins.audit_path` when you want the call audit on the control-plane volume. Inline and sidecar plugins need no extra RBAC: their objects are part of the session pod.

## Real cluster with Kustomize

1. Build and push images:

```bash
docker build --target mayflyd -t registry.example.com/mayflyd:v0.1.0 .
docker build --target mayfly-agent -t registry.example.com/mayfly-agent:v0.1.0 .
docker push registry.example.com/mayflyd:v0.1.0
docker push registry.example.com/mayfly-agent:v0.1.0
```

2. Edit the base manifests or overlay:

- `deployment.yaml`: control-plane image and optional agent ConfigMap mount.
- `configmap.yaml`: `runtime.image`, `server.public_url`, `server.internal_url`, LLM, MCP, agents, plugins, triggers, artifact store.
- `serviceaccount.yaml` / `rbac.yaml`: namespace and permissions if you do not use `mayfly`.
- `service.yaml`: use `ClusterIP` or `LoadBalancer` behind your ingress; NodePort is for the demo.

Kustomize builds must allow skill files outside the overlay directory:

```bash
kubectl kustomize --load-restrictor=LoadRestrictionsNone deploy/k8s/overlays/local | kubectl apply -f -
```

For a real cluster, create your own overlay with your images, config, and skills. Keep `--load-restrictor=LoadRestrictionsNone` if skills live outside the overlay.

3. Create the secret. The control plane reads these by environment name:

```bash
kubectl -n mayfly create secret generic mayfly-secrets \
  --from-literal=MAYFLY_API_TOKEN="$(openssl rand -hex 24)" \
  --from-literal=MAYFLY_WEBHOOK_SECRET="$(openssl rand -hex 24)" \
  --from-literal=OPENAI_API_KEY="sk-..." \
  --from-literal=S3_ACCESS_KEY="..." \
  --from-literal=S3_SECRET_KEY="..." \
  --from-literal=GITHUB_MCP_TOKEN="..."
```

Use `envFrom` for additional variables or add them to the Deployment. Do not put credentials in `mayfly-config`.

4. Set `server.public_url` to the externally reachable URL, and `server.internal_url` to the in-cluster Service URL. Attach tickets and session links use `public_url`; session pods use `internal_url`.

5. Expose the API through an ingress with TLS and authentication. The default API token has the admin scope and is not a substitute for SSO. Prefer named tokens with narrow scopes.

## S3-compatible artifacts

```yaml
artifacts:
  store: s3
  s3:
    endpoint: https://s3.us-east-1.amazonaws.com
    region: us-east-1
    bucket: mayfly-artifacts
    access_key_env: S3_ACCESS_KEY
    secret_key_env: S3_SECRET_KEY
    path_style: false
    prefix: prod
  max_bytes: 268435456
```

For MinIO:

```yaml
    endpoint: http://minio.minio.svc.cluster.local:9000
    path_style: true
```

On AWS with a service-account role, omit the key variables and let the SDK use the pod's IAM role:

```yaml
artifacts:
  store: s3
  s3:
    region: us-east-1
    bucket: mayfly-artifacts
    prefix: prod
```

The control plane then picks up web identity (`IRSA`), ECS task role, or instance metadata. `access_key_env` and `secret_key_env` must be set together or left empty; one alone is a startup error. The role needs `s3:GetObject` and `s3:PutObject` on the prefix. `/readyz` does not call the store, so a bucket outage does not remove the replica from the Service and does not drop live sessions.

For GCS interoperability, use the GCS XML endpoint and HMAC keys. Native GCS JSON API support requires a new `artifact.Store`.

The control plane writes and reads bundles. Session pods never receive storage credentials. Downloads still work through `GET /api/v1/sessions/{id}/artifact`; the API streams from the store.

## Queue

Webhooks are accepted into a queue before a pod exists. Choose the backend by what you already operate:

- `bbolt` (chart default): one file at `/var/lib/mayfly/queue.db`. Control-plane persistence is on by default, so the file survives pod replacement. Turning it off is only reasonable with the `redis` queue and a throwaway control plane:

  ```bash
  helm upgrade mayfly deploy/helm/mayfly --set persistence.enabled=false
  ```

  One replica only: bbolt locks the file.
- `redis`: any Redis-protocol service, including Valkey, ElastiCache, Memorystore, Azure Managed Redis, and Upstash. Set `queue.backend=redis` and `queue.redis.url`, and keep the cache non-evicting with AOF enabled. This is the option that allows more than one replica later.

The `memory` backend loses accepted work on restart. Use it only for development.

The turn journal (`journal.bbolt.path`, default `/var/lib/mayfly/journal.db`) shares the control-plane volume, so it survives a restart with the same default persistence. Session records and the artifact index use `sessions.records.path` (default `/var/lib/mayfly/sessions.db`) on the same volume: after a restart, `GET /api/v1/sessions` still lists finished sessions and `GET /api/v1/sessions/{id}/artifact` still downloads their bundles. Disabling `persistence` loses the journal and the session records on every control-plane restart. A bundle that an older build uploaded has no index entry: the object store still holds it, but the API cannot serve it until a session uploads again. A runtime that is mid-turn still finishes its turn, but the thread history from before the restart is gone, and the session's new messages are numbered from the restart time in microseconds so none reuses the in-flight turn's key. The kustomize base (`deploy/k8s/base`) mounts the same state directory from the `mayfly-state` claim.

## Shutdown

A SIGTERM starts one stop sequence with a single budget, `server.shutdown_budget` (default `25s`). The budget splits into the HTTP listener drain, the queue worker drain, and the session manager shutdown, at 20, 40, and 40 percent in that order. Each stage gets its own deadline inside the budget, so a slow drain cannot leave the session manager with an expired context.

The pod gets `terminationGracePeriodSeconds` before SIGKILL. The chart templates that value from `shutdown.gracePeriodSeconds` (default `30`) and writes `shutdown.budgetSeconds` (default `25`) into `server.shutdown_budget`. `helm template` fails when the budget is not smaller than the grace period. Keep a few seconds of margin: the process needs time to exit after its own budget ends.

With your own manifest, set both values. A budget that reaches the grace period loses the last stage to SIGKILL.

A job that exhausts `queue.max_attempts` is buried. List it with `GET /api/v1/queue/dead`, inspect it with `GET /api/v1/queue/jobs/{id}`, then retry (`POST /api/v1/queue/dead/{id}/retry`) or delete it. Buried jobs are kept until you act; size the queue volume or cache for that. Completed jobs keep a session link for `queue.tombstone_ttl`.

## Operating notes

- Run one control-plane replica. Finished session records and the artifact index survive a restart through `sessions.records`; thread history and the token index do not. Pod adoption after a restart works from ConfigMap and Secret, and the last journaled turn is replayed into the new thread. A session that adoption did not rebuild is recorded as failed, because no runtime can continue its turn. Finalized sessions leave the list after `sessions.retention`; artifact metadata stays until `artifacts.index_retention` passes. Accepted webhook work and the in-flight turn's step journal survive with `bbolt` or `redis` (queue) and `journal.backend: bbolt`.
- Remote skill archives require outbound network access from the control plane at startup. If you prefer GitOps, sync the skill repository to a volume and use `skills.dirs` instead; the platform then needs no fetch capability.

For multiple model providers, deploy LiteLLM and point `llm.base_url` at its Service. A starting manifest is in [examples/litellm/deployment.yaml](../examples/litellm/deployment.yaml); see [providers.md](providers.md) for keys, IRSA, and model aliases.

The control plane needs RBAC for pods, pod logs, secrets, configmaps, networkpolicies, deployments, and services in the session namespace. Service plugins need the same permissions on secrets, services, deployments, and networkpolicies in `plugins.namespace`. See [rbac.yaml](../deploy/k8s/base/rbac.yaml).
- Session pods use a dedicated ServiceAccount with no bindings and token automount disabled.
- Set `runtime.network_policy: true` and verify your CNI enforces NetworkPolicy.
- Add ResourceQuota and LimitRange to cap concurrent sessions.
- Each session creates one Secret, ConfigMap, NetworkPolicy, and Pod. Cleanup happens on finalize. If the control plane dies mid-close, the pod remains until its `activeDeadlineSeconds` backstop.
- Local artifacts on an EmptyDir volume are lost when the control-plane pod restarts. Use S3 for durability.
- There is no Prometheus metrics endpoint yet; use structured logs and Kubernetes probes.
- Artifact bundles can contain sensitive data. Apply bucket encryption, retention, and access policy.
- The web UI stores the token in localStorage. Treat the browser profile accordingly.
- `kubectl delete namespace mayfly` removes all sessions and control-plane state. Artifacts in S3 remain.

## Troubleshooting

```bash
kubectl -n mayfly logs deploy/mayflyd --tail=100
kubectl -n mayfly get pods -l mayfly.io/managed=true
kubectl -n mayfly describe pod mayfly-<id>
kubectl -n mayfly logs mayfly-<id> -c agent
kubectl -n mayfly get networkpolicy mayfly-<id> -o yaml
```

Common causes:

| Symptom | Check |
|---|---|
| Pod stays Pending | Image loaded or pullable; namespace quota; service account exists |
| Runtime cannot reach control plane | `server.internal_url`, Service name/port, NetworkPolicy egress labels |
| Model calls fail | `llm.base_url`, API key, model allowlist, egress from control plane |
| MCP tool missing | Server assigned to session, upstream reachable from control plane, headers resolved, `tools` allowlist |
| Plugin tool missing | Plugin in the agent `plugins` list, manifest under `plugins/`, `mayflyd -check` output, image allowlist |
| Plugin unavailable at session start | Init and sidecar logs, service Deployment and probes, gateway Secret in `plugins.namespace`, `mayflyd` reconcile errors |
| Service plugin tool fails | Service pod logs, NetworkPolicy egress to `service.egress` and DNS, call token key in `mayfly-gateway-<name>` |
| No artifact | Runtime logs; close grace; fallback bundle; store credentials |
| Attach fails | `server.public_url` scheme and host; ticket expiry; ingress WebSocket support |
