// Package policy has environment policies and deployment approvals (M4.1;
// plan §8.2, §13). It replaces the Rust module kuben-core/src/policy.rs, and
// holds EnvironmentPolicy::for_preview of kuben-core/src/preview.rs.
//
// An environment's policy is an immutable revision: who may start
// deployments there, how many distinct people must approve a change before
// it is delivered, who may approve, and how long a request waits. Protection
// is policy, never inferred from a name. The rules here are pure; the store
// applies them inside the transaction that locks the run.
//
//	planned ──(approvals required)──► awaitingApproval ──(enough approvals)──► pendingDelivery
//	                                       │ reject / expiry
//	                                       ▼
//	                                   cancelled
package policy

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Teamtem-dev/kuben/go/hub/internal/core/kerr"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/perm"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/scan"
)

const (
	// MaxApprovals is the most approvals a policy may require.
	MaxApprovals uint8 = 5
	// MinApprovalTTLSecs is the shortest time a change may wait for its
	// approvals.
	MinApprovalTTLSecs uint32 = 5 * 60
	// MaxApprovalTTLSecs is the longest time a change may wait for its
	// approvals.
	MaxApprovalTTLSecs uint32 = 30 * 24 * 3600
	// DefaultApprovalTTLSecs is the default wait for approvals: one week.
	DefaultApprovalTTLSecs uint32 = 7 * 24 * 3600
	// MaxCommentChars is the longest approval comment kept.
	MaxCommentChars = 1024

	// maxHexChars is the longest plan hash read from hex.
	maxHexChars = 128
)

// ErrorKind is the rule a refused policy broke.
type ErrorKind string

// The rules a policy can break.
const (
	KindTooManyApprovals ErrorKind = "tooManyApprovals"
	KindDeployRole       ErrorKind = "deployRole"
	KindApproveRole      ErrorKind = "approveRole"
	KindApprovalTTL      ErrorKind = "approvalTtl"
	KindScanGate         ErrorKind = "scanGate"
)

// Error is why a policy was refused. It is comparable, so callers match it
// with == or errors.As; it is also a validation error to errors.Is.
type Error struct {
	Kind ErrorKind
	// Role is the refused role of a KindDeployRole or KindApproveRole error.
	Role perm.Role
}

func (e Error) Error() string {
	switch e.Kind {
	case KindTooManyApprovals:
		return fmt.Sprintf("at most %d approvals can be required", MaxApprovals)
	case KindDeployRole:
		return fmt.Sprintf("the deploy role `%s` cannot deploy", e.Role)
	case KindApproveRole:
		return fmt.Sprintf("the approve role `%s` cannot approve releases", e.Role)
	case KindApprovalTTL:
		return fmt.Sprintf("approvals must wait between %d and %d seconds", MinApprovalTTLSecs, MaxApprovalTTLSecs)
	case KindScanGate:
		return "the scan gate counts `high` or `critical` findings, with a maximum age of an hour to 90 days"
	}
	return "invalid policy: " + string(e.Kind)
}

// Is makes every policy error match kerr.ErrValidation: the API answers a
// refused policy as a validation failure.
func (e Error) Is(target error) bool { return target == error(kerr.ErrValidation) }

// EnvironmentPolicy is one revision of an environment's policy. The zero
// value is not a valid policy; [Open] is the default one.
type EnvironmentPolicy struct {
	// RequiredApprovals are the distinct approvers a change needs before
	// delivery; 0 for none.
	RequiredApprovals uint8 `json:"requiredApprovals"`
	// DeployRole is the weakest role that may start deployments here.
	DeployRole perm.Role `json:"deployRole"`
	// ApproveRole is the weakest role that may approve deployments here.
	ApproveRole perm.Role `json:"approveRole"`
	// ApprovalTTLSecs is how long a change waits for its approvals before it
	// is cancelled.
	ApprovalTTLSecs uint32 `json:"approvalTtlSecs"`
	// Scan is what vulnerability findings a deployment may carry (M4.6).
	Scan scan.Gate `json:"scan"`
}

// Open is the default policy: no approvals; developers deploy.
func Open() EnvironmentPolicy {
	return EnvironmentPolicy{
		RequiredApprovals: 0,
		DeployRole:        perm.Developer,
		ApproveRole:       perm.Admin,
		ApprovalTTLSecs:   DefaultApprovalTTLSecs,
		Scan:              scan.Off(),
	}
}

// Production is one approval by an admin who did not ask for the change;
// known critical findings are refused.
func Production() EnvironmentPolicy {
	p := Open()
	p.RequiredApprovals = 1
	p.Scan = scan.Production()
	return p
}

// Initial is the first policy of a new environment.
func Initial(production bool) EnvironmentPolicy {
	if production {
		return Production()
	}
	return Open()
}

// ForPreview is a preview's policy, inherited from its source environment's:
// the same roles and vulnerability gate, but no approvals, so every push of
// the pull request deploys.
func ForPreview(source EnvironmentPolicy) EnvironmentPolicy {
	source.RequiredApprovals = 0
	return source
}

// UnmarshalJSON requires every field but `scan`, which defaults to the off
// gate (policies stored before M4.6 have none), and accepts only known roles.
func (p *EnvironmentPolicy) UnmarshalJSON(data []byte) error {
	var w struct {
		RequiredApprovals opt.Val[uint8]     `json:"requiredApprovals"`
		DeployRole        opt.Val[string]    `json:"deployRole"`
		ApproveRole       opt.Val[string]    `json:"approveRole"`
		ApprovalTTLSecs   opt.Val[uint32]    `json:"approvalTtlSecs"`
		Scan              opt.Val[scan.Gate] `json:"scan"`
	}
	if err := json.Unmarshal(data, &w); err != nil {
		return err
	}
	approvals, hasApprovals := w.RequiredApprovals.Get()
	deploy, hasDeploy := w.DeployRole.Get()
	approve, hasApprove := w.ApproveRole.Get()
	ttl, hasTTL := w.ApprovalTTLSecs.Get()
	for _, field := range []struct {
		name    string
		present bool
	}{
		{"requiredApprovals", hasApprovals},
		{"deployRole", hasDeploy},
		{"approveRole", hasApprove},
		{"approvalTtlSecs", hasTTL},
	} {
		if !field.present {
			return kerr.New(kerr.Validation, "missing field `%s`", field.name)
		}
	}
	deployRole, err := perm.ParseRole(deploy)
	if err != nil {
		return fmt.Errorf("deployRole: %w", err)
	}
	approveRole, err := perm.ParseRole(approve)
	if err != nil {
		return fmt.Errorf("approveRole: %w", err)
	}
	*p = EnvironmentPolicy{
		RequiredApprovals: approvals,
		DeployRole:        deployRole,
		ApproveRole:       approveRole,
		ApprovalTTLSecs:   ttl,
		Scan:              w.Scan.Or(scan.Off()),
	}
	return nil
}

// Validate refuses a policy that may not be stored, with an [Error].
func (p EnvironmentPolicy) Validate() error {
	switch {
	case p.RequiredApprovals > MaxApprovals:
		return Error{Kind: KindTooManyApprovals}
	case !p.DeployRole.Grants(perm.AppDeploy):
		return Error{Kind: KindDeployRole, Role: p.DeployRole}
	case !p.ApproveRole.Grants(perm.ReleaseApprove):
		return Error{Kind: KindApproveRole, Role: p.ApproveRole}
	case p.ApprovalTTLSecs < MinApprovalTTLSecs || p.ApprovalTTLSecs > MaxApprovalTTLSecs:
		return Error{Kind: KindApprovalTTL}
	case !p.Scan.Valid():
		return Error{Kind: KindScanGate}
	}
	return nil
}

// ChangeKind is what a deployment run changes, as far as approval is
// concerned. It has no wire form: the store maps its run kinds to it.
type ChangeKind string

// The kinds of change.
const (
	Deploy    ChangeKind = "deploy"
	Rollback  ChangeKind = "rollback"
	Promotion ChangeKind = "promotion"
	// Build is an automatic deploy of a verified build.
	Build ChangeKind = "build"
	// Restart is the same release and configuration, pods replaced.
	Restart ChangeKind = "restart"
	// Handover is the same release and configuration, delivered by the agent.
	Handover ChangeKind = "handover"
	// Rotation is the same release and configuration with a secret's new
	// revision: new values reach production like any other change.
	Rotation ChangeKind = "rotation"
	// Emergency is a person's break-glass rollback (M4.9): it cannot wait
	// for approvals.
	Emergency ChangeKind = "emergency"
)

// ParseChangeKind reads a kind of change.
func ParseChangeKind(s string) (ChangeKind, error) {
	switch k := ChangeKind(s); k {
	case Deploy, Rollback, Promotion, Build, Restart, Handover, Rotation, Emergency:
		return k, nil
	}
	return "", kerr.New(kerr.Validation, "unknown kind of change `%s`", s)
}

// ApprovalsFor is how many approvals a change of kind needs. Restarts and
// handovers change neither the release nor the configuration; anything that
// is not a known kind needs the approvals of a deployment.
func (p EnvironmentPolicy) ApprovalsFor(kind ChangeKind) uint8 {
	switch kind {
	case Restart, Handover, Emergency:
		return 0
	case Deploy, Rollback, Promotion, Build, Rotation:
		return p.RequiredApprovals
	}
	return p.RequiredApprovals
}

// MayDeploy reports whether a caller whose strongest role on the environment
// is role may start a deployment there.
func (p EnvironmentPolicy) MayDeploy(role opt.Val[perm.Role]) bool {
	r, ok := role.Get()
	return ok && r.Rank() >= p.DeployRole.Rank() && r.Grants(perm.AppDeploy)
}

// MayApprove reports whether a caller whose strongest role on the
// environment is role may approve there.
func (p EnvironmentPolicy) MayApprove(role opt.Val[perm.Role]) bool {
	r, ok := role.Get()
	return ok && r.Rank() >= p.ApproveRole.Rank() && r.Grants(perm.ReleaseApprove)
}

// Weakens reports whether p protects less than current in any respect.
// Weakening production protection is an owner's decision.
func (p EnvironmentPolicy) Weakens(current EnvironmentPolicy) bool {
	return p.RequiredApprovals < current.RequiredApprovals ||
		p.DeployRole.Rank() < current.DeployRole.Rank() ||
		p.ApproveRole.Rank() < current.ApproveRole.Rank() ||
		p.ApprovalTTLSecs > current.ApprovalTTLSecs ||
		p.Scan.Weakens(current.Scan)
}

// Decision is an approver's answer. Its wire names are the request's
// (`approve`); [Decision.Stored] is the name of the recorded decision.
type Decision string

// The decisions.
const (
	Approve Decision = "approve"
	Reject  Decision = "reject"
)

// ParseDecision reads a decision.
func ParseDecision(s string) (Decision, error) {
	switch d := Decision(s); d {
	case Approve, Reject:
		return d, nil
	}
	return "", kerr.New(kerr.Validation, "unknown decision `%s`", s)
}

// Stored is the stored name of the decision (`approved`, `rejected`); empty
// for a value that is not a decision.
func (d Decision) Stored() string {
	switch d {
	case Approve:
		return "approved"
	case Reject:
		return "rejected"
	}
	return ""
}

// UnmarshalJSON accepts only the exact wire names.
func (d *Decision) UnmarshalJSON(data []byte) error {
	var text string
	if err := json.Unmarshal(data, &text); err != nil {
		return err
	}
	v, err := ParseDecision(text)
	if err != nil {
		return err
	}
	*d = v
	return nil
}

// ApprovalError is why a decision was refused. Nothing is recorded for a
// refusal. Callers match the constants with == or errors.Is.
type ApprovalError string

// The refusals; the value is the message.
const (
	ErrNotAwaiting    ApprovalError = "the deployment is not waiting for approval"
	ErrExpired        ApprovalError = "the approval window of the deployment has closed"
	ErrSelfApproval   ApprovalError = "whoever asked for a deployment cannot approve it"
	ErrAlreadyDecided ApprovalError = "this approver has already decided on the deployment"
	ErrStalePlan      ApprovalError = "the deployment changed since it was shown: reload and decide again"
)

func (e ApprovalError) Error() string { return string(e) }

// Pending is what a run waiting for approval looks like to a decision.
type Pending struct {
	// RequestedBy is the `kind:principal` of whoever asked for the run.
	RequestedBy string
	Required    uint8
	// Approved are the distinct approvals recorded so far.
	Approved  uint8
	ExpiresAt int64
	PlanHash  []byte
}

// Tally is the outcome of a valid decision.
//
//sumtype:decl
type Tally interface{ tally() }

// Waiting: more approvals are needed.
type Waiting struct{ Remaining uint8 }

// Approved: enough approvals, the run may be delivered.
type Approved struct{}

// Rejected: one rejection cancels the run.
type Rejected struct{}

func (Waiting) tally()  {}
func (Approved) tally() {}
func (Rejected) tally() {}

// Principal is the principal part of `kind:principal`.
func Principal(actor string) string {
	if _, id, found := strings.Cut(actor, ":"); found {
		return id
	}
	return actor
}

// Decide says whether approver (a user id) may record decision on run at
// now, having seen planHash, and what the run becomes. A refusal is an
// [ApprovalError].
func Decide(run Pending, approver string, decidedBefore bool, decision Decision, planHash []byte, now int64) (Tally, error) {
	if now >= run.ExpiresAt {
		return nil, ErrExpired
	}
	// A requester may withdraw nothing and approve nothing: only others decide.
	if Principal(run.RequestedBy) == approver {
		return nil, ErrSelfApproval
	}
	if decidedBefore {
		return nil, ErrAlreadyDecided
	}
	if !bytes.Equal(planHash, run.PlanHash) {
		return nil, ErrStalePlan
	}
	switch decision {
	case Reject:
		return Rejected{}, nil
	case Approve:
		count := run.Approved
		if count < 255 {
			count++
		}
		if count >= run.Required {
			return Approved{}, nil
		}
		return Waiting{Remaining: run.Required - count}, nil
	}
	return nil, kerr.New(kerr.Validation, "unknown decision `%s`", string(decision))
}

// Hex is a plan hash as the API shows it.
func Hex(hash []byte) string { return hex.EncodeToString(hash) }

// Unhex is a plan hash given as hex, in either case; false when it is not
// hex or longer than a plan hash can be.
func Unhex(text string) ([]byte, bool) {
	if len(text)%2 != 0 || len(text) > maxHexChars {
		return nil, false
	}
	hash, err := hex.DecodeString(text)
	if err != nil {
		return nil, false
	}
	return hash, true
}
