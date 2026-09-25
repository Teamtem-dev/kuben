package v1alpha1

import (
	"encoding/json"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// App is the unit of deployment. It is namespaced: it lives in its
// Environment's namespace. Secrets are referenced, never inlined (Release
// snapshots hold Secret references, not values).
//
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,shortName=kapp
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type==\"Ready\")].status"
// +kubebuilder:printcolumn:name="Release",type="string",JSONPath=".status.currentRelease"
// +kubebuilder:printcolumn:name="URL",type="string",JSONPath=".status.url"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
type App struct {
	metav1.TypeMeta `json:",inline"`
	// Standard object metadata.
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// Spec is what the user asked for.
	Spec AppSpec `json:"spec"`
	// Status is what the controller observed; nil until it wrote one.
	Status *AppStatus `json:"status,omitempty"`
}

// AppList is a list of App.
//
// +kubebuilder:object:root=true
type AppList struct {
	metav1.TypeMeta `json:",inline"`
	// Standard list metadata.
	metav1.ListMeta `json:"metadata,omitempty"`
	// Items are the listed objects.
	Items []App `json:"items"`
}

// AppSpec is the desired state of an App.
type AppSpec struct {
	// Source says where the image comes from.
	Source Source `json:"source"`
	// Runtime has the processes and how they run.
	Runtime Runtime `json:"runtime"`
	// Env is the processes' environment.
	Env []EnvVar `json:"env,omitempty"`
	// Domains are the hostnames routed to the app's HTTP process.
	Domains []Domain `json:"domains,omitempty"`
	// Volumes are persistent volumes (scenario 6). An app with volumes runs
	// a single process with at most one replica (ReadWriteOnce).
	Volumes []Volume `json:"volumes,omitempty"`
	// ImagePullSecrets are Secrets of the app's namespace the kubelet pulls
	// its image with. Kuben sets them from the environment's registry
	// credentials.
	ImagePullSecrets []string `json:"imagePullSecrets,omitempty"`
}

// Volume is a persistent volume mounted into the app's process. It is
// backed by a PVC named <app>-<name> that is retained when the app is
// deleted.
type Volume struct {
	// Name is the volume's name within the app.
	Name string `json:"name"`
	// MountPath is an absolute path inside the container, e.g. "/data".
	MountPath string `json:"mountPath"`
	// Size is the requested capacity, e.g. "5Gi". It can grow, never
	// shrink. Decoding defaults it to DefaultVolumeSize.
	// +optional
	// +kubebuilder:default="1Gi"
	Size string `json:"size"`
	// StorageClass is the PVC's StorageClass; the cluster default when nil.
	StorageClass *string `json:"storageClass,omitempty"`
}

// DefaultVolumeSize is the capacity of a volume that does not name one.
const DefaultVolumeSize = "1Gi"

// UnmarshalJSON applies the serde defaults of the Rust type.
func (v *Volume) UnmarshalJSON(data []byte) error {
	type plain Volume
	out := plain{Size: DefaultVolumeSize}
	if err := json.Unmarshal(data, &out); err != nil {
		return err
	}
	*v = Volume(out)
	return nil
}

// Protocol says how a process's port is exposed. The zero value means
// ProtocolHTTP, the Rust default.
//
// +kubebuilder:validation:Enum=http;tcp
type Protocol string

// Protocols.
const (
	// ProtocolHTTP routes the port through the Gateway (HTTPRoute) on the
	// app's hostnames.
	ProtocolHTTP Protocol = "http"
	// ProtocolTCP is cluster-internal TCP (databases, caches): a Service on
	// the real port, never a public route.
	ProtocolTCP Protocol = "tcp"
)

// Valid reports whether p is one of the Protocol constants.
func (p Protocol) Valid() bool {
	switch p {
	case ProtocolHTTP, ProtocolTCP:
		return true
	}
	return false
}

// IsHTTP reports whether p is ProtocolHTTP or the zero value, which stands
// for it.
func (p Protocol) IsHTTP() bool { return p == "" || p == ProtocolHTTP }

// ParseProtocol returns the Protocol spelled text.
func ParseProtocol(text string) (Protocol, error) {
	return parseEnum(text, "protocol", Protocol.Valid)
}

// MarshalJSON writes the wire string; the zero value is "http".
func (p Protocol) MarshalJSON() ([]byte, error) {
	return marshalEnum(p, ProtocolHTTP, "protocol", Protocol.Valid)
}

// UnmarshalJSON refuses unknown strings, as serde did.
func (p *Protocol) UnmarshalJSON(data []byte) error {
	return unmarshalEnum(data, p, "protocol", Protocol.Valid)
}

// Source says where the image comes from. Set exactly one of Image or Git
// (Kubernetes one-of style: structural CRD schemas cannot express tagged
// enums).
type Source struct {
	// Image is a prebuilt image, e.g. "ghcr.io/acme/api:1.2.3". It is pinned
	// to a digest at Release time.
	Image *string `json:"image,omitempty"`
	// Git builds the image from a git repository.
	Git *GitSource `json:"git,omitempty"`
}

// SourceFromImage returns a prebuilt-image source.
func SourceFromImage(image string) Source {
	return Source{Image: &image}
}

// GitSource is a git repository an image is built from.
type GitSource struct {
	// Repo is the repository URL.
	Repo string `json:"repo"`
	// Branch is the branch to build; decoding defaults it to "main".
	// +optional
	// +kubebuilder:default="main"
	Branch string `json:"branch"`
	// Path is the build context inside the repository; empty is the root
	// and is left out of the JSON.
	Path string `json:"path,omitempty"`
	// Build says how the image is built.
	// +optional
	// +kubebuilder:default={strategy:"auto"}
	Build Build `json:"build"`
}

// DefaultBranch is the branch of a git source that does not name one.
const DefaultBranch = "main"

// UnmarshalJSON applies the serde defaults of the Rust type.
func (g *GitSource) UnmarshalJSON(data []byte) error {
	type plain GitSource
	out := plain{Branch: DefaultBranch, Build: Build{Strategy: BuildStrategyAuto}}
	if err := json.Unmarshal(data, &out); err != nil {
		return err
	}
	*g = GitSource(out)
	return nil
}

// Build says how an image is built from source.
type Build struct {
	// Strategy picks the builder; decoding defaults it to
	// BuildStrategyAuto.
	// +optional
	// +kubebuilder:default="auto"
	Strategy BuildStrategy `json:"strategy"`
	// Dockerfile is the Dockerfile path relative to the source path
	// (strategy dockerfile).
	Dockerfile *string `json:"dockerfile,omitempty"`
}

// UnmarshalJSON applies the serde defaults of the Rust type.
func (b *Build) UnmarshalJSON(data []byte) error {
	type plain Build
	out := plain{Strategy: BuildStrategyAuto}
	if err := json.Unmarshal(data, &out); err != nil {
		return err
	}
	*b = Build(out)
	return nil
}

// BuildStrategy picks the image builder. The zero value means
// BuildStrategyAuto, the Rust default.
//
// +kubebuilder:validation:Enum=dockerfile;railpack;auto
type BuildStrategy string

// Build strategies.
const (
	// BuildStrategyAuto uses the Dockerfile if present, otherwise Railpack.
	BuildStrategyAuto BuildStrategy = "auto"
	// BuildStrategyDockerfile builds the Dockerfile.
	BuildStrategyDockerfile BuildStrategy = "dockerfile"
	// BuildStrategyRailpack builds with Railpack.
	BuildStrategyRailpack BuildStrategy = "railpack"
)

// Valid reports whether s is one of the BuildStrategy constants.
func (s BuildStrategy) Valid() bool {
	switch s {
	case BuildStrategyAuto, BuildStrategyDockerfile, BuildStrategyRailpack:
		return true
	}
	return false
}

// ParseBuildStrategy returns the BuildStrategy spelled text.
func ParseBuildStrategy(text string) (BuildStrategy, error) {
	return parseEnum(text, "build strategy", BuildStrategy.Valid)
}

// MarshalJSON writes the wire string; the zero value is "auto".
func (s BuildStrategy) MarshalJSON() ([]byte, error) {
	return marshalEnum(s, BuildStrategyAuto, "build strategy", BuildStrategy.Valid)
}

// UnmarshalJSON refuses unknown strings, as serde did.
func (s *BuildStrategy) UnmarshalJSON(data []byte) error {
	return unmarshalEnum(data, s, "build strategy", BuildStrategy.Valid)
}

// Runtime has an App's processes and how they run.
type Runtime struct {
	// Processes are the named processes (web, worker, ...). Exactly one may
	// expose a port.
	Processes map[string]Process `json:"processes"`
	// HealthCheck is the HTTP readiness check of the process with a port.
	HealthCheck *HealthCheck `json:"healthCheck,omitempty"`
	// FSGroup is the group id that owns the volumes (fsGroup), for images
	// running as a non-root user, e.g. 1000.
	FSGroup *int64 `json:"fsGroup,omitempty"`
}

// MarshalJSON writes a nil Processes as {}: the Rust map was always written.
func (r Runtime) MarshalJSON() ([]byte, error) {
	type plain Runtime
	out := plain(r)
	if out.Processes == nil {
		out.Processes = map[string]Process{}
	}
	return json.Marshal(out)
}

// Process is one named process of an App.
type Process struct {
	// Command overrides the image's command.
	Command []string `json:"command,omitempty"`
	// Port is the port the process listens on, if any.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=65535
	Port *uint16 `json:"port,omitempty"`
	// Size is a size preset name from KubenConfigSpec.Sizes; decoding
	// defaults it to DefaultProcessSize.
	// +optional
	// +kubebuilder:default="small"
	Size string `json:"size"`
	// Replicas bounds the replica count; decoding defaults both bounds
	// to 1.
	// +optional
	// +kubebuilder:default={min:1,max:1}
	Replicas Replicas `json:"replicas"`
	// Idle configures scale-to-zero and throttling.
	Idle *Idle `json:"idle,omitempty"`
	// Schedule is a cron expression (scenario 7). A scheduled process runs
	// as a CronJob instead of a Deployment and may not expose a port.
	Schedule *string `json:"schedule,omitempty"`
	// TimeZone is the IANA time zone of Schedule, e.g. "Europe/Berlin"
	// (default UTC).
	TimeZone *string `json:"timeZone,omitempty"`
	// Protocol says how Port is exposed. It is left out of the JSON when it
	// is http (or empty), as the Rust type did.
	Protocol Protocol `json:"protocol,omitempty"`
}

// DefaultProcessSize is the size preset of a process that does not name one.
const DefaultProcessSize = "small"

// MarshalJSON leaves Protocol out when it is http, as serde's
// skip_serializing_if = "Protocol::is_http" did.
func (p Process) MarshalJSON() ([]byte, error) {
	type plain Process
	out := plain(p)
	if out.Protocol.IsHTTP() {
		out.Protocol = ""
	}
	return json.Marshal(out)
}

// UnmarshalJSON applies the serde defaults of the Rust type.
func (p *Process) UnmarshalJSON(data []byte) error {
	type plain Process
	out := plain{Size: DefaultProcessSize, Replicas: DefaultReplicas(), Protocol: ProtocolHTTP}
	if err := json.Unmarshal(data, &out); err != nil {
		return err
	}
	*p = Process(out)
	return nil
}

// Replicas bounds a process's replica count.
type Replicas struct {
	// Min is the lower bound; decoding defaults it to 1.
	// +optional
	// +kubebuilder:default=1
	Min uint32 `json:"min"`
	// Max is the upper bound; decoding defaults it to 1.
	// +optional
	// +kubebuilder:default=1
	Max uint32 `json:"max"`
}

// DefaultReplicas is exactly one replica, the Rust Default.
func DefaultReplicas() Replicas { return Replicas{Min: 1, Max: 1} }

// UnmarshalJSON applies the serde defaults of the Rust type.
func (r *Replicas) UnmarshalJSON(data []byte) error {
	type plain Replicas
	out := plain(DefaultReplicas())
	if err := json.Unmarshal(data, &out); err != nil {
		return err
	}
	*r = Replicas(out)
	return nil
}

// Idle is the scale-to-zero / throttling configuration (Master Blueprint
// §3.3).
type Idle struct {
	// Mode is what happens when the process is idle; decoding defaults it
	// to IdleModeOff.
	// +optional
	// +kubebuilder:default="off"
	Mode IdleMode `json:"mode"`
	// After is the inactivity before sleeping, e.g. "15m"; decoding
	// defaults it to DefaultIdleAfter.
	// +optional
	// +kubebuilder:default="15m"
	After string `json:"after"`
}

// DefaultIdleAfter is the inactivity after which an idle process sleeps
// when Idle.After is not given.
const DefaultIdleAfter = "15m"

// UnmarshalJSON applies the serde defaults of the Rust type.
func (i *Idle) UnmarshalJSON(data []byte) error {
	type plain Idle
	out := plain{Mode: IdleModeOff, After: DefaultIdleAfter}
	if err := json.Unmarshal(data, &out); err != nil {
		return err
	}
	*i = Idle(out)
	return nil
}

// IdleMode is what happens to an idle process. The zero value means
// IdleModeOff, the Rust default.
//
// +kubebuilder:validation:Enum=off;zero;throttle
type IdleMode string

// Idle modes.
const (
	// IdleModeOff keeps the process running.
	IdleModeOff IdleMode = "off"
	// IdleModeZero scales the process to zero.
	IdleModeZero IdleMode = "zero"
	// IdleModeThrottle throttles the process.
	IdleModeThrottle IdleMode = "throttle"
)

// Valid reports whether m is one of the IdleMode constants.
func (m IdleMode) Valid() bool {
	switch m {
	case IdleModeOff, IdleModeZero, IdleModeThrottle:
		return true
	}
	return false
}

// ParseIdleMode returns the IdleMode spelled text.
func ParseIdleMode(text string) (IdleMode, error) {
	return parseEnum(text, "idle mode", IdleMode.Valid)
}

// MarshalJSON writes the wire string; the zero value is "off".
func (m IdleMode) MarshalJSON() ([]byte, error) {
	return marshalEnum(m, IdleModeOff, "idle mode", IdleMode.Valid)
}

// UnmarshalJSON refuses unknown strings, as serde did.
func (m *IdleMode) UnmarshalJSON(data []byte) error {
	return unmarshalEnum(data, m, "idle mode", IdleMode.Valid)
}

// HealthCheck is an HTTP readiness check.
type HealthCheck struct {
	// Path is the HTTP path probed, e.g. "/healthz".
	Path string `json:"path"`
	// Port is the probed port; the process's port when nil.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=65535
	Port *uint16 `json:"port,omitempty"`
}

// EnvVar is an environment variable. Set exactly one of Value, FromSecret
// and FromService.
type EnvVar struct {
	// Name is the variable's name.
	Name string `json:"name"`
	// Value is a literal value.
	Value *string `json:"value,omitempty"`
	// FromSecret takes the value from a Secret key.
	FromSecret *KeyRef `json:"fromSecret,omitempty"`
	// FromService takes the value from a Service key.
	FromService *KeyRef `json:"fromService,omitempty"`
}

// Domain is a hostname routed to an App.
type Domain struct {
	// Host is the hostname.
	Host string `json:"host"`
	// TLS is "auto" (cert-manager), "none", or a Secret name; decoding
	// defaults it to DefaultTLS.
	// +optional
	// +kubebuilder:default="auto"
	TLS string `json:"tls"`
}

// DefaultTLS is the TLS setting of a domain that does not name one.
const DefaultTLS = "auto"

// UnmarshalJSON applies the serde defaults of the Rust type.
func (d *Domain) UnmarshalJSON(data []byte) error {
	type plain Domain
	out := plain{TLS: DefaultTLS}
	if err := json.Unmarshal(data, &out); err != nil {
		return err
	}
	*d = Domain(out)
	return nil
}

// AppStatus is what the App controller observed.
type AppStatus struct {
	// ObservedGeneration is the metadata.generation last acted on.
	ObservedGeneration *int64 `json:"observedGeneration,omitempty"`
	// CurrentRelease is the name of the active Release.
	CurrentRelease *string `json:"currentRelease,omitempty"`
	// URL is where the app is reachable.
	URL *string `json:"url,omitempty"`
	// Conditions is always written, empty or not.
	// +optional
	Conditions Conditions `json:"conditions"`
}
