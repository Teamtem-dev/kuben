// Package target has the application target's control state: the monotonic
// desired generation, the source epoch and the deploy policy (ADR-026). It
// replaces the Rust module kuben-core/src/ops/target.rs.
//
// Only [State]'s methods may raise the generation. A late build (older source
// epoch), a stale request (older expected generation), a pinned target or a
// recreated target (different lifecycle UID) cannot move it:
//
//	A accepted: epoch 41 → build A
//	B observed: epoch 42 → build B
//	B finishes → CAS epoch=42 → generation 90
//	A finishes → CAS epoch=41 fails → artifact kept, run Superseded, no deploy
//
// In the product these checks run inside one SQL transaction with the target
// row locked, so the in-memory model here is the specification the store
// implements.
package target

import (
	"fmt"
	"math"

	"github.com/google/uuid"

	"github.com/Teamtem-dev/kuben/go/hub/internal/core/kerr"
)

// Generation is the monotonic desired-state counter of one target.
type Generation uint64

// SourceEpoch counts the source heads observed for a source binding. A
// force-push is a new head too; commit timestamps never decide order.
type SourceEpoch uint64

// DeployPolicy says who moves a target to a new release. The zero
// DeployPolicy is not a policy: [New] starts a target on [Auto], and a target
// without a policy deploys nothing automatically.
type DeployPolicy string

// The policies.
const (
	// Auto: successful builds of the current source head deploy themselves.
	Auto DeployPolicy = "auto"
	// Manual: builds produce releases; a human or API call deploys them.
	Manual DeployPolicy = "manual"
	// Pinned: held on a chosen release (after a rollback) until resumed
	// explicitly.
	Pinned DeployPolicy = "pinned"
)

// ParseDeployPolicy reads a policy as stored.
func ParseDeployPolicy(s string) (DeployPolicy, error) {
	switch p := DeployPolicy(s); p {
	case Auto, Manual, Pinned:
		return p, nil
	}
	return "", kerr.New(kerr.Validation, "unknown deploy policy `%s`", s)
}

func (p DeployPolicy) String() string { return string(p) }

// UnmarshalText refuses a policy that does not exist.
func (p *DeployPolicy) UnmarshalText(text []byte) error {
	parsed, err := ParseDeployPolicy(string(text))
	if err != nil {
		return err
	}
	*p = parsed
	return nil
}

// variantName is the policy as Rust's Debug printed it, which is what
// [NotAutomatic]'s message shows.
func (p DeployPolicy) variantName() string {
	switch p {
	case Auto:
		return "Auto"
	case Manual:
		return "Manual"
	case Pinned:
		return "Pinned"
	}
	return string(p)
}

// Reject is why a change to the target was refused. Every variant is
// comparable, so callers may switch on the type or compare values.
//
//sumtype:decl
type Reject interface {
	error
	reject()
}

type (
	// LifecycleMismatch means the target was deleted and recreated.
	LifecycleMismatch struct{}
	// Deleting means the target is being deleted.
	Deleting struct{}
	// StaleSource means a newer source head exists.
	StaleSource struct{ Current, Requested uint64 }
	// BuildConfigChanged means the build configuration changed since the build
	// started.
	BuildConfigChanged struct{}
	// GenerationMoved means the target moved on since the caller read it.
	GenerationMoved struct{ Current, Expected uint64 }
	// NotAutomatic means automatic deploys are off for this target.
	NotAutomatic struct{ Policy DeployPolicy }
	// Exhausted means the generation counter is exhausted.
	Exhausted struct{}
)

func (LifecycleMismatch) reject()  {}
func (Deleting) reject()           {}
func (StaleSource) reject()        {}
func (BuildConfigChanged) reject() {}
func (GenerationMoved) reject()    {}
func (NotAutomatic) reject()       {}
func (Exhausted) reject()          {}

func (LifecycleMismatch) Error() string {
	return "the target was deleted and recreated (lifecycle mismatch)"
}

func (Deleting) Error() string { return "the target is being deleted" }

func (r StaleSource) Error() string {
	return fmt.Sprintf("a newer source head exists (epoch %d, request %d)", r.Current, r.Requested)
}

func (BuildConfigChanged) Error() string {
	return "the build configuration changed since the build started"
}

func (r GenerationMoved) Error() string {
	return fmt.Sprintf("the target moved on (generation %d, expected %d)", r.Current, r.Expected)
}

func (r NotAutomatic) Error() string {
	return fmt.Sprintf("automatic deploys are off for this target (%s)", r.Policy.variantName())
}

func (Exhausted) Error() string { return "the generation counter is exhausted" }

// AutodeployRequest is an automatic deploy proposed by a finished build.
type AutodeployRequest struct {
	LifecycleUID        uuid.UUID
	SourceEpoch         SourceEpoch
	BuildConfigRevision uint64
	ExpectedGeneration  Generation
}

// State is the control state of one target. The fields are exported for
// the store and the wire; only the methods change them.
type State struct {
	LifecycleUID        uuid.UUID    `json:"lifecycleUid"`
	Deleting            bool         `json:"deleting"`
	DesiredGeneration   Generation   `json:"desiredGeneration"`
	SourceEpoch         SourceEpoch  `json:"sourceEpoch"`
	BuildConfigRevision uint64       `json:"buildConfigRevision"`
	Policy              DeployPolicy `json:"policy"`
}

// New is the state of a fresh target: generation 0, epoch 0, automatic.
func New(lifecycleUID uuid.UUID) State {
	return State{LifecycleUID: lifecycleUID, Policy: Auto}
}

// ObserveSourceHead records that a new head was read from the provider (not
// merely a webhook arriving) and returns its epoch.
func (s *State) ObserveSourceHead() SourceEpoch {
	if s.SourceEpoch < math.MaxUint64 {
		s.SourceEpoch++
	}
	return s.SourceEpoch
}

// ChangeBuildConfig records that the build configuration changed; builds of
// the old configuration may no longer deploy automatically.
func (s *State) ChangeBuildConfig() {
	if s.BuildConfigRevision < math.MaxUint64 {
		s.BuildConfigRevision++
	}
}

// TryAutodeploy is the compare-and-set for a build's automatic deploy.
func (s *State) TryAutodeploy(req AutodeployRequest) (Generation, Reject) {
	if rej := s.guard(req.LifecycleUID, req.ExpectedGeneration); rej != nil {
		return 0, rej
	}
	if s.Policy != Auto {
		return 0, NotAutomatic{Policy: s.Policy}
	}
	if req.SourceEpoch != s.SourceEpoch {
		return 0, StaleSource{Current: uint64(s.SourceEpoch), Requested: uint64(req.SourceEpoch)}
	}
	if req.BuildConfigRevision != s.BuildConfigRevision {
		return 0, BuildConfigChanged{}
	}
	return s.bump()
}

// DeployExplicit is an explicit deploy or promotion chosen by a person or an
// API client. It works under any policy; it does not unpin.
func (s *State) DeployExplicit(lifecycleUID uuid.UUID, expected Generation) (Generation, Reject) {
	if rej := s.guard(lifecycleUID, expected); rej != nil {
		return 0, rej
	}
	return s.bump()
}

// Rollback is a new generation that pins the target, so late webhooks and
// builds cannot move it off the chosen release.
func (s *State) Rollback(lifecycleUID uuid.UUID, expected Generation) (Generation, Reject) {
	if rej := s.guard(lifecycleUID, expected); rej != nil {
		return 0, rej
	}
	generation, rej := s.bump()
	if rej != nil {
		return 0, rej
	}
	s.Policy = Pinned
	return generation, nil
}

// ResumeAuto explicitly resumes automatic deploys after a pin.
func (s *State) ResumeAuto(lifecycleUID uuid.UUID) Reject {
	if lifecycleUID != s.LifecycleUID {
		return LifecycleMismatch{}
	}
	s.Policy = Auto
	return nil
}

// BeginDeletion marks the target as being deleted; it accepts nothing after.
func (s *State) BeginDeletion() { s.Deleting = true }

func (s *State) guard(lifecycleUID uuid.UUID, expected Generation) Reject {
	if lifecycleUID != s.LifecycleUID {
		return LifecycleMismatch{}
	}
	if s.Deleting {
		return Deleting{}
	}
	if expected != s.DesiredGeneration {
		return GenerationMoved{Current: uint64(s.DesiredGeneration), Expected: uint64(expected)}
	}
	return nil
}

func (s *State) bump() (Generation, Reject) {
	if s.DesiredGeneration == math.MaxUint64 {
		return 0, Exhausted{}
	}
	s.DesiredGeneration++
	return s.DesiredGeneration, nil
}
