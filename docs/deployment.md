# Deployment

## kind (included)

Requirements: Docker, kind, kubectl, jq, openssl, Go 1.26+.

```bash
./hack/kind-up.sh
```

The script:

1. Builds `mayflyd:dev`, `mayfly-agent:dev`, `mayfly-llmstub:dev`, `mayfly-mcpstub:dev`.
2. Creates the `mayfly` kind cluster from [deploy/kind.yaml](../deploy/kind.yaml) if absent. The cluster maps host port 8080 to NodePort 30080.
3. Loads the images into the node.
4. Creates the `mayfly` namespace and the `mayfly-secrets` Secret with an admin token and webhook secret.
5. Applies the kind overlay and waits for rollouts.

The overlay includes deterministic LLM and MCP stubs and an example agent manifest. Remove the stubs and point `llm.base_url` at a real provider for normal use.

Agent manifests are mounted from the `mayfly-agents` ConfigMap at `/etc/mayfly-agents`. Add files with the same kustomize generator pattern, or patch `agents.dirs`.

To add a harness, build it into the agent image at `/opt/mayfly/harnesses/<name>` and declare `harness: <name>` in the manifest. The image entrypoint is `/usr/local/bin/mayfly-harness`; it falls back to `/usr/local/bin/mayfly-agent` when no harness is selected. No per-agent image is required.

The base overlay does not enable agent directories by default. Add them when you mount manifests. The kind overlay enables `/etc/mayfly-agents` and includes one example agent.

Delete the cluster:

```bash
make kind-down
```

The e2e test ([hack/e2e-kind.sh](../hack/e2e-kind.sh)) checks:

- webhook creation,
- pod security context and NetworkPolicy existence,
- cloud metadata endpoint blocked,
- MCP tool call,
- WebSocket replay,
- close and artifact contents,
- TTL expiry.

## Helm

The release chart is published to `oci://ghcr.io/hayden/charts/mayfly`.

```bash
kubectl create namespace mayfly
kubectl -n mayfly create secret generic mayfly-secrets \
  --from-literal=MAYFLY_API_TOKEN="$(openssl rand -hex 24)" \
  --from-literal=MAYFLY_WEBHOOK_SECRET="$(openssl rand -hex 24)" \
  --from-literal=OPENAI_API_KEY="sk-..."

helm install mayfly oci://ghcr.io/hayden/charts/mayfly \
  --version 0.1.0 \
  --namespace mayfly \
  --set existingSecret=mayfly-secrets \
  --set config.server.public_url=https://mayfly.example.com
```

Or install the local chart:

```bash
helm install mayfly deploy/helm/mayfly \
  --namespace mayfly \
  --set existingSecret=mayfly-secrets \
  --set config.server.public_url=https://mayfly.example.com
```

The chart creates the control-plane and agent ServiceAccounts, RBAC, ConfigMaps for skills and agent manifests, Service, optional Ingress, and optional LiteLLM deployment. Set `image.digest` and `agentImage.digest` for immutable images and `imagePullSecrets` for private registries. See [../deploy/helm/mayfly/README.md](../deploy/helm/mayfly/README.md).

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
- `configmap.yaml`: `runtime.image`, `server.public_url`, `server.internal_url`, LLM, MCP, agents, triggers, artifact store.
- `serviceaccount.yaml` / `rbac.yaml`: namespace and permissions if you do not use `mayfly`.
- `service.yaml`: use `ClusterIP` or `LoadBalancer` behind your ingress; NodePort is for the demo.

Kustomize builds must allow skill files outside the overlay directory:

```bash
kubectl kustomize --load-restrictor=LoadRestrictionsNone deploy/k8s/overlays/kind | kubectl apply -f -
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

5. Expose the API through an ingress with TLS and authentication. The admin token is a shared bearer token and is not a substitute for SSO.

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

For GCS interoperability, use the GCS XML endpoint and HMAC keys. Native GCS JSON API support requires a new `artifact.Store`.

The control plane writes and reads bundles. Session pods never receive storage credentials. Downloads still work through `GET /api/v1/sessions/{id}/artifact`; the API streams from the store.

## Operating notes

- Run one control-plane replica. Session records, thread history, and the token index are in memory. Pod adoption after a restart works from ConfigMap and Secret, but pre-restart events are not replayed.
- Remote skill archives require outbound network access from the control plane at startup. If you prefer GitOps, sync the skill repository to a volume and use `skills.dirs` instead; the platform then needs no fetch capability.

For multiple model providers, deploy LiteLLM and point `llm.base_url` at its Service. A starting manifest is in [examples/litellm/deployment.yaml](../examples/litellm/deployment.yaml); see [providers.md](providers.md) for keys, IRSA, and model aliases.

The control plane needs RBAC for pods, pod logs, secrets, configmaps, and networkpolicies in the session namespace. See [rbac.yaml](../deploy/k8s/base/rbac.yaml).
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
| MCP tool missing | Server assigned to session, upstream reachable from control plane, headers resolved |
| No artifact | Runtime logs; close grace; fallback bundle; store credentials |
| Attach fails | `server.public_url` scheme and host; ticket expiry; ingress WebSocket support |
