package v1alpha1

import (
	"encoding/json"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// KubenConfig is the cluster-scoped singleton holding platform settings.
// One resource replaces both the platform CR and a config.yaml file:
// settings live in the cluster, not on disk (Invariant I-17).
//
// +kubebuilder:object:root=true
// +kubebuilder:resource:path=kubenconfigs,scope=Cluster,shortName=kcfg
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="BaseDomain",type="string",JSONPath=".spec.baseDomain"
type KubenConfig struct {
	metav1.TypeMeta `json:",inline"`
	// Standard object metadata.
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// Spec is the platform settings.
	Spec KubenConfigSpec `json:"spec"`
	// Status is what the controller observed; nil until it wrote one.
	Status *KubenConfigStatus `json:"status,omitempty"`
}

// KubenConfigList is a list of KubenConfig.
//
// +kubebuilder:object:root=true
type KubenConfigList struct {
	metav1.TypeMeta `json:",inline"`
	// Standard list metadata.
	metav1.ListMeta `json:"metadata,omitempty"`
	// Items are the listed objects.
	Items []KubenConfig `json:"items"`
}

// KubenConfigSpec is the platform settings.
type KubenConfigSpec struct {
	// BaseDomain is the base domain of generated app hostnames, e.g.
	// "apps.example.com".
	BaseDomain *string `json:"baseDomain,omitempty"`
	// Gateway is the Gateway API Gateway used for HTTPRoutes
	// (namespace/name). With GatewayClassName it defaults to
	// "kuben-system/kuben".
	Gateway *string `json:"gateway,omitempty"`
	// GatewayClassName is the GatewayClass of the Gateway Kuben creates and
	// owns. Unset: Kuben uses the existing Gateway named in Gateway, and
	// writes its listeners only when it carries the label
	// kuben.dev/gateway-owner=kuben.
	GatewayClassName *string `json:"gatewayClassName,omitempty"`
	// GatewayPorts are the ports of the listeners Kuben writes on its
	// Gateway. Some Gateway controllers match listeners to their own entry
	// points: Traefik (and k3s's bundled Traefik) listens on 8000 and 8443.
	GatewayPorts *GatewayPorts `json:"gatewayPorts,omitempty"`
	// ClusterIssuer is the cert-manager ClusterIssuer name.
	ClusterIssuer *string `json:"clusterIssuer,omitempty"`
	// WildcardTLSSecret is a Secret in the Gateway's namespace holding a
	// certificate for *.<baseDomain> (DNS-01). When set, generated
	// hostnames share one wildcard HTTPS listener instead of one listener
	// per host.
	WildcardTLSSecret *string `json:"wildcardTlsSecret,omitempty"`
	// Registry is the container registry used for build outputs.
	Registry *RegistryCfg `json:"registry,omitempty"`
	// Sizes are the compute size presets; decoding defaults them to
	// DefaultSizes when the field is absent. An explicit empty list stays
	// empty, and a nil Sizes is written as [], as the Rust Vec was.
	// +optional
	Sizes []SizePreset `json:"sizes"`
}

// MarshalJSON writes a nil Sizes as []: the Rust Vec was always written.
func (s KubenConfigSpec) MarshalJSON() ([]byte, error) {
	type plain KubenConfigSpec
	out := plain(s)
	if out.Sizes == nil {
		out.Sizes = []SizePreset{}
	}
	return json.Marshal(out)
}

// UnmarshalJSON applies the serde defaults of the Rust type.
func (s *KubenConfigSpec) UnmarshalJSON(data []byte) error {
	type plain KubenConfigSpec
	out := plain{Sizes: DefaultSizes()}
	if err := json.Unmarshal(data, &out); err != nil {
		return err
	}
	*s = KubenConfigSpec(out)
	return nil
}

// GatewayPorts are the listener ports of Kuben's Gateway.
type GatewayPorts struct {
	// HTTP is the plain HTTP listener's port; decoding defaults it to 80.
	// +optional
	// +kubebuilder:default=80
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=65535
	HTTP uint16 `json:"http"`
	// HTTPS is the HTTPS listener's port; decoding defaults it to 443.
	// +optional
	// +kubebuilder:default=443
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=65535
	HTTPS uint16 `json:"https"`
}

// DefaultGatewayPorts are 80 and 443, the Rust Default.
func DefaultGatewayPorts() GatewayPorts { return GatewayPorts{HTTP: 80, HTTPS: 443} }

// UnmarshalJSON applies the serde defaults of the Rust type.
func (p *GatewayPorts) UnmarshalJSON(data []byte) error {
	type plain GatewayPorts
	out := plain(DefaultGatewayPorts())
	if err := json.Unmarshal(data, &out); err != nil {
		return err
	}
	*p = GatewayPorts(out)
	return nil
}

// RegistryCfg is the container registry build outputs are pushed to.
type RegistryCfg struct {
	// Prefix is the repository prefix, e.g. "ghcr.io/acme".
	Prefix string `json:"prefix"`
	// CredentialsSecret is a Secret in kuben-system holding a
	// .dockerconfigjson.
	CredentialsSecret *string `json:"credentialsSecret,omitempty"`
}

// KubenConfigStatus is what the platform controller observed.
type KubenConfigStatus struct {
	// ObservedGeneration is the metadata.generation last acted on.
	ObservedGeneration *int64 `json:"observedGeneration,omitempty"`
	// Conditions is always written, empty or not.
	// +optional
	Conditions Conditions `json:"conditions"`
}

// DefaultSizes returns the compute size presets a KubenConfig without sizes
// gets: nano, small, medium and large. Every call returns a new slice.
func DefaultSizes() []SizePreset {
	return []SizePreset{
		{Name: "nano", CPURequest: "50m", MemoryRequest: "64Mi", MemoryLimit: "128Mi"},
		{Name: "small", CPURequest: "100m", MemoryRequest: "128Mi", MemoryLimit: "256Mi"},
		{Name: "medium", CPURequest: "250m", MemoryRequest: "512Mi", MemoryLimit: "1Gi"},
		{Name: "large", CPURequest: "1", MemoryRequest: "2Gi", MemoryLimit: "4Gi"},
	}
}
