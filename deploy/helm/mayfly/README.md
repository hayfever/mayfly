# Mayfly Helm chart

Installs the Mayfly control plane and optionally a LiteLLM gateway.

## Prerequisites

- Kubernetes 1.27+
- A CNI that enforces NetworkPolicy
- An agent image at `agentImage.repository:tag`
- An existing Secret with `MAYFLY_API_TOKEN` and any webhook secrets

```bash
kubectl create namespace mayfly

kubectl -n mayfly create secret generic mayfly-secrets \
  --from-literal=MAYFLY_API_TOKEN="$(openssl rand -hex 24)" \
  --from-literal=MAYFLY_WEBHOOK_SECRET="$(openssl rand -hex 24)" \
  --from-literal=OPENAI_API_KEY="sk-..."
```

## Install

```bash
helm install mayfly deploy/helm/mayfly \
  --namespace mayfly \
  --set existingSecret=mayfly-secrets \
  --set config.server.public_url=https://mayfly.example.com
```

The default images are `ghcr.io/hayfever/mayflyd` and `ghcr.io/hayfever/mayfly-agent`, tagged with the chart's `appVersion` (`1.0.0-rc.2` for this chart) when `image.tag` and `agentImage.tag` are empty. Override `image` and `agentImage` for your registry.

Check the token:

```bash
kubectl -n mayfly get secret mayfly-secrets -o jsonpath='{.data.MAYFLY_API_TOKEN}' | base64 -d
```

For local access:

```bash
kubectl -n mayfly port-forward svc/mayfly 8080:8080
```

Set `config.server.public_url=http://localhost:8080` when using port-forward. Ingress and TLS must use the external URL.

## Named API tokens

Set `config.server.tokens` to replace the single token from `MAYFLY_API_TOKEN` with scoped tokens. Put each `secret_env` variable in one Secret, and name that Secret in `controlPlane.tokensSecret`:

```bash
kubectl -n mayfly create secret generic mayfly-tokens \
  --from-literal=MAYFLY_CI_TOKEN="$(openssl rand -hex 24)"
```

```yaml
config:
  server:
    tokens:
      - name: ci
        scopes: [sessions:read, sessions:write]
        secret_env: MAYFLY_CI_TOKEN
controlPlane:
  tokensSecret: mayfly-tokens
```

The chart passes that Secret to the control plane as environment variables. A missing variable stops startup. The token name appears in the audit log. See [security.md](../../../docs/security.md#api-authentication) for the scope of each route.

## Agents and skills

Inline files:

```yaml
skills:
  files:
    incident-triage.md: |
      ---
      name: incident-triage
      description: Triage incidents.
      ---
      Check evidence before recommending action.

agents:
  files:
    triage.yaml: |
      schemaVersion: 1
      name: mayfly/agents
      agents:
        - name: triage
          description: Incident triage.
          skills: [incident-triage]
          runtime:
            config:
              triage: true
```

They are mounted at `/etc/mayfly-skills` and `/etc/mayfly-agents`.

## LiteLLM

```bash
kubectl -n mayfly create secret generic litellm-secrets \
  --from-literal=LITELLM_MASTER_KEY="$(openssl rand -hex 24)" \
  --from-literal=OPENAI_API_KEY="sk-..." \
  --from-literal=ANTHROPIC_API_KEY="sk-ant-..." \
  --from-literal=OPENROUTER_API_KEY="sk-or-..."

# Add the same LITELLM_MASTER_KEY to mayfly-secrets.
kubectl -n mayfly patch secret mayfly-secrets \
  -p "{\"stringData\":{\"LITELLM_MASTER_KEY\":\"$(kubectl -n mayfly get secret litellm-secrets -o jsonpath='{.data.LITELLM_MASTER_KEY}' | base64 -d)\"}}"

helm upgrade --install mayfly deploy/helm/mayfly \
  --namespace mayfly \
  --set existingSecret=mayfly-secrets \
  --set litellm.enabled=true \
  --set config.server.public_url=https://mayfly.example.com
```

LiteLLM model aliases live in `litellm.models`. Mayfly `config.llm.model` and `config.llm.allow_models` must reference those aliases. See [docs/providers.md](../../../docs/providers.md).

For Bedrock on EKS, annotate `litellm.serviceAccount.annotations` with your IRSA role and omit static AWS keys.

## Key values

| Value | Default | Meaning |
|---|---|---|
| `image.repository` | `ghcr.io/hayfever/mayflyd` | Control-plane image |
| `image.digest` | empty | Pin instead of tag |
| `agentImage.repository` | `ghcr.io/hayfever/mayfly-agent` | Agent image |
| `agentImage.digest` | empty | Pin instead of tag |
| `imagePullSecrets` | empty | Secrets for control-plane, LiteLLM, and session images |
| `existingSecret` | empty | Secret with control-plane env |
| `secret.create` | false | Create the Secret from `secret.literals` |
| `config.version` | `1` | Config file format version. `mayflyd` reads version 1 |
| `config.server.public_url` | `http://localhost:8080` | External URL |
| `config.llm.base_url` | empty | OpenAI by default; LiteLLM URL when enabled |
| `config.agents.allowed_images` | empty | Image allowlist for manifests |
| `config.queue.backend` | `bbolt` | `memory`, `bbolt`, or `redis`; see [docs/configuration.md](../../../docs/configuration.md#queue) |
| `persistence.enabled` | true | Put `/var/lib/mayfly` (queue database, turn journal, session records, schedule watermark, skill cache, local artifacts) on a PersistentVolume; disabling loses accepted work, in-flight turns, and the session list across a control-plane restart |
| `persistence.size` | `2Gi` | Volume size |
| `metrics.enabled` | true | Serve Prometheus metrics; see [docs/observability.md](../../../docs/observability.md) |
| `metrics.address` | empty | Separate metrics listen address, such as `:9090`; empty serves `/metrics` on the main port |
| `metrics.prometheusAnnotations` | false | Add scrape annotations to the control-plane pod |
| `metrics.serviceMonitor.enabled` | false | Create a Prometheus Operator `ServiceMonitor`; needs a bearer token Secret or an unauthenticated separate address |
| `ingress.enabled` | false | Create an Ingress |
| `controlPlane.resources` | small | Control-plane resources |
| `litellm.enabled` | false | Deploy LiteLLM |
| `litellm.image.tag` | `v1.110.0` | LiteLLM release tag. The chart rejects a rolling tag such as `main-stable` |

The control plane runs one replica. The live event thread is in memory; accepted work, the turn journal, session records, and artifacts are durable with `bbolt` plus persistence, or with `redis` for the queue. See [docs/deployment.md](../../../docs/deployment.md).

`values.schema.json` validates every value. `helm install` and `helm upgrade` reject a value that is not in the schema, an unknown value, and a value outside its allowed set. There is no replica count to set: the chart pins one replica, because the control plane keeps its state on one `ReadWriteOnce` volume.
