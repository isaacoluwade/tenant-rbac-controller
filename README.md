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
