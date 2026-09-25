package apiclient

import (
	"encoding/json"

	"github.com/google/uuid"

	"github.com/Teamtem-dev/kuben/internal/core/opt"
)

// The request and response bodies the client exchanges, written out as the
// Rust handlers' DTOs serialize them (field order, null versus omitted):
// `kuben apps --json` and `kuben status --json` print them again, and their
// output is part of the contract.

// ProjectDto is one project (routes/projects.rs ProjectDto).
type ProjectDto struct {
	Name         string          `json:"name"`
	UID          opt.Val[string] `json:"uid"`
	DisplayName  string          `json:"display_name"`
	Description  opt.Val[string] `json:"description"`
	Org          opt.Val[string] `json:"org"`
	Environments uint32          `json:"environments"`
	Ready        bool            `json:"ready"`
	Deleting     bool            `json:"deleting"`
	CreatedAt    opt.Val[string] `json:"created_at"`
}

// EnvironmentDto is one environment of a project (routes/environments.rs).
type EnvironmentDto struct {
	// Name is the short name used in URLs, e.g. `prod`.
	Name string `json:"name"`
	// ResourceName is the Kubernetes object name, e.g. `shop-prod`.
	ResourceName        string          `json:"resource_name"`
	Project             string          `json:"project"`
	EnvType             string          `json:"env_type"`
	Namespace           string          `json:"namespace"`
	Phase               opt.Val[string] `json:"phase"`
	Ready               bool            `json:"ready"`
	Message             opt.Val[string] `json:"message"`
	Deleting            bool            `json:"deleting"`
	DeletionScheduledAt opt.Val[string] `json:"deletion_scheduled_at"`
	CreatedAt           opt.Val[string] `json:"created_at"`
}

// ProcessDto is one process of an app (routes/apps/mod.rs ProcessDto).
type ProcessDto struct {
	Name        string          `json:"name"`
	Command     []string        `json:"command"`
	Port        opt.Val[uint16] `json:"port"`
	Size        string          `json:"size"`
	MinReplicas uint32          `json:"min_replicas"`
	MaxReplicas uint32          `json:"max_replicas"`
	// Schedule is the cron expression of a scheduled process.
	Schedule opt.Val[string] `json:"schedule"`
	// Protocol is `http` or `tcp`.
	Protocol string `json:"protocol"`
}

// SecretRef names a key of a secret.
type SecretRef struct {
	Name string `json:"name"`
	Key  string `json:"key"`
}

// EnvVarDto is an environment variable: a plain value or a secret
// reference; both are left out when absent.
type EnvVarDto struct {
	Name   string             `json:"name"`
	Value  opt.Val[string]    `json:"value,omitzero"`
	Secret opt.Val[SecretRef] `json:"secret,omitzero"`
}

// VolumeDto is a persistent volume of an app.
type VolumeDto struct {
	Name      string `json:"name"`
	MountPath string `json:"mount_path"`
	// Size is a capacity such as `5Gi`; `1Gi` when the answer leaves it out
	// (serde default).
	Size string `json:"size"`
}

// UnmarshalJSON gives a volume without a size the default of 1Gi.
func (v *VolumeDto) UnmarshalJSON(data []byte) error {
	type plain VolumeDto
	p := plain{Size: "1Gi"}
	if err := json.Unmarshal(data, &p); err != nil {
		return err //nolint:wrapcheck // the caller names the answer
	}
	*v = VolumeDto(p)
	return nil
}

// HostDto is one hostname of an app.
type HostDto struct {
	Host               string          `json:"host"`
	TLS                string          `json:"tls"`
	CertificateReady   opt.Val[bool]   `json:"certificate_ready"`
	CertificateMessage opt.Val[string] `json:"certificate_message"`
}

// ExposureDto is how an app is reached through the gateway.
type ExposureDto struct {
	Routed  opt.Val[bool]   `json:"routed"`
	Message opt.Val[string] `json:"message"`
	Hosts   []HostDto       `json:"hosts"`
}

// AppDto is an app and its state (routes/apps/mod.rs AppDto).
type AppDto struct {
	Name        string          `json:"name"`
	Project     string          `json:"project"`
	Environment string          `json:"environment"`
	Namespace   string          `json:"namespace"`
	Image       opt.Val[string] `json:"image"`
	GitRepo     opt.Val[string] `json:"git_repo"`
	URL         opt.Val[string] `json:"url"`
	Ready       bool            `json:"ready"`
	// Reason is `Available`, `Progressing`, `RolloutFailed`, …
	Reason    opt.Val[string]      `json:"reason"`
	Message   opt.Val[string]      `json:"message"`
	Processes []ProcessDto         `json:"processes"`
	Env       []EnvVarDto          `json:"env"`
	Domains   []string             `json:"domains"`
	Volumes   []VolumeDto          `json:"volumes"`
	CreatedAt opt.Val[string]      `json:"created_at"`
	Exposure  opt.Val[ExposureDto] `json:"exposure"`
	// Paused is why delivery is paused, while it is; left out otherwise.
	Paused opt.Val[string] `json:"paused,omitzero"`
}

// PodDto is one pod of an app.
type PodDto struct {
	Name    string          `json:"name"`
	Process opt.Val[string] `json:"process"`
	// Phase is `pending`, `running`, `succeeded`, `failed` or `unknown`.
	Phase     string          `json:"phase"`
	Ready     bool            `json:"ready"`
	Restarts  int32           `json:"restarts"`
	Reason    opt.Val[string] `json:"reason"`
	Node      opt.Val[string] `json:"node"`
	StartedAt opt.Val[string] `json:"started_at"`
}

// AppDetail is an app with its pods.
type AppDetail struct {
	App  AppDto   `json:"app"`
	Pods []PodDto `json:"pods"`
}

// ReleaseDto is one revision in an app's history (routes/apps/releases.rs).
type ReleaseDto struct {
	Revision int64           `json:"revision"`
	Image    opt.Val[string] `json:"image"`
	// Reason is `create`, `deploy`, `config`, `rollback`, `promote`, …
	Reason string          `json:"reason"`
	Note   opt.Val[string] `json:"note"`
	// Actor is the email of whoever made the change.
	Actor     opt.Val[string] `json:"actor"`
	CreatedAt int64           `json:"created_at"`
	// Current marks the newest revision (what should be running).
	Current bool `json:"current"`
}

// rollbackRequest is the body of POST …/rollback.
type rollbackRequest struct {
	Revision int64 `json:"revision"`
}

// DoctorCheck is one check of an app's Doctor.
type DoctorCheck struct {
	ID string `json:"id"`
	// Subject is what was checked, when there are several; may be empty.
	Subject string `json:"subject"`
	// Status is `ok`, `warn`, `unknown` or `fail`.
	Status string          `json:"status"`
	Detail string          `json:"detail"`
	Hint   opt.Val[string] `json:"hint"`
}

// DoctorReport is an app's Doctor (routes/apps/doctor.rs).
type DoctorReport struct {
	Status string        `json:"status"`
	Checks []DoctorCheck `json:"checks"`
	// Graph and Findings are opaque JSON (serde_json::Value); absent, they
	// are null, as serde reads a missing Value.
	Graph    json.RawMessage `json:"graph"`
	Findings json.RawMessage `json:"findings"`
}

// DeployReason is why a run exists.
type DeployReason string

// The reasons a deployment can be started for.
const (
	ReasonDeploy    DeployReason = "deploy"
	ReasonRollback  DeployReason = "rollback"
	ReasonPromotion DeployReason = "promotion"
)

// StartDeploymentRequest is the body of POST …/deployments. Absent values
// are sent as null, as serde wrote them.
type StartDeploymentRequest struct {
	Image              opt.Val[string]          `json:"image"`
	Release            opt.Val[uuid.UUID]       `json:"release"`
	Config             opt.Val[json.RawMessage] `json:"config"`
	Reason             DeployReason             `json:"reason"`
	ExpectedGeneration uint64                   `json:"expected_generation"`
}

// DeploymentDto is a deployment run and where it stands.
type DeploymentDto struct {
	Run        uuid.UUID `json:"run"`
	Operation  uuid.UUID `json:"operation"`
	Generation uint64    `json:"generation"`
	// Phase is `planned`, `awaitingApproval`, …, `succeeded`, `failed`, …
	Phase             string          `json:"phase"`
	ApprovalsRequired uint8           `json:"approvals_required"`
	ApprovalExpiresAt opt.Val[int64]  `json:"approval_expires_at"`
	PlanHash          opt.Val[string] `json:"plan_hash"`
	Warnings          []string        `json:"warnings,omitempty"`
}

// PodLogs is the log of one pod.
type PodLogs struct {
	Pod     string          `json:"pod"`
	Process opt.Val[string] `json:"process"`
	Lines   []string        `json:"lines"`
	// Error is why no logs could be read.
	Error opt.Val[string] `json:"error"`
}

// LogLine is one line of a followed log.
type LogLine struct {
	Pod     string          `json:"pod"`
	Process opt.Val[string] `json:"process"`
	Time    opt.Val[string] `json:"time"`
	Line    string          `json:"line"`
}

// LogEnd says that a followed log stopped: one pod's, or all of them when
// Pod is absent.
type LogEnd struct {
	Pod   opt.Val[string] `json:"pod"`
	Error opt.Val[string] `json:"error"`
}
