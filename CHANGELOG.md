# Changelog

All notable changes to `tenant-rbac-controller` are documented here. Format
based on [Keep a Changelog](https://keepachangelog.com/); versioning follows
[SemVer](https://semver.org/).

This operator is pre-1.0. The API may break between minor versions; we will
ship a 1.0.0 once the `Tenant` CRD is stable enough that downstream consumers
can pin without expecting churn.

## [0.1.0] - 2026-05-24

### Added

- **Tenant CRD** (`api/v1alpha1/tenant_types.go`). Spec fields: `displayName`,
  `namespace`, `owners`, `quotas`, `secrets`, `ingress`, `irsaRoleArn`,
  `deployRepo`, `deployRef`. Status carries 7 per-concern conditions
  (`NamespaceReady`, `RBACReady`, `NetworkPolicyReady`, `QuotaReady`,
  `SecretsReady`, `IngressReady`, `AppProjectReady`) plus an aggregate `Ready`
  and a `Phase`.
- **Reconciler** that materializes a full tenant environment from each Tenant
  CR: `Namespace` with labels, `ResourceQuota` + `LimitRange`,
  default-deny + allow-within-namespace + allow-platform-ingress
  `NetworkPolicy`, IRSA-annotated `ServiceAccount`, `RoleBinding` for owner
  subjects, `ExternalSecret` per declared secret, default `Ingress` (when
  `spec.ingress` is set), ArgoCD `AppProject`, ArgoCD `Application` watching
  the tenant's deploy repo.
- **Finalizer** (`platform.caas/tenant-finalizer`) ensures clean deletion of
  the namespace cascade.
- **Prometheus metrics** registered to controller-runtime's metrics registry:
  `tenant_reconcile_duration_seconds{tenant,result}` and
  `tenant_reconcile_errors_total{tenant,reason}`.
- **Defaulting + Validating admission webhooks** for the Tenant CRD
  (`api/v1alpha1/tenant_webhook.go`). The validator rejects invalid Tenants
  at admission time, before they're persisted to etcd. Rules:
  - `spec.namespace` is a valid DNS-1123 label and not in the platform's
    reserved-namespace blocklist.
  - `spec.quotas.cpu` ≤ 100 cores; `spec.quotas.memory` ≤ 500Gi;
    `spec.quotas.pods` ≤ 1000 (catches `100000`-typo class of bugs).
  - `spec.secrets[*].remoteRef` matches the platform-path pattern and its
    namespace component equals `spec.namespace`.
  - `spec.ingress.host` ends with one of the platform's apex domains.
  - `spec.irsaRoleArn` is a well-formed IAM role ARN.
  - `spec.deployRepo` is a github.com URL (no Bitbucket / GitLab).
  Defaulter: `spec.namespace` falls back to `metadata.name`; `spec.deployRef`
  to `"main"`; `spec.quotas.pods` to `"100"`. 19 table-driven tests cover
  every rule + the happy path.
- **Kind-based real-cluster integration test** (`test/integration/`).
  `TestTenantE2E` boots a Kind cluster (Kubernetes 1.29.4), installs the
  upstream external-secrets and ArgoCD CRDs at pinned versions, runs the
  controller in-process against the cluster, applies a sample Tenant, and
  asserts all 8 status conditions reach `True` within 90 seconds. Exercises
  mutation (quota bump → ResourceQuota reconciles within 30s) and deletion
  (finalizer drains the namespace within 60s). Gated by the `integration`
  Go build tag so PR unit-test runs stay fast; `make test-integration`
  runs the suite locally, `.github/workflows/integration-test.yml` runs it
  on every PR.
- **Supply-chain release tooling**: `.goreleaser.yml` builds multi-arch
  binaries (linux/amd64, linux/arm64, darwin/amd64, darwin/arm64), pushes a
  multi-arch container image to `ghcr.io/example/tenant-rbac-controller`,
  signs the image with `cosign` keyless (via GitHub OIDC), generates SPDX
  SBOMs for both archives and image, and attaches everything to the GitHub
  release. `.github/workflows/release.yml` runs goreleaser, then pushes the
  Helm chart to `oci://ghcr.io/example/charts`. Dockerfile carries
  OCI labels (`org.opencontainers.image.{source,revision,version}`).
- **Helm chart** (`helm-chart/tenant-rbac-controller/`) for deployment with
  CRDs, ClusterRole, ClusterRoleBinding, ServiceAccount (IRSA annotations
  exposed via `serviceAccount.annotations`), Deployment, Service,
  ServiceMonitor, plus webhook ValidatingWebhookConfiguration +
  cert-manager Certificate when `webhook.enabled = true`.

### Module contract (initial)

- CRD group: `platform.caas.platform/v1alpha1`
- Kind: `Tenant`
- Required spec fields: `displayName`, `owners`, `quotas`, `deployRepo`
- Optional spec fields: `namespace`, `secrets`, `ingress`, `irsaRoleArn`, `deployRef`
- Status conditions: 7 per-concern + aggregate `Ready`
- Finalizer: `platform.caas/tenant-finalizer`
- Webhook port: 9443 (cert at `/tmp/k8s-webhook-server/serving-certs`)
- Metrics port: 8443
- Probes port: 8081

[0.1.0]: https://github.com/example/tenant-rbac-controller/releases/tag/v0.1.0
