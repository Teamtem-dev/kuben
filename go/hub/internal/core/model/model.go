// Package model has the persistent identity and audit models (owned by SQL,
// see ADR-015). It replaces the Rust module kuben-core/src/model.rs.
package model

import (
	"github.com/google/uuid"

	"github.com/Teamtem-dev/kuben/go/hub/internal/core/ids"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/kerr"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/perm"
)

// Organization is a tenant.
type Organization struct {
	ID        ids.OrgID `json:"id"`
	Slug      string    `json:"slug"`
	Name      string    `json:"name"`
	CreatedAt int64     `json:"created_at"`
}

// User is a user account.
type User struct {
	ID          ids.UserID      `json:"id"`
	Email       string          `json:"email"`
	DisplayName opt.Val[string] `json:"display_name"`
	IsActive    bool            `json:"is_active"`
	// MustChangePassword is set for invited users until they replace their
	// temporary password.
	MustChangePassword bool  `json:"must_change_password"`
	CreatedAt          int64 `json:"created_at"`
}

// UserCredentials is a user together with its password hash (PHC string).
// It is never serialized: the hash is left out of any JSON.
type UserCredentials struct {
	User         User            `json:"-"`
	PasswordHash opt.Val[string] `json:"-"`
}

// Session is a login session of a user.
type Session struct {
	UserID     ids.UserID
	CreatedAt  int64
	ExpiresAt  int64
	LastSeenAt opt.Val[int64]
	RevokedAt  opt.Val[int64]
}

// IsValidAt reports whether the session is neither revoked nor expired at
// nowMs.
func (s Session) IsValidAt(nowMs int64) bool {
	return s.RevokedAt.IsNone() && s.ExpiresAt > nowMs
}

// RoleBinding grants a role to a subject on one scope of an organization.
type RoleBinding struct {
	OrgID       ids.OrgID
	SubjectKind SubjectKind
	SubjectID   string
	Role        perm.Role
	ScopeKind   ScopeKind
	ScopeUID    opt.Val[string]
}

// SubjectKind is what a role binding's subject is.
type SubjectKind string

// The subject kinds.
const (
	SubjectUser  SubjectKind = "user"
	SubjectTeam  SubjectKind = "team"
	SubjectToken SubjectKind = "token"
)

// ParseSubjectKind reads a subject kind as stored.
func ParseSubjectKind(s string) (SubjectKind, error) {
	switch k := SubjectKind(s); k {
	case SubjectUser, SubjectTeam, SubjectToken:
		return k, nil
	}
	return "", kerr.New(kerr.Validation, "unknown subject kind `%s`", s)
}

func (k SubjectKind) String() string { return string(k) }

// UnmarshalText refuses a kind that does not exist.
func (k *SubjectKind) UnmarshalText(text []byte) error {
	parsed, err := ParseSubjectKind(string(text))
	if err != nil {
		return err
	}
	*k = parsed
	return nil
}

// ScopeKind is the level of the resource hierarchy a role binding is on.
type ScopeKind string

// The scope kinds, outermost first.
const (
	ScopeOrg         ScopeKind = "org"
	ScopeProject     ScopeKind = "project"
	ScopeEnvironment ScopeKind = "environment"
	ScopeApp         ScopeKind = "app"
)

// ParseScopeKind reads a scope kind as stored.
func ParseScopeKind(s string) (ScopeKind, error) {
	switch k := ScopeKind(s); k {
	case ScopeOrg, ScopeProject, ScopeEnvironment, ScopeApp:
		return k, nil
	}
	return "", kerr.New(kerr.Validation, "unknown scope kind `%s`", s)
}

func (k ScopeKind) String() string { return string(k) }

// UnmarshalText refuses a kind that does not exist.
func (k *ScopeKind) UnmarshalText(text []byte) error {
	parsed, err := ParseScopeKind(string(text))
	if err != nil {
		return err
	}
	*k = parsed
	return nil
}

// AuditEvent is an append-only audit record.
type AuditEvent struct {
	// Seq is the monotonic insertion order (pagination cursor).
	Seq        int64              `json:"seq"`
	ID         ids.AuditID        `json:"id"`
	OrgID      opt.Val[ids.OrgID] `json:"org_id"`
	ActorKind  string             `json:"actor_kind"`
	ActorID    opt.Val[string]    `json:"actor_id"`
	Action     string             `json:"action"`
	TargetKind opt.Val[string]    `json:"target_kind"`
	TargetRef  opt.Val[string]    `json:"target_ref"`
	Outcome    string             `json:"outcome"`
	IP         opt.Val[string]    `json:"ip"`
	RequestID  opt.Val[string]    `json:"request_id"`
	Data       opt.Val[any]       `json:"data"`
	CreatedAt  int64              `json:"created_at"`
}

// Member is a member of an organization with its org-level role.
type Member struct {
	User User
	Role perm.Role
}

// TokenScope is what an API token may do, stored as JSON in
// `api_tokens.scopes`. The effective role is the weaker of Role and the
// owner's own role; the optional project/environment narrows the token to
// that subtree.
type TokenScope struct {
	Role        perm.Role          `json:"role"`
	Project     opt.Val[uuid.UUID] `json:"project,omitzero"`
	Environment opt.Val[uuid.UUID] `json:"environment,omitzero"`
}

// APIToken is a personal API token. Only sha256(secret) is ever stored.
type APIToken struct {
	ID    ids.TokenID
	OrgID ids.OrgID
	Owner opt.Val[ids.UserID]
	Name  string
	// Prefix is the non-secret display prefix, e.g. `kbn_pat_0192f3a1`.
	Prefix     string
	SecretHash []byte `json:"-"`
	Scope      TokenScope
	ExpiresAt  opt.Val[int64]
	LastUsedAt opt.Val[int64]
	RevokedAt  opt.Val[int64]
	CreatedAt  int64
}

// IsUsableAt reports whether the token is neither revoked nor expired at
// nowMs. A token without an expiry never expires.
func (t APIToken) IsUsableAt(nowMs int64) bool {
	if t.RevokedAt.IsSome() {
		return false
	}
	at, expires := t.ExpiresAt.Get()
	return !expires || at > nowMs
}

// AppRelease is one deployed revision of an App: a snapshot of its spec
// (which only ever holds Secret references, never values).
type AppRelease struct {
	ID        string          `json:"id"`
	Revision  int64           `json:"revision"`
	Namespace string          `json:"namespace"`
	App       string          `json:"app"`
	Image     opt.Val[string] `json:"image"`
	Spec      any             `json:"spec"`
	// Reason is `create`, `deploy`, `config`, `rollback`, `promote` or
	// `template`.
	Reason    string          `json:"reason"`
	ActorID   opt.Val[string] `json:"actor_id"`
	Note      opt.Val[string] `json:"note"`
	CreatedAt int64           `json:"created_at"`
}
