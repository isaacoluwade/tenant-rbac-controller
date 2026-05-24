// Package v1alpha1 — webhook unit tests.
//
// We exercise the validator and defaulter directly (no envtest API server)
// so the test runtime stays under a second. Direct invocation is fine
// because the controller-runtime webhook plumbing is just glue —
// behaviour we care about lives in Default() and validate().
//
// A separate envtest-backed suite can stand up a real API server with the
// ValidatingWebhookConfiguration registered to assert the wiring works
// end-to-end; that's gated behind `make webhook-test` to keep CI fast.
package v1alpha1

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	rbacv1 "k8s.io/api/rbac/v1"
)

// baseTenant returns a fully-valid Tenant. Test cases mutate fields on a
// copy of this object to isolate exactly the rule under test.
func baseTenant() *Tenant {
	return &Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "payments"},
		Spec: TenantSpec{
			DisplayName: "Payments",
			Namespace:   "payments",
			Owners: []rbacv1.Subject{{
				Kind:     "Group",
				Name:     "my-org:payments-team",
				APIGroup: "rbac.authorization.k8s.io",
			}},
			Quotas: Quotas{
				CPULimits:    "20",
				MemoryLimits: "40Gi",
				Pods:         "100",
			},
			Secrets: []SecretRef{{
				Name:      "payments-db-creds",
				RemoteRef: "platform/prod/payments/db-creds",
			}},
			Ingress: &IngressSpec{
				Host: "payments.caas.example.com",
			},
			IRSARoleArn: "arn:aws:iam::123456789012:role/mtkp-prod-use1-payments",
			DeployRepo:  "https://github.com/my-org/tenant-deploy-payments.git",
			DeployRef:   "main",
		},
	}
}

// TestDefaulter — verifies the mutator fills in the documented defaults
// when the user omits the corresponding fields.
func TestDefaulter(t *testing.T) {
	d := &TenantDefaulter{}

	t.Run("namespace defaults to metadata.name", func(t *testing.T) {
		tn := baseTenant()
		tn.Spec.Namespace = ""
		require.NoError(t, d.Default(context.Background(), tn))
		assert.Equal(t, "payments", tn.Spec.Namespace)
	})

	t.Run("deployRef defaults to main", func(t *testing.T) {
		tn := baseTenant()
		tn.Spec.DeployRef = ""
		require.NoError(t, d.Default(context.Background(), tn))
		assert.Equal(t, "main", tn.Spec.DeployRef)
	})

	t.Run("pods defaults to 100", func(t *testing.T) {
		tn := baseTenant()
		tn.Spec.Quotas.Pods = ""
		require.NoError(t, d.Default(context.Background(), tn))
		assert.Equal(t, "100", tn.Spec.Quotas.Pods)
	})

	t.Run("defaulter does not overwrite explicit values", func(t *testing.T) {
		tn := baseTenant()
		tn.Spec.Namespace = "explicit-ns"
		tn.Spec.DeployRef = "release-1.2"
		tn.Spec.Quotas.Pods = "250"
		require.NoError(t, d.Default(context.Background(), tn))
		assert.Equal(t, "explicit-ns", tn.Spec.Namespace)
		assert.Equal(t, "release-1.2", tn.Spec.DeployRef)
		assert.Equal(t, "250", tn.Spec.Quotas.Pods)
	})

	t.Run("validator passes once defaulter has run", func(t *testing.T) {
		// A minimal Tenant — every defaultable field blank — should
		// pass validation after the defaulter normalises it.
		tn := &Tenant{
			ObjectMeta: metav1.ObjectMeta{Name: "minimal"},
			Spec: TenantSpec{
				DisplayName: "Minimal",
				Owners:      []rbacv1.Subject{{Kind: "Group", Name: "g", APIGroup: "rbac.authorization.k8s.io"}},
				DeployRepo:  "https://github.com/my-org/minimal.git",
			},
		}
		require.NoError(t, d.Default(context.Background(), tn))
		_, err := (&TenantValidator{}).ValidateCreate(context.Background(), tn)
		assert.NoError(t, err)
	})
}

// TestValidator_HappyPath — the canonical good Tenant must pass cleanly.
// This is the regression guard: if anyone tightens a rule, this case is
// the canary.
func TestValidator_HappyPath(t *testing.T) {
	v := &TenantValidator{}
	_, err := v.ValidateCreate(context.Background(), baseTenant())
	assert.NoError(t, err)
}

// TestValidator_Rules — table-driven coverage for every documented
// rejection rule. Each row mutates exactly one field on a fresh
// baseTenant so failures point at the rule under test, not interactions.
func TestValidator_Rules(t *testing.T) {
	tests := []struct {
		name      string
		mutate    func(*Tenant)
		wantFrag  string // substring expected in the aggregated error
	}{
		{
			name:     "rejects invalid DNS-1123 namespace",
			mutate:   func(t *Tenant) { t.Spec.Namespace = "Payments_Team" },
			wantFrag: "DNS-1123",
		},
		{
			name:     "rejects overly long namespace",
			mutate:   func(t *Tenant) { t.Spec.Namespace = strings.Repeat("a", 64) },
			wantFrag: "DNS-1123",
		},
		{
			name:     "rejects reserved namespace kube-system",
			mutate:   func(t *Tenant) { t.Spec.Namespace = "kube-system" },
			wantFrag: "reserved",
		},
		{
			name:     "rejects reserved namespace argocd",
			mutate:   func(t *Tenant) { t.Spec.Namespace = "argocd" },
			wantFrag: "reserved",
		},
		{
			name:     "rejects cpu over 100 cores (catches 100000 typo)",
			mutate:   func(t *Tenant) { t.Spec.Quotas.CPULimits = "100000" },
			wantFrag: "cpu",
		},
		{
			name:     "rejects memory over 500Gi",
			mutate:   func(t *Tenant) { t.Spec.Quotas.MemoryLimits = "501Gi" },
			wantFrag: "memory",
		},
		{
			name:     "rejects pods over 1000",
			mutate:   func(t *Tenant) { t.Spec.Quotas.Pods = "1001" },
			wantFrag: "pod count",
		},
		{
			name: "rejects remoteRef not matching platform-path",
			mutate: func(t *Tenant) {
				t.Spec.Secrets[0].RemoteRef = "vault/secret/data/foo"
			},
			wantFrag: "platform/",
		},
		{
			name: "rejects remoteRef pointing at another tenant's namespace",
			mutate: func(t *Tenant) {
				t.Spec.Secrets[0].RemoteRef = "platform/prod/other-tenant/db-creds"
			},
			wantFrag: "spec.namespace",
		},
		{
			name: "rejects ingress host outside platform apex",
			mutate: func(t *Tenant) {
				t.Spec.Ingress.Host = "payments.evil.com"
			},
			wantFrag: "platform apex",
		},
		{
			name:     "rejects malformed irsaRoleArn",
			mutate:   func(t *Tenant) { t.Spec.IRSARoleArn = "arn:aws:s3:::my-bucket" },
			wantFrag: "iam::",
		},
		{
			name:     "rejects non-github deployRepo",
			mutate:   func(t *Tenant) { t.Spec.DeployRepo = "https://gitlab.com/my-org/repo.git" },
			wantFrag: "github.com",
		},
	}

	v := &TenantValidator{}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tenant := baseTenant()
			tc.mutate(tenant)
			_, err := v.ValidateCreate(context.Background(), tenant)
			require.Error(t, err, "expected rejection")
			assert.Contains(t, err.Error(), tc.wantFrag,
				"error %q should mention %q", err.Error(), tc.wantFrag)
		})
	}
}

// TestValidator_ValidateDelete confirms deletion is always permitted —
// the reconciler's finalizer is the only thing that should gate it.
func TestValidator_ValidateDelete(t *testing.T) {
	v := &TenantValidator{}
	_, err := v.ValidateDelete(context.Background(), baseTenant())
	assert.NoError(t, err)
}

// TestValidator_ValidateUpdate ensures update admissions run the full
// rule set on the new object (we don't care about transitions here).
func TestValidator_ValidateUpdate(t *testing.T) {
	v := &TenantValidator{}
	old := baseTenant()
	updated := baseTenant()
	updated.Spec.DeployRepo = "https://gitlab.com/my-org/repo.git"
	_, err := v.ValidateUpdate(context.Background(), old, updated)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "github.com")
}
