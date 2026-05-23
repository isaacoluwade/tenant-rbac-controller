package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	rbacv1 "k8s.io/api/rbac/v1"
)

// SecretRef points to an external secret the tenant wants synced into their namespace.
type SecretRef struct {
	// Name of the ExternalSecret/Kubernetes Secret to create.
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// RemoteRef is the path/key in the upstream secret store (Secrets Manager, Vault, etc.).
	// +kubebuilder:validation:Required
	RemoteRef string `json:"remoteRef"`

	// SecretStoreRef is the name of the ClusterSecretStore to read from.
	// Defaults to "platform-secret-store".
	// +kubebuilder:default="platform-secret-store"
	SecretStoreRef string `json:"secretStoreRef,omitempty"`
}

// IngressSpec describes the default Ingress to create for the tenant namespace.
type IngressSpec struct {
	// Host is the FQDN to expose.
	// +kubebuilder:validation:Required
	Host string `json:"host"`

	// ServiceName is the backing service. Defaults to the tenant name.
	ServiceName string `json:"serviceName,omitempty"`

	// ServicePort to route traffic to.
	// +kubebuilder:default=80
	ServicePort int32 `json:"servicePort,omitempty"`

	// IngressClass to use. Defaults to "alb".
	// +kubebuilder:default="alb"
	IngressClass string `json:"ingressClass,omitempty"`

	// Annotations to merge onto the Ingress (e.g. ALB controller annotations).
	Annotations map[string]string `json:"annotations,omitempty"`
}

// Quotas describes the ResourceQuota and LimitRange for the tenant namespace.
type Quotas struct {
	// +kubebuilder:default="10"
	CPURequests string `json:"cpuRequests,omitempty"`
	// +kubebuilder:default="20"
	CPULimits string `json:"cpuLimits,omitempty"`
	// +kubebuilder:default="10Gi"
	MemoryRequests string `json:"memoryRequests,omitempty"`
	// +kubebuilder:default="20Gi"
	MemoryLimits string `json:"memoryLimits,omitempty"`
	// +kubebuilder:default="100"
	Pods string `json:"pods,omitempty"`

	// LimitRange defaults applied to every container in the namespace.
	// +kubebuilder:default="500m"
	DefaultLimitCPU string `json:"defaultLimitCpu,omitempty"`
	// +kubebuilder:default="512Mi"
	DefaultLimitMemory string `json:"defaultLimitMemory,omitempty"`
	// +kubebuilder:default="100m"
	DefaultRequestCPU string `json:"defaultRequestCpu,omitempty"`
	// +kubebuilder:default="128Mi"
	DefaultRequestMemory string `json:"defaultRequestMemory,omitempty"`
}

// TenantSpec defines the desired state of a Tenant.
type TenantSpec struct {
	// DisplayName is the human-friendly name of the tenant.
	// +kubebuilder:validation:Required
	DisplayName string `json:"displayName"`

	// Namespace is the namespace to create for the tenant.
	// Defaults to .metadata.name when omitted.
	Namespace string `json:"namespace,omitempty"`

	// Owners is the list of RBAC Subjects granted edit access to the tenant namespace.
	// +kubebuilder:validation:MinItems=1
	Owners []rbacv1.Subject `json:"owners"`

	// Quotas controls the ResourceQuota and LimitRange for the namespace.
	Quotas Quotas `json:"quotas"`

	// Secrets is the list of ExternalSecrets to materialize in the namespace.
	Secrets []SecretRef `json:"secrets,omitempty"`

	// Ingress describes a default Ingress to provision. Optional.
	Ingress *IngressSpec `json:"ingress,omitempty"`

	// IRSARoleArn is the AWS IAM role ARN to annotate onto the tenant ServiceAccount.
	IRSARoleArn string `json:"irsaRoleArn,omitempty"`

	// DeployRepo is the Git repository ArgoCD should watch for the tenant's manifests.
	// +kubebuilder:validation:Required
	DeployRepo string `json:"deployRepo"`

	// DeployRef is the Git ref (branch/tag) ArgoCD should track. Defaults to HEAD.
	// +kubebuilder:default="HEAD"
	DeployRef string `json:"deployRef,omitempty"`
}

// TenantStatus reflects the observed state of a Tenant.
type TenantStatus struct {
	// Conditions hold per-concern readiness signals: NamespaceReady, RBACReady,
	// NetworkPolicyReady, QuotaReady, SecretsReady, IngressReady, AppProjectReady, Ready.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// Namespace is the observed namespace name (echo of spec or default).
	Namespace string `json:"namespace,omitempty"`

	// Phase is a coarse summary: Pending / Provisioning / Ready / Failed / Terminating.
	Phase string `json:"phase,omitempty"`

	// ObservedGeneration is the .metadata.generation last successfully reconciled.
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// Phase constants surfaced on Tenant.Status.Phase.
const (
	PhasePending      = "Pending"
	PhaseProvisioning = "Provisioning"
	PhaseReady        = "Ready"
	PhaseFailed       = "Failed"
	PhaseTerminating  = "Terminating"
)

// Condition type constants for Tenant.Status.Conditions.
const (
	ConditionNamespaceReady     = "NamespaceReady"
	ConditionRBACReady          = "RBACReady"
	ConditionNetworkPolicyReady = "NetworkPolicyReady"
	ConditionQuotaReady         = "QuotaReady"
	ConditionSecretsReady       = "SecretsReady"
	ConditionIngressReady       = "IngressReady"
	ConditionAppProjectReady    = "AppProjectReady"
	ConditionReady              = "Ready"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=tn
// +kubebuilder:printcolumn:name="Display",type=string,JSONPath=`.spec.displayName`
// +kubebuilder:printcolumn:name="Namespace",type=string,JSONPath=`.status.namespace`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Tenant is the Schema for the tenants API.
type Tenant struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   TenantSpec   `json:"spec,omitempty"`
	Status TenantStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// TenantList contains a list of Tenant.
type TenantList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Tenant `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Tenant{}, &TenantList{})
}
