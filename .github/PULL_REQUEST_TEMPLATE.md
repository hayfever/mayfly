## Summary

What behavior changes, and why?

## Verification

- [ ] `gofmt -l cmd internal test e2e`
- [ ] `go vet ./...`
- [ ] `go test ./...`
- [ ] `helm lint deploy/helm/mayfly --set existingSecret=mayfly-secrets`
- [ ] k3d e2e, if lifecycle, Kubernetes, networking, artifacts, or agent image changed

Describe the exact commands or scenario you ran.

## Risk

- Isolation or credential changes:
- Data or artifact format changes:
- API, config, or manifest changes:
- Rollback plan:

## Checklist

- [ ] No credentials, tokens, or customer data in the diff or logs.
- [ ] Docs and changelog updated for user-visible behavior.
- [ ] Lifecycle changes are idempotent and cover failure paths.
- [ ] New configuration is validated and has an example.
