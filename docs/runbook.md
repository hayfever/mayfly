# Operator runbook

Routine operations for one Mayfly control plane. The examples use release `mayfly` in namespace `mayfly` and the chart default values. Change both for your install.

## Before you start

- One control plane replica owns every session. Do not run two.
- The state volume at `/var/lib/mayfly` holds the queue database, the session record database, the turn journal, the schedule watermark, local artifact bundles, and the verified skill archive cache. It is the only durable state on the control-plane side.
- Session pods keep their own workspace claims. They survive a control-plane restart and a control-plane upgrade.
- The control plane reads its configuration and the token secrets at startup. A change to either needs a pod restart.
- Export a named API token for the commands below as `MAYFLY_API_TOKEN`. The commands need `sessions:read`, `sessions:write`, `queue:read`, and `queue:write`. The `admin` scope covers all four. The legacy token from `server.token_env` is named `default` and has the `admin` scope.

## Upgrade

1. Read the new chart values that the release notes name, then update your values file.
2. Back up the state volume. See [Backup the state volume](#backup-the-state-volume).
3. Run the upgrade:

```bash
helm upgrade mayfly oci://ghcr.io/<owner>/charts/mayfly --version 1.0.0-rc.2 \
  -n mayfly -f values.yaml
kubectl -n mayfly rollout status deployment/mayfly --timeout=3m
```

4. Check `/readyz`, then check that the session list still answers:

```bash
kubectl -n mayfly port-forward svc/mayfly 8080:8080 &
curl -s -o /dev/null -w '%{http_code}\n' http://localhost:8080/readyz
curl -s -H "Authorization: Bearer $MAYFLY_API_TOKEN" \
  'http://localhost:8080/api/v1/sessions?limit=5'
```

5. Read the startup log when a session is missing from the list. The new control plane adopts running pods. A session whose pod it could not rebuild is marked `failed`.

The deployment uses the `Recreate` strategy, so the old pod stops before the new one starts. Sessions in flight retry their model, tool, and journal calls through the new control plane for up to 5 minutes per step.

A config file with no `version` key is read as version 1 and logs a warning. A config file with an unknown version stops the load with an error that names both versions, and the process exits with code 2. Fix the file, then run `helm upgrade` again or `helm rollback mayfly -n mayfly`.

## Backup the state volume

The state databases are bbolt files. A bbolt file must not be copied while a process writes it. Stop the control plane first.

1. Stop the control plane and wait for the pod to exit:

```bash
kubectl -n mayfly scale deployment/mayfly --replicas=0
kubectl -n mayfly get pods -l app.kubernetes.io/component=control-plane
```

Repeat the second command until it lists no pod.

2. Start one pod that mounts the state claim. The claim is `<release fullname>-state`, and `mayfly-state` here:

```bash
cat <<'EOF' | kubectl -n mayfly apply -f -
apiVersion: v1
kind: Pod
metadata:
  name: state-copy
spec:
  restartPolicy: Never
  securityContext:
    runAsUser: 65532
    runAsGroup: 65532
    fsGroup: 65532
  containers:
    - name: copy
      image: busybox:1.36
      command: ["sleep", "3600"]
      volumeMounts:
        - name: state
          mountPath: /state
  volumes:
    - name: state
      persistentVolumeClaim:
        claimName: mayfly-state
EOF
kubectl -n mayfly wait --for=condition=Ready pod/state-copy --timeout=120s
```

3. Copy the whole directory to your workstation. The archive holds `queue.db`, `sessions.db`, `journal.db`, `schedule.db`, `skill-cache/`, and, when the artifact store is `local`, `artifacts/`:

```bash
kubectl -n mayfly exec state-copy -- tar -C /state -czf - . \
  > mayfly-state-$(date -u +%Y%m%dT%H%M%SZ).tgz
```

4. Check that the archive is readable and not empty:

```bash
tar -tzf mayfly-state-*.tgz >/dev/null
```

5. Remove the pod and start the control plane:

```bash
kubectl -n mayfly delete pod state-copy
kubectl -n mayfly scale deployment/mayfly --replicas=1
kubectl -n mayfly rollout status deployment/mayfly --timeout=3m
```

6. Store the archive off the cluster. The volume size is `persistence.size`.

The archive does not include the per-session workspace claims (`mayfly-<id>-workspace`). A session that is running when you take the backup can resume only while its claim and pod still exist. Export the artifact of a session you need before you delete its claim. An S3 artifact store is outside this archive; back it up with the storage provider.

Scheduled source runs and webhook jobs reference session ids. Restore the four databases from one backup, not from different backups.

## Restore the state volume

1. Scale the control plane to zero and start the copy pod, as in steps 1 and 2 of [Backup the state volume](#backup-the-state-volume).
2. Remove the current files and unpack the archive:

```bash
kubectl -n mayfly exec state-copy -- sh -c 'rm -rf /state/*'
kubectl -n mayfly exec -i state-copy -- tar -C /state -xzf - < mayfly-state-20261118T120000Z.tgz
kubectl -n mayfly exec state-copy -- ls -l /state
```

3. Delete the copy pod, then scale the control plane to one replica.
4. Check `/readyz` and `GET /api/v1/sessions`. A record that the build cannot read stops startup and names the file. Use a backup from the same release line, or restore only the databases that you need and leave the rest empty.

5. Check the sessions that were running at backup time. The archive does not include their workspace claims, and a full cluster loss also removes their pods. A restored session that cannot be adopted is listed as `failed` and keeps its record until you delete it or `sessions.retention` expires.

A bbolt file from a newer build can fail the layout check of an older build. Restore the same build that wrote the archive. A control plane that reads a session record with a newer `format_version` refuses to start; do not downgrade after an upgrade writes the newer format.

## Rotate an API token

Named tokens are configured under `server.tokens`, and each token reads its secret from the environment variable that `secret_env` names. The Helm chart loads those variables from the Secret that `controlPlane.tokensSecret` names. The legacy token from `server.token_env` is one token named `default` with the `admin` scope, and the chart reads it from `existingSecret`.

1. Generate a new value and write it to the Secret key for the token. This example rotates a token named `ci` whose `secret_env` is `MAYFLY_CI_TOKEN`. Use the key that your `server.tokens` entry names:

```bash
NEW_TOKEN="$(openssl rand -hex 32)"
kubectl -n mayfly patch secret mayfly-tokens --type merge \
  -p "{\"stringData\":{\"MAYFLY_CI_TOKEN\":\"${NEW_TOKEN}\"}}"
```

For the legacy default token, patch the key in `existingSecret` that `server.token_env` names. The default key is `MAYFLY_API_TOKEN`.

2. Restart the control plane, then check the new and old values:

```bash
kubectl -n mayfly rollout restart deployment/mayfly
kubectl -n mayfly rollout status deployment/mayfly --timeout=3m
curl -s -o /dev/null -w '%{http_code}\n' \
  -H "Authorization: Bearer ${NEW_TOKEN}" http://localhost:8080/api/v1/sessions
curl -s -o /dev/null -w '%{http_code}\n' \
  -H "Authorization: Bearer ${OLD_TOKEN}" http://localhost:8080/api/v1/sessions
```

The new value returns `200`. The old value returns `401` after the restart. Give the new value to every client.

3. Rotate a webhook secret the same way. The key is the one that `triggers[].secret_env` names. Restart, then update the sender, or its deliveries fail verification.

Session pods keep their own session tokens in their own Secret. Rotating an API token does not touch them.

## Sign in and sign out of the web UI

The web UI exchanges the API token for a browser session once. The token is not stored in the browser.

1. Open the control-plane URL, paste the token in Settings, and submit. The control plane sets the `mayfly_session` cookie (HttpOnly, SameSite=Strict, Secure over https) and the `mayfly_csrf` cookie. The session lasts 12 hours, or less when the token's `expires_at` is earlier.
2. Use the Sign out control to end the session. `DELETE /api/v1/auth/session` expires both cookies. A mutation from the browser must send `X-Mayfly-CSRF` with the `mayfly_csrf` value; the UI adds the header.
3. Sign in again after a control-plane restart. Sessions live in the process and not on the state volume, so a restart ends every one. The chart runs one replica.

A shell or CI client that sends `Authorization: Bearer` needs no cookie and no CSRF header.

## Triage dead letters

A job becomes `dead` after `queue.max_attempts` attempts (default 5). The job is kept until you retry or delete it.

1. List the buried jobs:

```bash
curl -s -H "Authorization: Bearer $MAYFLY_API_TOKEN" \
  'http://localhost:8080/api/v1/queue/dead?limit=50' | jq '.items[] | {id, trigger, attempts, error, failed_at}'
```

2. Read the full record of one job, including its prompt:

```bash
curl -s -H "Authorization: Bearer $MAYFLY_API_TOKEN" \
  'http://localhost:8080/api/v1/queue/jobs/<job_id>' | jq
```

3. Fix the cause. A common cause is `unknown agent "..."`, which means the agent manifest was not loaded at startup, or `model not in allow_models`. Both need a config or bundle change and a restart.

4. Retry the job, or drop it:

```bash
curl -s -X POST -H "Authorization: Bearer $MAYFLY_API_TOKEN" \
  'http://localhost:8080/api/v1/queue/dead/<job_id>/retry'
curl -s -X DELETE -H "Authorization: Bearer $MAYFLY_API_TOKEN" \
  'http://localhost:8080/api/v1/queue/dead/<job_id>'
```

Retry resets the attempt count. Delete drops the job without running it. A retried job that fails again comes back as `dead`.

A dead job whose prompt is stale and whose trigger is noisy is a candidate for delete. A dead job for a source connection usually needs the connection fixed first.

## Triage a recovered panic

A recovered panic is a bug, not a normal retry. The control plane stays up and writes an error line with a stack trace. A queue job is Nacked and retried with backoff. Background session work stops for that goroutine, and shutdown still completes.

1. Confirm the component and the rate:

```bash
curl -s http://localhost:9090/metrics | grep '^mayfly_panics_total'
kubectl -n mayfly logs deployment/mayfly --since=15m | grep -E 'job handler panicked|session background work panicked'
```

2. Read the `stack` field. For a queue job, note `job`, `kind`, `trigger`, and `attempt`. The stack names the handler or source projection that panicked.

3. If the job is now `dead`, follow [Triage dead letters](#triage-dead-letters). Fix the cause before you retry. A retry of the same input panics again. A dead source job whose error contains `internal error` came from a source projection panic: check the source definition and the upstream page before you retry.

4. If the component is `session`, the log has no session id. Use the stack's method name and the nearby session records to find the session. If the component is `http`, the log has the session id and the WebSocket connection is closed. Do not restart the control plane to clear the log. The panic is already contained.

5. If the rate stays above zero for one handler or source, disable that schedule or trigger, capture the stack, and report the input. A restart does not fix a poison input.

## Clear a stuck session

A session is stuck when its status stays `pending` or `running` and the thread stops.

1. Read the record and the events:

```bash
curl -s -H "Authorization: Bearer $MAYFLY_API_TOKEN" \
  'http://localhost:8080/api/v1/sessions/<id>' | jq '.session | {status, error, created_at}'
curl -s -H "Authorization: Bearer $MAYFLY_API_TOKEN" \
  'http://localhost:8080/api/v1/sessions/<id>/events?after=0' | jq '.events[-5:]'
```

2. Read the pod. The pod is `mayfly-<id>` and the agent container is `agent`:

```bash
kubectl -n mayfly get pod mayfly-<id>
kubectl -n mayfly logs mayfly-<id> -c agent --tail=100
kubectl -n mayfly describe pod mayfly-<id>
```

3. Try a graceful close first. The runtime exports its bundle and exits:

```bash
curl -s -X POST -H "Authorization: Bearer $MAYFLY_API_TOKEN" \
  -H 'Content-Type: application/json' -d '{"reason":"operator close"}' \
  'http://localhost:8080/api/v1/sessions/<id>/close'
```

4. Force-stop a session that does not close within `sessions.close_grace`:

```bash
curl -s -X DELETE -H "Authorization: Bearer $MAYFLY_API_TOKEN" \
  'http://localhost:8080/api/v1/sessions/<id>'
```

Delete stops the pod, marks the session `failed`, and keeps the record until `sessions.retention` expires. The artifact of an unclosed session is not available, because no runtime uploaded a bundle.

5. Check that the session objects are gone:

```bash
kubectl -n mayfly get pods,configmaps,secrets -l mayfly.io/session=<id>
```

Do not delete the session ConfigMap or Secret by hand. Session resources are labelled `mayfly.io/managed=true`. The control plane reads the ConfigMap and Secret to adopt a running pod, and it deletes a pod whose ConfigMap is gone.

## Restart the control plane

1. Restart the deployment:

```bash
kubectl -n mayfly rollout restart deployment/mayfly
kubectl -n mayfly rollout status deployment/mayfly --timeout=3m
```

2. Read the startup log for adoption:

```bash
kubectl -n mayfly logs deployment/mayfly --tail=100 | grep -i adopt
```

SIGTERM is not a close. Each runtime stops its turn, exits non-zero, and the new control plane starts a replacement pod or adopts the live pod. A session whose pod could not be rebuilt is marked `failed`, because no runtime can continue its turn.

3. Check `/readyz` and the session list. `/readyz` returns `503` while the process drains, so a failing probe during a restart is expected.

A control-plane restart loses thread history in memory, the token index, and every web UI session. The journal keeps the last turn, and the runtime keeps `conversation.jsonl`. The artifact bundle is the durable history. Browsers sign in again after the restart; see [Sign in and sign out of the web UI](#sign-in-and-sign-out-of-the-web-ui).
