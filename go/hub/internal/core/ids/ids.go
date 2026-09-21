// Package ids has Kuben's identifiers: UUIDv7 (time ordered, so they index
// well in PostgreSQL B-trees), one distinct Go type per kind of thing, so a
// release id cannot be passed where a target id is wanted.
package ids

import (
	"database/sql/driver"
	"fmt"

	"github.com/google/uuid"
)

// The kinds of thing an id can name.
type (
	User           struct{} // A user account.
	Org            struct{} // An organization (tenant).
	Audit          struct{} // An audit event.
	Token          struct{} // An API token.
	Target         struct{} // One application on one environment placement (ADR-026).
	Release        struct{} // An immutable, portable release: artifact digests plus portable config.
	DeploymentRun  struct{} // One attempt to make a release effective on one target.
	BuildAttempt   struct{} // One build attempt; an infrastructure retry is a new attempt.
	SourceBinding  struct{} // A target's binding to one Git repository and branch (M3).
	Project        struct{} // A project: owns applications and environments.
	Environment    struct{} // A logical environment such as staging or production (ADR-026).
	Cluster        struct{} // A Kubernetes cluster registered with Kuben.
	Placement      struct{} // An environment's binding to one cluster and namespace (ADR-026).
	Application    struct{} // An application definition in a project.
	Operation      struct{} // A durable operation: one accepted request and its execution (plan §9.2).
	ConfigRevision struct{} // One immutable configuration revision of a target (ADR-026).
	RenderPlan     struct{} // A frozen, content-addressed render plan (ADR-026, I22).
)

// Kind is closed: only the kinds above name ids.
type Kind interface {
	User | Org | Audit | Token | Target | Release | DeploymentRun | BuildAttempt | SourceBinding |
		Project | Environment | Cluster | Placement | Application | Operation | ConfigRevision | RenderPlan
}

// The id types, by kind.
type (
	UserID           = ID[User]
	OrgID            = ID[Org]
	AuditID          = ID[Audit]
	TokenID          = ID[Token]
	TargetID         = ID[Target]
	ReleaseID        = ID[Release]
	DeploymentRunID  = ID[DeploymentRun]
	BuildAttemptID   = ID[BuildAttempt]
	SourceBindingID  = ID[SourceBinding]
	ProjectID        = ID[Project]
	EnvironmentID    = ID[Environment]
	ClusterID        = ID[Cluster]
	PlacementID      = ID[Placement]
	ApplicationID    = ID[Application]
	OperationID      = ID[Operation]
	ConfigRevisionID = ID[ConfigRevision]
	RenderPlanID     = ID[RenderPlan]
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
