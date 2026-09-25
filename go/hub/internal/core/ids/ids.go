// Package ids has Kuben's identifiers: UUIDv7 (time ordered, so they index
// well in PostgreSQL B-trees), one distinct Go type per kind of thing, so a
// release id cannot be passed where a target id is wanted.
package ids

import (
	"database/sql/driver"
	"fmt"

	"github.com/google/uuid"
)

// The kinds of thing an id can name: each type is the kind of its ids.
type (
	// User is a user account.
	User struct{}
	// Org is an organization (tenant).
	Org struct{}
	// Audit is an audit event.
	Audit struct{}
	// Token is an API token.
	Token struct{}
	// Target is one application on one environment placement (ADR-026).
	Target struct{}
	// Release is an immutable, portable release: artifact digests plus portable config.
	Release struct{}
	// DeploymentRun is one attempt to make a release effective on one target.
	DeploymentRun struct{}
	// BuildAttempt is one build attempt; an infrastructure retry is a new attempt.
	BuildAttempt struct{}
	// SourceBinding is a target's binding to one Git repository and branch (M3).
	SourceBinding struct{}
	// Project is a project: owns applications and environments.
	Project struct{}
	// Environment is a logical environment such as staging or production (ADR-026).
	Environment struct{}
	// Cluster is a Kubernetes cluster registered with Kuben.
	Cluster struct{}
	// Placement is an environment's binding to one cluster and namespace (ADR-026).
	Placement struct{}
	// Application is an application definition in a project.
	Application struct{}
	// Operation is a durable operation: one accepted request and its execution (plan §9.2).
	Operation struct{}
	// ConfigRevision is one immutable configuration revision of a target (ADR-026).
	ConfigRevision struct{}
	// RenderPlan is a frozen, content-addressed render plan (ADR-026, I22).
	RenderPlan struct{}
)

// Kind is closed: only the kinds above name ids.
type Kind interface {
	User | Org | Audit | Token | Target | Release | DeploymentRun | BuildAttempt | SourceBinding |
		Project | Environment | Cluster | Placement | Application | Operation | ConfigRevision | RenderPlan
}

// The id types, by kind.
type (
	// UserID is an id of kind User.
	UserID = ID[User]
	// OrgID is an id of kind Org.
	OrgID = ID[Org]
	// AuditID is an id of kind Audit.
	AuditID = ID[Audit]
	// TokenID is an id of kind Token.
	TokenID = ID[Token]
	// TargetID is an id of kind Target.
	TargetID = ID[Target]
	// ReleaseID is an id of kind Release.
	ReleaseID = ID[Release]
	// DeploymentRunID is an id of kind DeploymentRun.
	DeploymentRunID = ID[DeploymentRun]
	// BuildAttemptID is an id of kind BuildAttempt.
	BuildAttemptID = ID[BuildAttempt]
	// SourceBindingID is an id of kind SourceBinding.
	SourceBindingID = ID[SourceBinding]
	// ProjectID is an id of kind Project.
	ProjectID = ID[Project]
	// EnvironmentID is an id of kind Environment.
	EnvironmentID = ID[Environment]
	// ClusterID is an id of kind Cluster.
	ClusterID = ID[Cluster]
	// PlacementID is an id of kind Placement.
	PlacementID = ID[Placement]
	// ApplicationID is an id of kind Application.
	ApplicationID = ID[Application]
	// OperationID is an id of kind Operation.
	OperationID = ID[Operation]
	// ConfigRevisionID is an id of kind ConfigRevision.
	ConfigRevisionID = ID[ConfigRevision]
	// RenderPlanID is an id of kind RenderPlan.
	RenderPlanID = ID[RenderPlan]
)

// ID names one thing of kind K. It is comparable, a valid map key, text in
// JSON and a uuid in PostgreSQL. The zero ID is the nil UUID.
type ID[K Kind] struct{ u uuid.UUID }

// New is a fresh, time-ordered id.
func New[K Kind]() ID[K] { return ID[K]{u: uuid.Must(uuid.NewV7())} }

// From names the thing whose UUID is u.
func From[K Kind](u uuid.UUID) ID[K] { return ID[K]{u: u} }

// Parse reads the text form of an id.
func Parse[K Kind](s string) (ID[K], error) {
	u, err := uuid.Parse(s)
	if err != nil {
		return ID[K]{}, fmt.Errorf("id %q: %w", s, err)
	}
	return ID[K]{u: u}, nil
}

// UUID is the id without its kind.
func (id ID[K]) UUID() uuid.UUID { return id.u }

// IsNil reports whether the id is the zero id.
func (id ID[K]) IsNil() bool { return id.u == uuid.Nil }

// Compare orders ids by their bytes, which for UUIDv7 is by creation time.
func (id ID[K]) Compare(other ID[K]) int {
	for i := range id.u {
		if id.u[i] != other.u[i] {
			if id.u[i] < other.u[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

func (id ID[K]) String() string { return id.u.String() }

// MarshalText makes the id a JSON string and a usable map key in JSON.
func (id ID[K]) MarshalText() ([]byte, error) { return id.u.MarshalText() }

// UnmarshalText reads what MarshalText wrote.
func (id *ID[K]) UnmarshalText(text []byte) error { return id.u.UnmarshalText(text) }

// Value stores the id as a uuid.
func (id ID[K]) Value() (driver.Value, error) { return id.u.Value() }

// Scan reads a uuid column.
func (id *ID[K]) Scan(src any) error { return id.u.Scan(src) }
