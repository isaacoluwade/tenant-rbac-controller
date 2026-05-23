package controller

import (
	"context"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	platformv1alpha1 "github.com/example/tenant-rbac-controller/api/v1alpha1"
)

// handleDeletion runs cleanup that isn't handled by Kubernetes garbage collection.
//
// What we explicitly delete:
//   - The ArgoCD Application and AppProject. These live in the argocd namespace,
//     not as children of the Tenant (controller references can't cross
//     namespaces for namespace-scoped owned resources), so GC won't touch them.
//
// What we let GC handle:
//   - The tenant Namespace, ResourceQuota, LimitRange, RoleBinding, NetworkPolicies,
//     ServiceAccount, ExternalSecrets, and Ingress. All have an ownerReference
//     pointing back at the Tenant, so deleting the Tenant cascades.
//
// Once the explicit deletes succeed (or are already gone), we strip the
// finalizer and the API server completes the delete.
func (r *Reconciler) handleDeletion(ctx context.Context, t *platformv1alpha1.Tenant) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("tenant", t.Name)

	if !controllerutil.ContainsFinalizer(t, FinalizerName) {
		return ctrl.Result{}, nil
	}

	argoNS := r.ArgoCDNamespace
	if argoNS == "" {
		argoNS = "argocd"
	}

	// Application first (so it stops syncing while we tear down).
	if err := r.deleteArgoResource(ctx, applicationGVK, t.Name, argoNS); err != nil {
		return ctrl.Result{}, fmt.Errorf("delete argocd application: %w", err)
	}
	if err := r.deleteArgoResource(ctx, appProjectGVK, t.Name, argoNS); err != nil {
		return ctrl.Result{}, fmt.Errorf("delete argocd appproject: %w", err)
	}

	controllerutil.RemoveFinalizer(t, FinalizerName)
	if err := r.Update(ctx, t); err != nil {
		if apierrors.IsConflict(err) {
			return ctrl.Result{Requeue: true}, nil
		}
		logger.Error(err, "removing finalizer")
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// deleteArgoResource issues a Delete for the named object using its GVK.
// "Not found" and "no matches for kind" (CRD not installed) are treated as
// success — the resource we wanted to remove isn't there.
func (r *Reconciler) deleteArgoResource(ctx context.Context, gvk schema.GroupVersionKind, name, ns string) error {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)
	u.SetName(name)
	u.SetNamespace(ns)
	if err := r.Delete(ctx, u); err != nil {
		if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
			return nil
		}
		// Some errors come back as a plain string but indicate the kind isn't
		// registered with the RESTMapper (e.g. when running in envtest without
		// ArgoCD CRDs installed). Tolerate them.
		if strings.Contains(err.Error(), "no matches for kind") {
			return nil
		}
		return err
	}
	return nil
}

// Compile-time assertion: Reconciler still satisfies controller-runtime's
// Reconciler interface. Helps catch refactor regressions.
var _ interface {
	Reconcile(context.Context, ctrl.Request) (ctrl.Result, error)
} = (*Reconciler)(nil)

// Keep imports stable when the controller is extended.
var _ = client.IgnoreNotFound
