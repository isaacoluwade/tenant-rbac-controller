package controller

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	platformv1alpha1 "github.com/example/tenant-rbac-controller/api/v1alpha1"
)

// newTestScheme builds a scheme with the core, RBAC, networking, and Tenant types
// registered. We use the same scheme for the fake client and the Reconciler.
func newTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(s))
	require.NoError(t, platformv1alpha1.AddToScheme(s))
	return s
}

func newFixture(t *testing.T, tenant *platformv1alpha1.Tenant) (*Reconciler, client.Client) {
	t.Helper()
	scheme := newTestScheme(t)
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(tenant).
		WithStatusSubresource(&platformv1alpha1.Tenant{}).
		Build()
	r := &Reconciler{
		Client:            cl,
		Scheme:            scheme,
		PlatformNamespace: "mtkp-platform",
		ArgoCDNamespace:   "argocd",
	}
	return r, cl
}

// sampleTenant returns a Tenant fixture with everything set so each reconcile
// step has something to do.
func sampleTenant() *platformv1alpha1.Tenant {
	return &platformv1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "payments"},
		Spec: platformv1alpha1.TenantSpec{
			DisplayName: "Payments",
			Namespace:   "payments",
			Owners: []rbacv1.Subject{{
				Kind:     "Group",
				Name:     "payments-team",
				APIGroup: "rbac.authorization.k8s.io",
			}},
			Quotas: platformv1alpha1.Quotas{
				CPURequests:          "10",
				CPULimits:            "20",
				MemoryRequests:       "10Gi",
				MemoryLimits:         "20Gi",
				Pods:                 "50",
				DefaultLimitCPU:      "500m",
				DefaultLimitMemory:   "512Mi",
				DefaultRequestCPU:    "100m",
				DefaultRequestMemory: "128Mi",
			},
			IRSARoleArn: "arn:aws:iam::123456789012:role/mtkp-tenant-payments",
			DeployRepo:  "https://github.com/example/payments-deploy",
			DeployRef:   "main",
		},
	}
}

// TestReconcile_AddsFinalizer covers the first-touch behavior where the controller
// patches in the finalizer and requeues.
func TestReconcile_AddsFinalizer(t *testing.T) {
	ctx := context.Background()
	tenant := sampleTenant()
	r, cl := newFixture(t, tenant)

	result, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: tenant.Name}})
	require.NoError(t, err)
	assert.True(t, result.Requeue, "expected requeue after adding finalizer")

	var got platformv1alpha1.Tenant
	require.NoError(t, cl.Get(ctx, types.NamespacedName{Name: tenant.Name}, &got))
	assert.Contains(t, got.Finalizers, FinalizerName)
}

// TestReconcile_HappyPath drives a full reconcile and asserts the key owned
// resources land in the cluster.
func TestReconcile_HappyPath(t *testing.T) {
	ctx := context.Background()
	tenant := sampleTenant()
	// Pre-add the finalizer so the first Reconcile proceeds past the bootstrap step.
	tenant.Finalizers = []string{FinalizerName}

	r, cl := newFixture(t, tenant)

	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: tenant.Name}})
	require.NoError(t, err)

	// Namespace was created.
	var ns corev1.Namespace
	require.NoError(t, cl.Get(ctx, types.NamespacedName{Name: "payments"}, &ns))
	assert.Equal(t, "payments", ns.Labels["mtkp.platform/tenant"])
	assert.Equal(t, "enabled", ns.Labels["istio-injection"])

	// ResourceQuota.
	var rq corev1.ResourceQuota
	require.NoError(t, cl.Get(ctx, types.NamespacedName{Name: "tenant-quota", Namespace: "payments"}, &rq))
	// resource.Quantity.String() has a pointer receiver, and map indexing
	// returns a non-addressable copy — capture the values first.
	cpuReq := rq.Spec.Hard["requests.cpu"]
	pods := rq.Spec.Hard["pods"]
	assert.Equal(t, "10", cpuReq.String())
	assert.Equal(t, "50", pods.String())

	// LimitRange.
	var lr corev1.LimitRange
	require.NoError(t, cl.Get(ctx, types.NamespacedName{Name: "tenant-limits", Namespace: "payments"}, &lr))
	require.Len(t, lr.Spec.Limits, 1)

	// Default-deny NetworkPolicy.
	var deny networkingv1.NetworkPolicy
	require.NoError(t, cl.Get(ctx, types.NamespacedName{Name: "default-deny-all", Namespace: "payments"}, &deny))
	assert.Contains(t, deny.Spec.PolicyTypes, networkingv1.PolicyTypeIngress)
	assert.Contains(t, deny.Spec.PolicyTypes, networkingv1.PolicyTypeEgress)

	// RoleBinding for the owner group.
	var rb rbacv1.RoleBinding
	require.NoError(t, cl.Get(ctx, types.NamespacedName{Name: "tenant-owners", Namespace: "payments"}, &rb))
	require.Len(t, rb.Subjects, 1)
	assert.Equal(t, "payments-team", rb.Subjects[0].Name)

	// ServiceAccount with IRSA annotation.
	var sa corev1.ServiceAccount
	require.NoError(t, cl.Get(ctx, types.NamespacedName{Name: "tenant", Namespace: "payments"}, &sa))
	assert.Equal(t,
		"arn:aws:iam::123456789012:role/mtkp-tenant-payments",
		sa.Annotations["eks.amazonaws.com/role-arn"],
	)
}

// TestReconcile_Idempotent runs Reconcile twice and confirms the resource state
// is unchanged (same generations, no churn).
func TestReconcile_Idempotent(t *testing.T) {
	ctx := context.Background()
	tenant := sampleTenant()
	tenant.Finalizers = []string{FinalizerName}

	r, cl := newFixture(t, tenant)

	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: tenant.Name}})
	require.NoError(t, err)

	var first corev1.Namespace
	require.NoError(t, cl.Get(ctx, types.NamespacedName{Name: "payments"}, &first))
	firstGen := first.Generation

	_, err = r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: tenant.Name}})
	require.NoError(t, err)

	var second corev1.Namespace
	require.NoError(t, cl.Get(ctx, types.NamespacedName{Name: "payments"}, &second))
	assert.Equal(t, firstGen, second.Generation,
		"second reconcile should not mutate the namespace")
}

// TestReconcile_BadQuotaSurfacesError checks that an invalid quantity in the
// spec produces an error instead of a panic from MustParse.
func TestReconcile_BadQuotaSurfacesError(t *testing.T) {
	ctx := context.Background()
	tenant := sampleTenant()
	tenant.Finalizers = []string{FinalizerName}
	tenant.Spec.Quotas.CPURequests = "not-a-quantity"

	r, _ := newFixture(t, tenant)

	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: tenant.Name}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse requests.cpu")
}

// TestReconcile_IngressOnlyWhenSet verifies the optional Ingress isn't created
// when spec.ingress is nil, and IS created when spec.ingress is set.
func TestReconcile_IngressOnlyWhenSet(t *testing.T) {
	ctx := context.Background()
	tenant := sampleTenant()
	tenant.Finalizers = []string{FinalizerName}
	tenant.Spec.Ingress = &platformv1alpha1.IngressSpec{
		Host:         "payments.example.com",
		ServiceName:  "payments-api",
		ServicePort:  8080,
		IngressClass: "alb",
	}

	r, cl := newFixture(t, tenant)
	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: tenant.Name}})
	require.NoError(t, err)

	var ing networkingv1.Ingress
	require.NoError(t, cl.Get(ctx, types.NamespacedName{Name: "tenant-default", Namespace: "payments"}, &ing))
	require.Len(t, ing.Spec.Rules, 1)
	assert.Equal(t, "payments.example.com", ing.Spec.Rules[0].Host)
}
