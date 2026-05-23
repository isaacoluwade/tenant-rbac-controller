package controller

import (
	"context"
	"fmt"

	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	platformv1alpha1 "github.com/example/tenant-rbac-controller/api/v1alpha1"
)

// reconcileNamespace creates/updates the tenant namespace with the standard labels.
func (r *Reconciler) reconcileNamespace(ctx context.Context, t *platformv1alpha1.Tenant) error {
	ns := &corev1.Namespace{}
	ns.Name = t.Spec.Namespace

	op, err := controllerutil.CreateOrUpdate(ctx, r.Client, ns, func() error {
		if ns.Labels == nil {
			ns.Labels = map[string]string{}
		}
		ns.Labels["mtkp.platform/tenant"] = t.Name
		ns.Labels["mtkp.platform/display-name"] = sanitizeLabel(t.Spec.DisplayName)
		ns.Labels["istio-injection"] = "enabled"
		// Namespace is cluster-scoped; ownerReference still works for cluster-scoped Tenant.
		return controllerutil.SetControllerReference(t, ns, r.Scheme)
	})
	if err != nil {
		return fmt.Errorf("namespace %s: %w", ns.Name, err)
	}
	_ = op
	return nil
}

// reconcileQuotas applies the ResourceQuota and the LimitRange.
func (r *Reconciler) reconcileQuotas(ctx context.Context, t *platformv1alpha1.Tenant) error {
	q := t.Spec.Quotas

	rq := &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{Name: "tenant-quota", Namespace: t.Spec.Namespace},
	}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, rq, func() error {
		hard, err := parseQuotaHard(q)
		if err != nil {
			return err
		}
		rq.Spec.Hard = hard
		return controllerutil.SetControllerReference(t, rq, r.Scheme)
	}); err != nil {
		return fmt.Errorf("resourcequota: %w", err)
	}

	lr := &corev1.LimitRange{
		ObjectMeta: metav1.ObjectMeta{Name: "tenant-limits", Namespace: t.Spec.Namespace},
	}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, lr, func() error {
		defLimit, err := parseLimitMap(q.DefaultLimitCPU, q.DefaultLimitMemory)
		if err != nil {
			return err
		}
		defRequest, err := parseLimitMap(q.DefaultRequestCPU, q.DefaultRequestMemory)
		if err != nil {
			return err
		}
		lr.Spec.Limits = []corev1.LimitRangeItem{{
			Type:           corev1.LimitTypeContainer,
			Default:        defLimit,
			DefaultRequest: defRequest,
		}}
		return controllerutil.SetControllerReference(t, lr, r.Scheme)
	}); err != nil {
		return fmt.Errorf("limitrange: %w", err)
	}
	return nil
}

// parseQuotaHard converts the tenant's Quotas into a corev1.ResourceList.
// We use ParseQuantity (returns error) instead of MustParse so a bad string
// surfaces as a reconcile error rather than panicking the controller.
func parseQuotaHard(q platformv1alpha1.Quotas) (corev1.ResourceList, error) {
	pairs := []struct {
		key string
		val string
	}{
		{"requests.cpu", q.CPURequests},
		{"limits.cpu", q.CPULimits},
		{"requests.memory", q.MemoryRequests},
		{"limits.memory", q.MemoryLimits},
		{"pods", q.Pods},
	}
	out := corev1.ResourceList{}
	for _, p := range pairs {
		if p.val == "" {
			continue
		}
		q, err := resource.ParseQuantity(p.val)
		if err != nil {
			return nil, fmt.Errorf("parse %s=%q: %w", p.key, p.val, err)
		}
		out[corev1.ResourceName(p.key)] = q
	}
	return out, nil
}

func parseLimitMap(cpu, mem string) (corev1.ResourceList, error) {
	out := corev1.ResourceList{}
	if cpu != "" {
		q, err := resource.ParseQuantity(cpu)
		if err != nil {
			return nil, fmt.Errorf("parse cpu=%q: %w", cpu, err)
		}
		out[corev1.ResourceCPU] = q
	}
	if mem != "" {
		q, err := resource.ParseQuantity(mem)
		if err != nil {
			return nil, fmt.Errorf("parse mem=%q: %w", mem, err)
		}
		out[corev1.ResourceMemory] = q
	}
	return out, nil
}

// reconcileNetworkPolicies applies three policies:
//   - default-deny-all (ingress+egress)
//   - allow-within-namespace (lets workloads talk to each other freely)
//   - allow-platform-ingress (lets the platform namespace reach the tenant, e.g. for probes/metrics)
func (r *Reconciler) reconcileNetworkPolicies(ctx context.Context, t *platformv1alpha1.Tenant) error {
	platformNS := r.PlatformNamespace
	if platformNS == "" {
		platformNS = "mtkp-platform"
	}

	policies := []*networkingv1.NetworkPolicy{
		buildDenyAll(t.Spec.Namespace),
		buildAllowWithinNamespace(t.Spec.Namespace),
		buildAllowPlatformIngress(t.Spec.Namespace, platformNS),
	}

	for _, desired := range policies {
		np := &networkingv1.NetworkPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: desired.Name, Namespace: desired.Namespace},
		}
		spec := desired.Spec
		if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, np, func() error {
			np.Spec = spec
			return controllerutil.SetControllerReference(t, np, r.Scheme)
		}); err != nil {
			return fmt.Errorf("networkpolicy %s: %w", desired.Name, err)
		}
	}
	return nil
}

func buildDenyAll(ns string) *networkingv1.NetworkPolicy {
	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "default-deny-all", Namespace: ns},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{},
			PolicyTypes: []networkingv1.PolicyType{
				networkingv1.PolicyTypeIngress,
				networkingv1.PolicyTypeEgress,
			},
		},
	}
}

func buildAllowWithinNamespace(ns string) *networkingv1.NetworkPolicy {
	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "allow-within-namespace", Namespace: ns},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{},
			PolicyTypes: []networkingv1.PolicyType{
				networkingv1.PolicyTypeIngress,
				networkingv1.PolicyTypeEgress,
			},
			Ingress: []networkingv1.NetworkPolicyIngressRule{{
				From: []networkingv1.NetworkPolicyPeer{{
					PodSelector: &metav1.LabelSelector{},
				}},
			}},
			Egress: []networkingv1.NetworkPolicyEgressRule{{
				To: []networkingv1.NetworkPolicyPeer{{
					PodSelector: &metav1.LabelSelector{},
				}},
			}},
		},
	}
}

func buildAllowPlatformIngress(ns, platformNS string) *networkingv1.NetworkPolicy {
	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "allow-platform-ingress", Namespace: ns},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{{
				From: []networkingv1.NetworkPolicyPeer{{
					NamespaceSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"kubernetes.io/metadata.name": platformNS},
					},
				}},
			}},
		},
	}
}

// reconcileRBAC creates a RoleBinding granting the tenant owners the cluster's `edit` ClusterRole.
// Using `edit` keeps the controller simple — owners get standard developer permissions and the
// platform's own ClusterRoles can layer more if needed.
func (r *Reconciler) reconcileRBAC(ctx context.Context, t *platformv1alpha1.Tenant) error {
	rb := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "tenant-owners", Namespace: t.Spec.Namespace},
	}
	subjects := make([]rbacv1.Subject, len(t.Spec.Owners))
	copy(subjects, t.Spec.Owners)
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, rb, func() error {
		rb.Subjects = subjects
		rb.RoleRef = rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "ClusterRole",
			Name:     "edit",
		}
		return controllerutil.SetControllerReference(t, rb, r.Scheme)
	}); err != nil {
		return fmt.Errorf("rolebinding: %w", err)
	}
	return nil
}

// reconcileServiceAccount creates the default tenant ServiceAccount with the IRSA annotation
// (if spec.irsaRoleArn is set).
func (r *Reconciler) reconcileServiceAccount(ctx context.Context, t *platformv1alpha1.Tenant) error {
	sa := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Name: "tenant", Namespace: t.Spec.Namespace},
	}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, sa, func() error {
		if sa.Annotations == nil {
			sa.Annotations = map[string]string{}
		}
		if t.Spec.IRSARoleArn != "" {
			sa.Annotations["eks.amazonaws.com/role-arn"] = t.Spec.IRSARoleArn
		} else {
			delete(sa.Annotations, "eks.amazonaws.com/role-arn")
		}
		return controllerutil.SetControllerReference(t, sa, r.Scheme)
	}); err != nil {
		return fmt.Errorf("serviceaccount: %w", err)
	}
	return nil
}

// externalSecretGVK is the GroupVersionKind for ExternalSecrets. We use unstructured
// to avoid taking a hard dependency on the external-secrets module.
var externalSecretGVK = schema.GroupVersionKind{
	Group:   "external-secrets.io",
	Version: "v1beta1",
	Kind:    "ExternalSecret",
}

// reconcileExternalSecrets writes one ExternalSecret per spec.secrets entry.
func (r *Reconciler) reconcileExternalSecrets(ctx context.Context, t *platformv1alpha1.Tenant) error {
	for _, s := range t.Spec.Secrets {
		es := &unstructured.Unstructured{}
		es.SetGroupVersionKind(externalSecretGVK)
		es.SetName(s.Name)
		es.SetNamespace(t.Spec.Namespace)

		if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, es, func() error {
			storeRef := s.SecretStoreRef
			if storeRef == "" {
				storeRef = "platform-secret-store"
			}
			spec := map[string]interface{}{
				"refreshInterval": "1h",
				"secretStoreRef": map[string]interface{}{
					"name": storeRef,
					"kind": "ClusterSecretStore",
				},
				"target": map[string]interface{}{
					"name":           s.Name,
					"creationPolicy": "Owner",
				},
				"data": []interface{}{
					map[string]interface{}{
						"secretKey": s.Name,
						"remoteRef": map[string]interface{}{
							"key": s.RemoteRef,
						},
					},
				},
			}
			es.Object["spec"] = spec
			return controllerutil.SetControllerReference(t, es, r.Scheme)
		}); err != nil {
			return fmt.Errorf("externalsecret %s: %w", s.Name, err)
		}
	}
	return nil
}

// appProjectGVK / applicationGVK reference the ArgoCD CRDs.
var (
	appProjectGVK = schema.GroupVersionKind{
		Group:   "argoproj.io",
		Version: "v1alpha1",
		Kind:    "AppProject",
	}
	applicationGVK = schema.GroupVersionKind{
		Group:   "argoproj.io",
		Version: "v1alpha1",
		Kind:    "Application",
	}
)

// reconcileAppProject creates a tenant-scoped AppProject. It restricts source repos
// to the tenant's DeployRepo and destinations to the tenant namespace.
func (r *Reconciler) reconcileAppProject(ctx context.Context, t *platformv1alpha1.Tenant) error {
	argoNS := r.ArgoCDNamespace
	if argoNS == "" {
		argoNS = "argocd"
	}

	proj := &unstructured.Unstructured{}
	proj.SetGroupVersionKind(appProjectGVK)
	proj.SetName(t.Name)
	proj.SetNamespace(argoNS)

	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, proj, func() error {
		spec := map[string]interface{}{
			"description": fmt.Sprintf("Tenant project for %s", t.Spec.DisplayName),
			"sourceRepos": []interface{}{t.Spec.DeployRepo},
			"destinations": []interface{}{
				map[string]interface{}{
					"namespace": t.Spec.Namespace,
					"server":    "https://kubernetes.default.svc",
				},
			},
			"clusterResourceWhitelist": []interface{}{
				map[string]interface{}{"group": "", "kind": "Namespace"},
			},
			"namespaceResourceWhitelist": []interface{}{
				map[string]interface{}{"group": "*", "kind": "*"},
			},
		}
		proj.Object["spec"] = spec
		// AppProjects live in the argocd namespace, not under the tenant.
		// We don't set controller reference (the parent is cluster-scoped and
		// in a different namespace); instead we rely on the finalizer to clean up.
		return nil
	}); err != nil {
		return fmt.Errorf("appproject: %w", err)
	}
	return nil
}

// reconcileApplication creates the ArgoCD Application that watches the tenant's deploy repo.
func (r *Reconciler) reconcileApplication(ctx context.Context, t *platformv1alpha1.Tenant) error {
	argoNS := r.ArgoCDNamespace
	if argoNS == "" {
		argoNS = "argocd"
	}

	app := &unstructured.Unstructured{}
	app.SetGroupVersionKind(applicationGVK)
	app.SetName(t.Name)
	app.SetNamespace(argoNS)

	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, app, func() error {
		ref := t.Spec.DeployRef
		if ref == "" {
			ref = "HEAD"
		}
		spec := map[string]interface{}{
			"project": t.Name,
			"source": map[string]interface{}{
				"repoURL":        t.Spec.DeployRepo,
				"targetRevision": ref,
				"path":           ".",
			},
			"destination": map[string]interface{}{
				"namespace": t.Spec.Namespace,
				"server":    "https://kubernetes.default.svc",
			},
			"syncPolicy": map[string]interface{}{
				"automated": map[string]interface{}{
					"prune":    true,
					"selfHeal": true,
				},
			},
		}
		app.Object["spec"] = spec
		return nil
	}); err != nil {
		return fmt.Errorf("application: %w", err)
	}
	return nil
}

// reconcileIngress provisions a default Ingress when spec.ingress is set.
func (r *Reconciler) reconcileIngress(ctx context.Context, t *platformv1alpha1.Tenant) error {
	in := t.Spec.Ingress

	ingress := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Name: "tenant-default", Namespace: t.Spec.Namespace},
	}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, ingress, func() error {
		pathType := networkingv1.PathTypePrefix
		svc := in.ServiceName
		if svc == "" {
			svc = t.Name
		}
		port := in.ServicePort
		if port == 0 {
			port = 80
		}
		className := in.IngressClass
		if className == "" {
			className = "alb"
		}
		ingress.Annotations = mergeAnnotations(ingress.Annotations, in.Annotations)
		ingress.Spec = networkingv1.IngressSpec{
			IngressClassName: &className,
			Rules: []networkingv1.IngressRule{{
				Host: in.Host,
				IngressRuleValue: networkingv1.IngressRuleValue{
					HTTP: &networkingv1.HTTPIngressRuleValue{
						Paths: []networkingv1.HTTPIngressPath{{
							Path:     "/",
							PathType: &pathType,
							Backend: networkingv1.IngressBackend{
								Service: &networkingv1.IngressServiceBackend{
									Name: svc,
									Port: networkingv1.ServiceBackendPort{
										Number: port,
									},
								},
							},
						}},
					},
				},
			}},
		}
		return controllerutil.SetControllerReference(t, ingress, r.Scheme)
	}); err != nil {
		return fmt.Errorf("ingress: %w", err)
	}
	return nil
}

func mergeAnnotations(existing, incoming map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range existing {
		out[k] = v
	}
	for k, v := range incoming {
		out[k] = v
	}
	return out
}

// sanitizeLabel makes an arbitrary string safe for use as a Kubernetes label value:
// alphanumeric, dash, dot, underscore, max 63 chars.
func sanitizeLabel(s string) string {
	if len(s) > 63 {
		s = s[:63]
	}
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '-', c == '.', c == '_':
			out = append(out, c)
		case c == ' ':
			out = append(out, '-')
		}
	}
	return string(out)
}

// helpful unused-suppressor symbols so linters don't complain when an extension
// package wants to reach for these helpers.
var _ = client.IgnoreNotFound
var _ = intstr.FromInt
var _ = prometheus.NewCounter
