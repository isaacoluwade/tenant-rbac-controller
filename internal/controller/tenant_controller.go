// Package controller hosts the Tenant reconciler.
package controller

import (
	"context"
	"fmt"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	platformv1alpha1 "github.com/example/tenant-rbac-controller/api/v1alpha1"
)

// FinalizerName is recorded on every Tenant so the controller can run cleanup
// before Kubernetes garbage collection removes the object.
const FinalizerName = "platform.mtkp/tenant-finalizer"

// Reconciler manages Tenant CRs and the resources they own.
type Reconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// PlatformNamespace identifies pods the operator allows ingress from when
	// rendering the "allow-platform-ingress" NetworkPolicy.
	PlatformNamespace string

	// ArgoCDNamespace is where AppProject/Application live (typically "argocd").
	ArgoCDNamespace string
}

// Prometheus metrics. Registered with the controller-runtime registry so the
// /metrics endpoint exposes them alongside the built-in controller metrics.
var (
	reconcileDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "tenant_reconcile_duration_seconds",
			Help:    "Time spent reconciling a Tenant, in seconds.",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"tenant"},
	)
	reconcileErrors = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "tenant_reconcile_errors_total",
			Help: "Total reconciliation errors, labeled by tenant and the failing concern.",
		},
		[]string{"tenant", "concern"},
	)
)

func init() {
	metrics.Registry.MustRegister(reconcileDuration, reconcileErrors)
}

// +kubebuilder:rbac:groups=platform.mtkp.platform,resources=tenants,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=platform.mtkp.platform,resources=tenants/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=platform.mtkp.platform,resources=tenants/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=namespaces;serviceaccounts;resourcequotas;limitranges,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=roles;rolebindings,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies;ingresses,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=external-secrets.io,resources=externalsecrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=argoproj.io,resources=appprojects;applications,verbs=get;list;watch;create;update;patch;delete

// Reconcile drives the desired-state machine for a Tenant.
//
// High-level flow:
//  1. Fetch the Tenant; ignore "not found" (already deleted).
//  2. If the object is being deleted, run finalizer cleanup.
//  3. Ensure the finalizer is set so cleanup runs before the API server GCs the CR.
//  4. Walk the dependency order: namespace -> quota/limits -> RBAC -> NetworkPolicy
//     -> ServiceAccount (IRSA) -> ExternalSecrets -> AppProject -> Application
//     -> Ingress (optional). Each step is idempotent via CreateOrUpdate.
//  5. Aggregate per-step results into status conditions and an overall phase.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	start := time.Now()
	defer func() {
		reconcileDuration.WithLabelValues(req.Name).Observe(time.Since(start).Seconds())
	}()

	logger := log.FromContext(ctx).WithValues("tenant", req.Name)

	var tenant platformv1alpha1.Tenant
	if err := r.Get(ctx, req.NamespacedName, &tenant); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}

	// Default Spec.Namespace before we use it downstream.
	if tenant.Spec.Namespace == "" {
		tenant.Spec.Namespace = tenant.Name
	}
	tenant.Status.Namespace = tenant.Spec.Namespace

	// Deletion path runs cleanup, then strips the finalizer.
	if !tenant.DeletionTimestamp.IsZero() {
		tenant.Status.Phase = platformv1alpha1.PhaseTerminating
		_ = r.Status().Update(ctx, &tenant)
		return r.handleDeletion(ctx, &tenant)
	}

	// Add the finalizer on first sight, before we provision any owned resources.
	if !controllerutil.ContainsFinalizer(&tenant, FinalizerName) {
		controllerutil.AddFinalizer(&tenant, FinalizerName)
		if err := r.Update(ctx, &tenant); err != nil {
			return reconcile.Result{}, fmt.Errorf("adding finalizer: %w", err)
		}
		return reconcile.Result{Requeue: true}, nil
	}

	tenant.Status.Phase = platformv1alpha1.PhaseProvisioning

	// Step 1: namespace.
	if err := r.reconcileNamespace(ctx, &tenant); err != nil {
		return r.failWith(ctx, &tenant, platformv1alpha1.ConditionNamespaceReady, "Namespace", err)
	}
	r.setReady(&tenant, platformv1alpha1.ConditionNamespaceReady, "NamespaceProvisioned",
		"namespace exists and is labeled")

	// Step 2: ResourceQuota + LimitRange.
	if err := r.reconcileQuotas(ctx, &tenant); err != nil {
		return r.failWith(ctx, &tenant, platformv1alpha1.ConditionQuotaReady, "Quotas", err)
	}
	r.setReady(&tenant, platformv1alpha1.ConditionQuotaReady, "QuotasApplied",
		"ResourceQuota and LimitRange reconciled")

	// Step 3: NetworkPolicy (default-deny + intra-namespace + platform ingress).
	if err := r.reconcileNetworkPolicies(ctx, &tenant); err != nil {
		return r.failWith(ctx, &tenant, platformv1alpha1.ConditionNetworkPolicyReady, "NetworkPolicy", err)
	}
	r.setReady(&tenant, platformv1alpha1.ConditionNetworkPolicyReady, "NetworkPoliciesApplied",
		"default-deny + baseline allows reconciled")

	// Step 4: RBAC (RoleBinding pointing at the cluster's `edit` ClusterRole).
	if err := r.reconcileRBAC(ctx, &tenant); err != nil {
		return r.failWith(ctx, &tenant, platformv1alpha1.ConditionRBACReady, "RBAC", err)
	}
	r.setReady(&tenant, platformv1alpha1.ConditionRBACReady, "RBACApplied",
		"owner RoleBinding reconciled")

	// Step 5: ServiceAccount with IRSA annotation.
	if err := r.reconcileServiceAccount(ctx, &tenant); err != nil {
		return r.failWith(ctx, &tenant, platformv1alpha1.ConditionRBACReady, "ServiceAccount", err)
	}

	// Step 6: ExternalSecrets.
	if err := r.reconcileExternalSecrets(ctx, &tenant); err != nil {
		return r.failWith(ctx, &tenant, platformv1alpha1.ConditionSecretsReady, "ExternalSecrets", err)
	}
	r.setReady(&tenant, platformv1alpha1.ConditionSecretsReady, "ExternalSecretsApplied",
		fmt.Sprintf("%d ExternalSecret(s) reconciled", len(tenant.Spec.Secrets)))

	// Step 7: ArgoCD AppProject + Application.
	if err := r.reconcileAppProject(ctx, &tenant); err != nil {
		return r.failWith(ctx, &tenant, platformv1alpha1.ConditionAppProjectReady, "AppProject", err)
	}
	if err := r.reconcileApplication(ctx, &tenant); err != nil {
		return r.failWith(ctx, &tenant, platformv1alpha1.ConditionAppProjectReady, "Application", err)
	}
	r.setReady(&tenant, platformv1alpha1.ConditionAppProjectReady, "ArgoCDProvisioned",
		"AppProject and Application reconciled")

	// Step 8: Ingress (only if spec.ingress is set).
	if tenant.Spec.Ingress != nil {
		if err := r.reconcileIngress(ctx, &tenant); err != nil {
			return r.failWith(ctx, &tenant, platformv1alpha1.ConditionIngressReady, "Ingress", err)
		}
		r.setReady(&tenant, platformv1alpha1.ConditionIngressReady, "IngressProvisioned",
			"default Ingress reconciled")
	} else {
		r.setReady(&tenant, platformv1alpha1.ConditionIngressReady, "IngressNotRequested",
			"spec.ingress omitted; nothing to do")
	}

	// All concerns succeeded: mark Ready and persist.
	r.setReady(&tenant, platformv1alpha1.ConditionReady, "Reconciled",
		"tenant environment fully reconciled")
	tenant.Status.Phase = platformv1alpha1.PhaseReady
	tenant.Status.ObservedGeneration = tenant.Generation

	if err := r.Status().Update(ctx, &tenant); err != nil {
		// Conflict here is benign; controller-runtime will requeue.
		if apierrors.IsConflict(err) {
			return reconcile.Result{Requeue: true}, nil
		}
		logger.Error(err, "status update failed")
		return reconcile.Result{}, err
	}

	// Re-reconcile periodically to catch drift even when nothing changes.
	return reconcile.Result{RequeueAfter: 5 * time.Minute}, nil
}

// SetupWithManager registers the controller with the manager and configures the watches.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&platformv1alpha1.Tenant{}).
		// Each downstream resource is owned by the Tenant; controller-runtime
		// will requeue the parent when an owned resource changes.
		Named("tenant").
		Complete(r)
}
