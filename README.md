# tenant-rbac-controller

Kubernetes operator that watches `Tenant` CRDs and reconciles the full tenant
environment: namespace, RBAC, NetworkPolicy, ResourceQuota, LimitRange,
IRSA-annotated ServiceAccount, ExternalSecret for tenant secrets, ArgoCD
AppProject + Application for the tenant's deploy repo, default Ingress.

Replaces the v1 Helm-rendered tenant onboarding from
[Project 9.3](../projects/3-aws-mtkp-argocd-and-platform-bootstrap/05-tenant-onboarding-v1/).

## Quick start

```bash
make manifests       # generate CRDs and RBAC manifests
make generate        # generate deepcopy
make test            # envtest unit tests
make docker-build IMG=ghcr.io/example/tenant-rbac-controller:dev
make install         # install CRDs into the current kube context
make run             # run the manager locally (against current context)
```

## Custom resource

```yaml
apiVersion: platform.mtkp.platform/v1alpha1
kind: Tenant
metadata:
  name: payments
spec:
  displayName: Payments
  namespace: payments
  owners:
    - kind: Group
      name: my-org:payments-team
      apiGroup: rbac.authorization.k8s.io
  quotas:
    cpu: "20"
    memory: "40Gi"
    pods: "100"
  secrets:
    - name: payments-db-creds
      remoteRef: platform/prod/payments/db-creds
  ingress:
    host: payments.mtkp.example.com
    className: alb
  irsaRoleArn: arn:aws:iam::123456789012:role/mtkp-prod-use1-payments
  deployRepo: https://github.com/my-org/tenant-deploy-payments.git
  deployRef: main
```

## Status conditions

The operator reports per-concern status: `NamespaceReady`, `RBACReady`,
`NetworkPolicyReady`, `QuotaReady`, `SecretsReady`, `IngressReady`,
`AppProjectReady`, and an aggregate `Ready`.

## Webhook

A pair of admission webhooks (defaulter + validator) run alongside the
controller on port `9443`. They reject invalid `Tenant` objects at
admission time so misconfigurations never reach etcd, and apply sensible
defaults so the reconciler can treat optional fields as populated.

### Defaulter

Applied on CREATE and UPDATE:

- `spec.namespace` defaults to `metadata.name`.
- `spec.deployRef` defaults to `"main"`.
- `spec.quotas.pods` defaults to `"100"` when unset.

### Validator

Applied on CREATE and UPDATE (DELETE is always allowed):

1. `spec.namespace` must be a valid DNS-1123 label
   (`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`, max 63 chars).
2. `spec.namespace` must not be one of the platform-reserved namespaces
   (`kube-system`, `kube-public`, `kube-node-lease`, `argocd`,
   `external-secrets`, `cert-manager`, `calico-system`, `karpenter`,
   `monitoring`, `velero`, `tenant-rbac-system`,
   `secret-distribution-system`).
3. `spec.quotas.cpuLimits` parses as a `resource.Quantity` and is ≤ 100
   cores (catches `"100000"` typos).
4. `spec.quotas.memoryLimits` parses as a `resource.Quantity` and is ≤
   500Gi.
5. `spec.quotas.pods` parses as an integer and is ≤ 1000.
6. Each `spec.secrets[*].remoteRef` must match
   `^platform/(dev|staging|prod|prod-dr)/<namespace>/<key>$` (the same
   policy the `secret-distribution-operator` enforces) AND the
   `<namespace>` segment must equal `spec.namespace`.
7. `spec.ingress.host` (if set) must end with one of
   `.caas.example.com`, `.dev.caas.example.com`,
   `.staging.caas.example.com`, `.dr.caas.example.com`.
8. `spec.irsaRoleArn` (if set) must match
   `^arn:aws[a-z-]*:iam::[0-9]{12}:role/`.
9. `spec.deployRepo` must be an `https://github.com/<org>/<repo>(.git)?`
   URL — GitLab/Bitbucket sources are not approved.

### Deployment

- Serving cert is provisioned by `cert-manager` via the `Certificate` in
  `config/certmanager/`. The Helm chart wires this automatically when
  `webhook.enabled=true`.
- `failurePolicy: Fail` — admission blocks if the webhook is down. This
  is a security boundary, not a best-effort hook.
- `sideEffects: None` — the webhook only validates / mutates the
  incoming object.
- `timeoutSeconds: 10`.

Run the webhook unit + envtest suite with:

```bash
make webhook-test
```

## Helm

```bash
helm install tenant-rbac-controller ./helm-chart/tenant-rbac-controller \
  --namespace tenant-rbac-system \
  --create-namespace \
  --set image.tag=v0.1.0
```

## Metrics

Exposed on `:8443/metrics` (ServiceMonitor-friendly):

- `tenant_reconcile_duration_seconds{tenant,result}`
- `tenant_reconcile_errors_total{tenant,reason}`
- Plus the controller-runtime defaults.

## Architecture

See [Project 9.5 chapters](../projects/5-aws-mtkp-custom-kubernetes-operators/03-tenant-rbac-controller/)
for the full design walkthrough.

## Releasing

Releases are driven by Git tags of the form `vX.Y.Z`. The
`.github/workflows/release.yml` workflow runs in three stages:

1. **verify** — confirms the tag matches `VERSION` (if present) and that
   `CHANGELOG.md` has a matching `## vX.Y.Z` heading. The matching section
   is extracted to `release-notes.md`.
2. **release** — runs [GoReleaser v2](https://goreleaser.com) against
   [`.goreleaser.yml`](.goreleaser.yml): cross-compiles linux + darwin x
   amd64 + arm64 binaries, builds & pushes multi-arch images to
   `ghcr.io/example/tenant-rbac-controller`, generates SPDX-JSON SBOMs
   for every archive and image (via [syft](https://github.com/anchore/syft)),
   signs every image with [cosign](https://github.com/sigstore/cosign)
   keyless (Fulcio + Rekor) using the GitHub OIDC token, computes
   SHA256 checksums, and publishes a GitHub release with everything
   attached.
3. **helm** — packages and pushes the Helm chart to
   `oci://ghcr.io/<owner>/charts`.

Validate the GoReleaser config locally with:

```bash
goreleaser check
goreleaser release --snapshot --clean   # dry run; no push, no sign
```

## Supply chain

The release pipeline is fully [SLSA-aligned](https://slsa.dev/):

- **Signed images.** No private keys exist anywhere. cosign requests a
  short-lived signing certificate from Fulcio that binds the GitHub
  Actions OIDC identity, signs the image, and writes the signature to
  the Rekor transparency log. Verification recomputes the binding.
- **SBOMs.** One SPDX-JSON SBOM per release archive and per docker
  image is attached to the GitHub release as an artifact.
- **Provenance.** GoReleaser embeds build metadata (`-X main.version`,
  `-X main.commit`, `-X main.date`) and the docker images carry
  `org.opencontainers.image.{source,revision,version}` labels.

### Verifying an image

```bash
cosign verify ghcr.io/example/tenant-rbac-controller:v1.0.0 \
  --certificate-identity-regexp '^https://github\.com/example/tenant-rbac-controller/\.github/workflows/release\.yml@refs/tags/v.*$' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

The `--certificate-identity-regexp` pins the signer to the
`release.yml` workflow on a `v*` tag in this repo. Any other identity —
a different workflow, a different repo, a non-tag ref — fails
verification. No public key is stored; trust comes from Fulcio + Rekor.

### Consuming the SBOM

```bash
gh release download v1.0.0 \
  --repo example/tenant-rbac-controller \
  --pattern '*.spdx.json'

# Or attach the image SBOM to the image itself (one-time, signer-side):
syft attest ghcr.io/example/tenant-rbac-controller:v1.0.0 \
  --output spdx-json | cosign attest --yes \
  --predicate /dev/stdin \
  --type spdxjson \
  ghcr.io/example/tenant-rbac-controller:v1.0.0
```
