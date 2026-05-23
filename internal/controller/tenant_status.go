package controller

import (
	"context"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log"

	platformv1alpha1 "github.com/example/tenant-rbac-controller/api/v1alpha1"
)

// setReady sets a condition to True with a structured reason/message and bumps ObservedGeneration.
func (r *Reconciler) setReady(t *platformv1alpha1.Tenant, condType, reason, msg string) {
	meta.SetStatusCondition(&t.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             metav1.ConditionTrue,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: t.Generation,
		LastTransitionTime: metav1.NewTime(time.Now()),
	})
}

// setFailed sets a condition to False and surfaces the underlying error message.
func (r *Reconciler) setFailed(t *platformv1alpha1.Tenant, condType, reason string, err error) {
	meta.SetStatusCondition(&t.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             metav1.ConditionFalse,
		Reason:             reason,
		Message:            err.Error(),
		ObservedGeneration: t.Generation,
		LastTransitionTime: metav1.NewTime(time.Now()),
	})
	// The overall Ready condition flips False as soon as any sub-concern fails.
	meta.SetStatusCondition(&t.Status.Conditions, metav1.Condition{
		Type:               platformv1alpha1.ConditionReady,
		Status:             metav1.ConditionFalse,
		Reason:             reason,
		Message:            fmt.Sprintf("%s failed: %s", condType, err.Error()),
		ObservedGeneration: t.Generation,
		LastTransitionTime: metav1.NewTime(time.Now()),
	})
}

// failWith records a structured failure on the Tenant, increments the per-concern
// error metric, persists the status, and returns the error so controller-runtime
// applies its standard exponential backoff.
//
// Status update conflicts are swallowed — a fresh reconcile will produce a fresh
// status snapshot.
func (r *Reconciler) failWith(
	ctx context.Context,
	t *platformv1alpha1.Tenant,
	condType, concern string,
	err error,
) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("tenant", t.Name, "concern", concern)
	logger.Error(err, "reconcile failure")

	reconcileErrors.WithLabelValues(t.Name, concern).Inc()

	t.Status.Phase = platformv1alpha1.PhaseFailed
	r.setFailed(t, condType, concern+"Failed", err)

	if updateErr := r.Status().Update(ctx, t); updateErr != nil && !apierrors.IsConflict(updateErr) {
		logger.Error(updateErr, "status update failed during error path")
	}
	return ctrl.Result{}, err
}
