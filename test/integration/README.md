# tenant-rbac-controller integration tests

Real-cluster integration test for the Tenant reconciler. Boots a Kind cluster,
installs CRDs (operator's own + external-secrets + ArgoCD), runs the controller
in-process against the cluster, applies a sample `Tenant`, and asserts the
downstream resources materialise correctly.

This layer complements the envtest unit tests in `internal/controller/` — it
catches things envtest can't: admission chain behaviour, real CRD installation
order, real namespace finalisation, real RBAC enforcement on the API server.

## Run locally

Prereqs:

- Docker daemon running
- `kubectl` on PATH
- Go 1.22+
- ~3 GB free RAM for the Kind node container

From the repo root:

```sh
make test-integration
```

That maps to:

```sh
go test -tags=integration -timeout 20m -v ./test/integration/...
```

The `integration` build tag gates this suite. Regular `go test ./...` skips it
so PR unit-test runs stay fast.

## What it covers

The single test `TestTenantE2E` walks the full lifecycle:

1. Apply `testdata/sample-tenant.yaml` (the `payments` tenant).
2. Wait up to 90 s for all eight status conditions to become `True`:
   `NamespaceReady`, `RBACReady`, `NetworkPolicyReady`, `QuotaReady`,
   `SecretsReady`, `IngressReady`, `AppProjectReady`, and aggregate `Ready`.
3. Assert each downstream resource exists with the expected shape:
   - `Namespace` `payments` with the standard labels
   - `ServiceAccount` `tenant` with the IRSA annotation
   - `RoleBinding` `tenant-owners` for the owner Subject
   - Three NetworkPolicies (`default-deny-all`, `allow-within-namespace`,
     `allow-platform-ingress`)
   - `ResourceQuota` `tenant-quota` matching the spec
   - `ExternalSecret` per `spec.secrets[]`
   - ArgoCD `AppProject` named after the tenant in the `argocd` namespace
   - ArgoCD `Application` pointing at the tenant's `deployRepo`
4. Mutate the Tenant (bump `cpuRequests`) and confirm the ResourceQuota
   reconciles within 30 s.
5. Delete the Tenant and confirm the finalizer-driven cleanup removes the
   namespace within 60 s.

## Pinned versions

Bumping any of these is a deliberate, reviewable change. They live as
constants in `suite_test.go`.

| Component          | Version                  |
|--------------------|--------------------------|
| Kubernetes (Kind)  | `v1.29.4` (`kindest/node:v1.29.4`) |
| external-secrets   | `v0.9.20`                |
| ArgoCD             | `v2.11.7`                |

## Why in-process, not container image?

The suite runs the controller in-process via `manager.New(...).Start(ctx)`,
not as a Pod inside the cluster. Reasons:

- The operator codepath is identical (same `manager.New` call as `main.go`).
- Saves ~3 minutes of CI per run (no `docker build` + `kind load docker-image`).
- The container build is verified separately by the Helm chart's CI workflow.

If a future regression turns out to be image-specific (entrypoint, base image,
permissions), add a second test that does `kind load docker-image` + `helm
install` rather than replacing this one.

## Skipping the suite

Set `SKIP_INTEGRATION=1` to exit immediately from `TestMain`. Useful when
running `go test -tags=integration ./...` on a machine that doesn't have
Docker.

## Cached downloads

Upstream CRDs are downloaded once and cached in `testdata/`. Re-runs use the
cache; delete the files to force a re-fetch. The cache files are gitignored.
