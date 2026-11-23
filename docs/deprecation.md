# Deprecation policy

From 1.0.0, a published interface stays until a major release removes it. This page names what the promise covers, how a deprecation runs, and what it does not cover. The migration guides for a major upgrade are [migrating-to-1.0.md](migrating-to-1.0.md) and [migrating-to-0.2.md](migrating-to-0.2.md).

## The promise

A deprecation starts in a minor release. The element keeps working for the rest of that major line. Removal waits for the next major version, so an element deprecated in 1.x is removed in 2.0.0 at the earliest. A patch release never deprecates and never removes.

Deprecation is not a breaking change. Removal is, and it needs the major version bump. The `api-diff` job reports a removal as a breaking change, so a pull request that removes an element fails until the major version is in the base ref.

## What the promise covers

| Surface | Promise |
|---|---|
| HTTP API at `/api/v1` | Routes, request and response fields, and error `code` values |
| Webhook receivers at `/hooks/{name}` | Route names, accepted bodies, status codes, and response fields |
| `api/openapi.yaml` | Describes the two surfaces above. The spec is the record of what is current |
| Config file keys | Key names, types, and meanings, plus the top-level `version` |
| Helm chart values | Value names, types, and defaults. `values.schema.json` rejects a value that the chart no longer reads |

The webhook receiver contract includes the trigger types (`generic`, `github`, `slack`, `datadog`) and their signature checks.

## CLI

The `mayfly` command is a public surface from 1.0.0, with one exception: its text output is not.

| Frozen | Detail |
|---|---|
| Command names | `run`, `agents`, `ls`, `get`, `events`, `attach`, `send`, `close`, `delete`, `artifact`, `transcript`, `version`, `plugin init`, `plugin check`, `plugin test`, and `help` |
| Flag names and forms | Every flag that `mayfly help` lists. A short form (`-o`) and its long form (`--output`) freeze together |
| Environment variables | `MAYFLY_URL` and `MAYFLY_API_TOKEN` |
| `--json` output | The top-level shape and the field names of every object. A new optional field is additive |

A command with `--json` writes one JSON document to stdout and nothing else. The document is the API object, or the array of API objects, that the command shows, so the HTTP API schema also describes it. `version --json` returns `{"version","commit","date"}`. A consumer ignores a field it does not know, because a minor release can add one.

The promise does not cover the text output. Wording, columns, alignment, ordering, and progress lines change without notice. Parse `--json`, not the text. The promise also does not cover error `message` text; match on the error `code`, as the HTTP API section says.

`cmd/mayfly` generates the surface into `cmd/mayfly/testdata/cli-surface.txt`, and `hack/surface-diff.sh` compares that file against the pull request base. A line that disappears or changes fails the gate unless `CHANGELOG.md` records the break under "Breaking changes".

## What the promise does not cover

- `/internal/v1`, the session runtime protocol. It is versioned with the runtime image, and both ends ship together.
- The web UI. It is not an API, and its routes, markup, and assets change without notice.
- Log text, log fields, and metric names. Read the API for state, not the log. The exception is the `audit` line: its `msg` value and the field names in [security.md](security.md#audit-log) stay stable through 1.x. A minor release can add a field.
- Error `message` text. Match on `error.code`, never on the message.
- Go packages. Packages live under `internal/`, a command directory, or `examples/`. None is a supported library.
- The behavior of a dependency. A provider API change can change what a session sees.

## How to deprecate

1. Mark the element in `api/openapi.yaml` with `deprecated: true` for API surfaces, or add a `Deprecated:` note above the key or value for config and chart surfaces. Keep the element in place.
2. Document the replacement in the same file. A deprecation with no replacement names why the element has no successor.
3. Log one warning per process when the deprecated element is used. Include the element and its replacement. Do not log per request.
4. Add the element under "Deprecated" in `CHANGELOG.md` and repeat it in the release notes. Name the release that removes it.
5. Verify with `hack/api-diff.sh` that the diff reports no breaking change.

Do not add a removal date that a later release cannot honor. The only fixed date is the next major release.

## Current deprecations

| Element | Deprecated in | Replacement | Removal |
|---|---|---|---|
| `MAYFLY_TOKEN` CLI environment variable | 0.2.0 | `MAYFLY_API_TOKEN`. The CLI prints one warning when it reads the alias | 2.0.0 at the earliest |
