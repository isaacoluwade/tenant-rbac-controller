// Package v1alpha1 — webhook plumbing for the Tenant CRD.
//
// This file wires the validating + mutating admission webhooks for Tenant.
// The validator runs at admission time so invalid Tenants are rejected
// before they hit etcd, instead of being persisted and surfaced later by
// the reconcile loop. The mutator (defaulter) fills in safe defaults for
// optional fields so the rest of the platform can assume them populated.
//
// Webhook registration uses the standard kubebuilder/controller-runtime
// admission package — NOT a raw net/http handler — so we get marshalling,
// patch generation, and SSA semantics for free.
package v1alpha1

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// tenantWebhookLog is the logger used by webhook handlers. controller-runtime
// gives us a per-call logger via the context, but the setup path doesn't have
// one yet, so we keep a package-level handle for SetupWebhookWithManager.
var tenantWebhookLog = logf.Log.WithName("tenant-webhook")

// dns1123LabelRegex matches the same DNS-1123 label rule the Kubernetes API
// server applies to namespace names. We re-implement it here so the webhook
// can reject malformed namespace fields without the user having to wait for
// the namespace create call later in the reconcile loop.
var dns1123LabelRegex = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// remoteRefRegex enforces the platform-path policy used by the
// secret-distribution-operator: `platform/<env>/<namespace>/<key>`.
// Keeping this regex in lock-step with that operator means a Tenant can't
// reference a path the secret operator would reject.
var remoteRefRegex = regexp.MustCompile(`^platform/(dev|staging|prod|prod-dr)/[a-z0-9-]+/[a-z0-9-]+$`)

// irsaRoleArnRegex matches the AWS IAM role ARN shape, allowing partition
// variants (aws, aws-cn, aws-us-gov, …) since the platform runs in
// multiple partitions.
var irsaRoleArnRegex = regexp.MustCompile(`^arn:aws[a-z-]*:iam::[0-9]{12}:role/`)

// deployRepoRegex pins the deploy repo to github.com — GitLab/Bitbucket are
// not approved sources for tenant manifests today.
var deployRepoRegex = regexp.MustCompile(`^https://github\.com/[a-zA-Z0-9_.-]+/[a-zA-Z0-9_.-]+(\.git)?$`)

// reservedNamespaces are namespaces a tenant must never claim. They are
// owned by the platform: kubelet kernel namespaces, GitOps machinery,
// platform operators, and the platform's own monitoring/backup tooling.
var reservedNamespaces = map[string]struct{}{
	"kube-system":                  {},
	"kube-public":                  {},
	"kube-node-lease":              {},
	"argocd":                       {},
	"external-secrets":             {},
	"cert-manager":                 {},
	"calico-system":                {},
	"karpenter":                    {},
	"monitoring":                   {},
	"velero":                       {},
	"tenant-rbac-system":           {},
	"secret-distribution-system":   {},
}

// allowedIngressSuffixes are the platform's apex domains. Tenants can claim
// any subdomain ending in one of these — anything else is rejected so a
// tenant can't accidentally (or maliciously) advertise an Ingress for a
// domain the platform doesn't own.
var allowedIngressSuffixes = []string{
	".caas.example.com",
	".dev.caas.example.com",
	".staging.caas.example.com",
	".dr.caas.example.com",
}

// Upper bounds for quotas. These exist to catch ".cpu = 100000" typos before
// the cluster has to deal with them; we err on the generous side — a real
// tenant request hitting the cap should be reviewed by the platform team.
const (
	maxCPUCores   = 100
	maxMemoryGi   = 500
	maxPodsPerNS  = 1000
)

// +kubebuilder:webhook:path=/mutate-platform-mtkp-platform-v1alpha1-tenant,mutating=true,failurePolicy=fail,sideEffects=None,groups=platform.mtkp.platform,resources=tenants,verbs=create;update,versions=v1alpha1,name=mtenant.kb.io,admissionReviewVersions=v1

// TenantDefaulter applies the mutating webhook (defaults) for Tenant.
// Defaulting is a separate object from the validator so the failure modes
// stay distinct in logs/metrics and we can mount different RBAC if needed
// later.
type TenantDefaulter struct{}

// Default implements webhook.CustomDefaulter and fills in the safe
// defaults the platform expects when the user leaves a field blank.
func (d *TenantDefaulter) Default(_ context.Context, obj runtime.Object) error {
	tenant, ok := obj.(*Tenant)
	if !ok {
		return fmt.Errorf("expected *Tenant, got %T", obj)
	}

	// spec.namespace defaults to metadata.name so a Tenant CR named "payments"
	// implicitly targets the "payments" namespace.
	if tenant.Spec.Namespace == "" {
		tenant.Spec.Namespace = tenant.Name
	}

	// spec.deployRef defaults to "main". The existing kubebuilder default on
	// the CRD is "HEAD" but the platform standardized on "main" branches —
	// fix that here so older CRDs without the schema default still get the
	// right value.
	if tenant.Spec.DeployRef == "" {
		tenant.Spec.DeployRef = "main"
	}

	// spec.quotas.pods defaults to "100" if unset / "0". Treat zero-value
	// strings ("", "0") as "unset" — the user shouldn't be able to ask for
	// zero pods anyway, that's an error.
	if tenant.Spec.Quotas.Pods == "" || tenant.Spec.Quotas.Pods == "0" {
		tenant.Spec.Quotas.Pods = "100"
	}

	return nil
}

// +kubebuilder:webhook:path=/validate-platform-mtkp-platform-v1alpha1-tenant,mutating=false,failurePolicy=fail,sideEffects=None,groups=platform.mtkp.platform,resources=tenants,verbs=create;update,versions=v1alpha1,name=vtenant.kb.io,admissionReviewVersions=v1

// TenantValidator implements the validating admission webhook.
//
// All rules return a field.ErrorList so multiple problems surface in a
// single admission response — kubectl prints them all at once instead of
// playing whack-a-mole.
type TenantValidator struct{}

// ValidateCreate is called for CREATE admissions.
func (v *TenantValidator) ValidateCreate(_ context.Context, obj runtime.Object) (admission.Warnings, error) {
	tenant, ok := obj.(*Tenant)
	if !ok {
		return nil, fmt.Errorf("expected *Tenant, got %T", obj)
	}
	return nil, v.validate(tenant)
}

// ValidateUpdate is called for UPDATE admissions. We run the same rule set
// on the new object — the old object is unused because none of the rules
// involve transitions (no immutable fields, no monotonic counters).
func (v *TenantValidator) ValidateUpdate(_ context.Context, _ runtime.Object, newObj runtime.Object) (admission.Warnings, error) {
	tenant, ok := newObj.(*Tenant)
	if !ok {
		return nil, fmt.Errorf("expected *Tenant, got %T", newObj)
	}
	return nil, v.validate(tenant)
}

// ValidateDelete is a no-op. Deletion is always allowed; the finalizer in
// the reconciler handles teardown ordering.
func (v *TenantValidator) ValidateDelete(_ context.Context, _ runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

// validate runs every rule and aggregates errors. Returning a non-nil
// error here turns into an admission denial with all field errors echoed
// back to the caller.
func (v *TenantValidator) validate(t *Tenant) error {
	var errs field.ErrorList
	specPath := field.NewPath("spec")

	errs = append(errs, validateNamespace(specPath.Child("namespace"), t.Spec.Namespace)...)
	errs = append(errs, validateQuotas(specPath.Child("quotas"), &t.Spec.Quotas)...)
	errs = append(errs, validateSecrets(specPath.Child("secrets"), t.Spec.Secrets, t.Spec.Namespace)...)
	errs = append(errs, validateIngress(specPath.Child("ingress"), t.Spec.Ingress)...)
	errs = append(errs, validateIRSARoleArn(specPath.Child("irsaRoleArn"), t.Spec.IRSARoleArn)...)
	errs = append(errs, validateDeployRepo(specPath.Child("deployRepo"), t.Spec.DeployRepo)...)

	if len(errs) == 0 {
		return nil
	}
	return errs.ToAggregate()
}

// validateNamespace enforces DNS-1123 + the reserved-namespace list.
// We return one error per rule so the caller sees both "invalid label"
// AND "reserved" if a tenant somehow manages to violate both.
func validateNamespace(p *field.Path, ns string) field.ErrorList {
	var errs field.ErrorList
	if ns == "" {
		// Empty is fine — the defaulter will fill it from metadata.name.
		// If it's still empty at validate time the defaulter didn't run
		// (or wasn't wired), which is a configuration issue, not a user
		// error. Validating webhooks always run AFTER mutating webhooks
		// in the admission chain, so this branch is mostly defensive.
		return errs
	}
	if len(ns) > 63 || !dns1123LabelRegex.MatchString(ns) {
		errs = append(errs, field.Invalid(p, ns,
			"must be a valid DNS-1123 label: lower-case alphanumeric or '-', start/end alphanumeric, ≤63 chars"))
	}
	if _, reserved := reservedNamespaces[ns]; reserved {
		errs = append(errs, field.Forbidden(p,
			fmt.Sprintf("%q is reserved for the platform and cannot be used by a tenant", ns)))
	}
	return errs
}

// validateQuotas checks the upper bounds. We parse the strings with
// resource.ParseQuantity so the user gets a consistent error format
// across cpu/memory and we don't drift from kube's own parser.
func validateQuotas(p *field.Path, q *Quotas) field.ErrorList {
	var errs field.ErrorList

	if q.CPULimits != "" {
		qty, err := resource.ParseQuantity(q.CPULimits)
		if err != nil {
			errs = append(errs, field.Invalid(p.Child("cpuLimits"), q.CPULimits,
				fmt.Sprintf("not a valid resource.Quantity: %v", err)))
		} else if qty.Value() > maxCPUCores {
			// Value() rounds DOWN to whole cores. "100001m" → 100 → passes;
			// "100000" → 100000 → fails. Catches the typo case (101) but
			// also genuine over-asks (>100 cores).
			errs = append(errs, field.Forbidden(p.Child("cpuLimits"),
				fmt.Sprintf("requested cpu limit %s exceeds the per-tenant cap of %d cores", q.CPULimits, maxCPUCores)))
		}
	}

	if q.MemoryLimits != "" {
		qty, err := resource.ParseQuantity(q.MemoryLimits)
		if err != nil {
			errs = append(errs, field.Invalid(p.Child("memoryLimits"), q.MemoryLimits,
				fmt.Sprintf("not a valid resource.Quantity: %v", err)))
		} else {
			limit := resource.MustParse(fmt.Sprintf("%dGi", maxMemoryGi))
			if qty.Cmp(limit) > 0 {
				errs = append(errs, field.Forbidden(p.Child("memoryLimits"),
					fmt.Sprintf("requested memory limit %s exceeds the per-tenant cap of %dGi", q.MemoryLimits, maxMemoryGi)))
			}
		}
	}

	if q.Pods != "" {
		// Pods is a plain integer count, not a resource.Quantity.
		n, err := strconv.Atoi(q.Pods)
		if err != nil {
			errs = append(errs, field.Invalid(p.Child("pods"), q.Pods,
				fmt.Sprintf("not a valid integer: %v", err)))
		} else if n > maxPodsPerNS {
			errs = append(errs, field.Forbidden(p.Child("pods"),
				fmt.Sprintf("requested pod count %d exceeds the per-tenant cap of %d", n, maxPodsPerNS)))
		}
	}

	return errs
}

// validateSecrets enforces the platform-path policy AND cross-references
// each remoteRef's namespace segment against spec.namespace, so a tenant
// can't pull another tenant's secrets even if RBAC on the secret store
// would allow it.
func validateSecrets(p *field.Path, secrets []SecretRef, tenantNS string) field.ErrorList {
	var errs field.ErrorList
	for i, s := range secrets {
		itemPath := p.Index(i).Child("remoteRef")
		if !remoteRefRegex.MatchString(s.RemoteRef) {
			errs = append(errs, field.Invalid(itemPath, s.RemoteRef,
				`must match ^platform/(dev|staging|prod|prod-dr)/<namespace>/<key>$`))
			continue
		}
		// Path is now known well-formed: split returns 4 segments.
		parts := strings.Split(s.RemoteRef, "/")
		if tenantNS != "" && parts[2] != tenantNS {
			errs = append(errs, field.Forbidden(itemPath,
				fmt.Sprintf("remoteRef namespace segment %q must equal spec.namespace %q", parts[2], tenantNS)))
		}
	}
	return errs
}

// validateIngress restricts the host to the platform's owned apex
// domains. We allow exact-match against the suffix (so "*.caas.example.com"
// is OK) plus arbitrary leading subdomains.
func validateIngress(p *field.Path, ing *IngressSpec) field.ErrorList {
	var errs field.ErrorList
	if ing == nil || ing.Host == "" {
		return errs
	}
	for _, sfx := range allowedIngressSuffixes {
		if strings.HasSuffix(ing.Host, sfx) {
			return errs
		}
	}
	errs = append(errs, field.Forbidden(p.Child("host"),
		fmt.Sprintf("%q must end with one of %v — domains outside the platform apex are not permitted",
			ing.Host, allowedIngressSuffixes)))
	return errs
}

// validateIRSARoleArn matches the AWS IAM role ARN shape. The field is
// optional so an empty string is fine.
func validateIRSARoleArn(p *field.Path, arn string) field.ErrorList {
	var errs field.ErrorList
	if arn == "" {
		return errs
	}
	if !irsaRoleArnRegex.MatchString(arn) {
		errs = append(errs, field.Invalid(p, arn,
			`must match ^arn:aws[a-z-]*:iam::[0-9]{12}:role/...`))
	}
	return errs
}

// validateDeployRepo restricts the source repo URL to github.com.
func validateDeployRepo(p *field.Path, repo string) field.ErrorList {
	var errs field.ErrorList
	if repo == "" {
		// Required at the schema level, but if we got here without it,
		// surface a clear webhook error instead of relying on the OpenAPI
		// rejection.
		errs = append(errs, field.Required(p, "deployRepo is required"))
		return errs
	}
	if !deployRepoRegex.MatchString(repo) {
		errs = append(errs, field.Invalid(p, repo,
			`must be an https://github.com/<org>/<repo>(.git)? URL — GitLab/Bitbucket are not approved sources`))
	}
	return errs
}

// SetupTenantWebhookWithManager registers both the mutating and the
// validating webhook handlers with the manager. This is the single
// entry point that main.go calls.
func SetupTenantWebhookWithManager(mgr ctrl.Manager) error {
	tenantWebhookLog.Info("registering Tenant defaulter + validator")
	if err := ctrl.NewWebhookManagedBy(mgr).
		For(&Tenant{}).
		WithDefaulter(&TenantDefaulter{}).
		WithValidator(&TenantValidator{}).
		Complete(); err != nil {
		return fmt.Errorf("registering tenant webhook: %w", err)
	}
	return nil
}

// Compile-time assertions that our types satisfy the controller-runtime
// webhook interfaces. If a controller-runtime upgrade changes the
// interface shape these will fail to compile, giving us an early signal
// instead of a runtime "webhook not registered" silence.
var (
	_ webhook.CustomDefaulter = (*TenantDefaulter)(nil)
	_ webhook.CustomValidator = (*TenantValidator)(nil)
)
