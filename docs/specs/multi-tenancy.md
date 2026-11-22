# Multi-tenancy: plan

Status: Proposed; not scheduled. Single-tenant installs are the supported model through 1.x.
Audience: Mayfly maintainers and operators running Mayfly for several clients

## Summary

Mayfly supports one install per client. This plan keeps that model through 1.x, lists the preparation that makes a later migration cheap, and describes a target design for real multi-tenancy in a future major version. The work is not scheduled.

## Supported model (1.x)

Run one Helm release per client on a shared cluster. Each release has its own namespace, control plane, Secrets, state volume, and session quota. Clients do not share a control plane.

| Item | Per client |
|---|---|
| Helm release | One, in its own namespace |
| Control plane | One `mayflyd` pod; one replica, no autoscaling |
| Namespaces | `sessions.namespace` and `plugins.namespace`, created before install |
| Secrets | `existingSecret` with the client's API token, webhook secrets, and upstream keys |
| Quota | `sessions.max_concurrent` set to 3 to 5 |
| Runtime capacity | Added by the cluster autoscaler as session pods become pending |

The chart does not create namespaces. Create the client namespace first, then install. Keep one control plane replica: `bbolt` locks its files, and thread buffers and the token index live in process memory. [architecture.md](../architecture.md#concurrency) has the details.

Set `sessions.max_concurrent` low per client. Session pods are short-lived, so the cluster autoscaler can add nodes as work arrives and remove them when the queue is empty. Do not autoscale the control plane.

### Operational guidance

The control plane, queue, journal, and session record store belong to one release. Do not point two clients at one queue or one state volume. A client's API tokens reach every session in that client's install. That is acceptable inside one client and not between clients.

## Preparation now

Three habits keep a later migration cheap:

1. Keep every client's configuration in Git: Helm values, agent manifests, triggers, and schedules. Treat the values file and the bundle files as the client's source of truth.
2. Give each client a stable hook URL on our domain, for example `https://hooks.<domain>/<client>`. The URL is an ingress or proxy mapping to the release's `POST /hooks/<name>`. A backend move changes the mapping, not the client's GoHighLevel workflows.
3. Store artifacts under a per-client prefix. Set `artifacts.s3.prefix` per release, for example `<client>/`, or use a separate `artifacts.local.dir` when the local store is in use.

### Operational guidance

The stable hook URL is a routing convention, not a Mayfly feature. Do not add a second webhook route. Point the proxy at the trigger name that the release already serves.

## When to build multi-tenancy

Build it when any one of these triggers is true:

- About 50 or more clients.
- Self-serve signups, where a client creates an install without an operator.
- Hosting Mayfly for other companies.

Until then, the cost of one release per client is lower than the cost of a tenant boundary that has not been exercised.

## Target design

Target: a future major version. A single-tenant install keeps working as a `default` tenant, so the upgrade does not force a migration on day one.

### Tenant object

A `Tenant` object names one client and owns its configuration and quota. Tokens, triggers, schedules, agents, sessions, artifacts, and queue jobs carry a tenant reference. A request without a tenant reference resolves to `default`.

### Namespace and RBAC

One namespace per tenant, labeled `mayfly.dev/tenant`. The control plane holds RBAC across the tenant namespaces, and the session ServiceAccount stays inside its tenant namespace. Namespace labels drive policy selection and cleanup, not authorization alone.

### Secrets

Each tenant's Secrets stay in its own namespace and are read only when that tenant's session starts. The control plane does not load every tenant's credentials at startup. The credential proxy and the tool gateway use per-tenant credentials, so one tenant's upstream key is never presented for another tenant's call.

### Isolation boundary

A compromised control plane can still reach every tenant: it can read tenant Secrets and call tenant workloads. The goal is narrower. A tenant's session, token, or workload must not reach another tenant's session, Secrets, artifacts, or queue state. State this limit in [security.md](../security.md) when the design is implemented.

### Network and quota

Each tenant namespace gets a default-deny NetworkPolicy, an egress allowlist, a `ResourceQuota`, and a session cap. The session cap becomes the per-tenant form of `sessions.max_concurrent`. NetworkPolicy stays CIDR-based, as it is today.

### Usage ledger

Per-tenant usage goes in a ledger. Do not add a tenant label to Prometheus metrics: labels stay bounded, as they are today. The ledger records sessions, model calls, tool calls, and artifact bytes per tenant.

### Tokens and joins

Tokens become tenant-scoped, with an admin scope across all tenants. A session join checks the session's tenant against the token's tenant. Today scopes apply across every session in the install ([security.md](../security.md#holder-of-a-scoped-api-token)); this changes that.

### Tests

End-to-end tests must prove that one tenant cannot reach another: no cross-tenant Secret read, no cross-tenant session attach, no cross-tenant artifact download, and no network path between tenant session pods.

### Documentation

Update [security.md](../security.md) when the design lands. That page currently describes one deployment in one namespace. The update is future work.

## Migration from N installs

Run the multi-tenant install next to the old ones. Move one client at a time.

1. Create the client's tenant and namespace in the new install.
2. Copy the client's Secrets into the tenant namespace.
3. Export the Helm values and bundle files from Git and import them as the tenant's config.
4. Copy the client's artifacts to the tenant prefix.
5. Repoint the stable hook URL to the new install.

Keep the old release as a fallback for a few days. Then run `helm uninstall` in the old namespace. Start with the smallest clients, a few per week, and keep the old installs until the new path has handled real traffic.

## Out of scope and open questions

Not part of this plan:

- Billing, invoicing, and per-tenant model budgets.
- A self-serve signup, provisioning, or upgrade flow.
- Hostname-based egress allowlists. NetworkPolicy remains CIDR-based.
- Multi-replica control planes.
- Per-user isolation inside one tenant.

Open questions:

- What is the quota unit: concurrent sessions, model tokens, tool calls, or a mix?
- Does a tenant map to one namespace or to a pair of namespaces, as `sessions.namespace` and `plugins.namespace` do today?
- How are existing bundles re-keyed when a client moves, and how long must the old prefix stay readable?
- Does the admin scope include attaching to a session, or only reading and deleting across tenants?
- How does a tenant get deleted without leaving Secrets, artifacts, or queue jobs behind?
