// Package perm has the permissions and the built-in roles.
package perm

import (
	"slices"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/kerrors"
)

// Perm is a fine-grained permission. Every mutating or streaming API
// requires one.
type Perm string

// The permissions.
const (
	OrgRead            Perm = "org-read"
	OrgAdmin           Perm = "org-admin"
	ProjectRead        Perm = "project-read"
	ProjectWrite       Perm = "project-write"
	EnvRead            Perm = "env-read"
	EnvWrite           Perm = "env-write"
	EnvDeleteProtected Perm = "env-delete-protected"
	EnvProtect         Perm = "env-protect" // Weaken an environment's protection policy (M4.1).
	AppRead            Perm = "app-read"
	AppWrite           Perm = "app-write"
	AppDeploy          Perm = "app-deploy"
	AppLogsRead        Perm = "app-logs-read"
	AppExec            Perm = "app-exec"
	SecretRead         Perm = "secret-read"
	SecretWrite        Perm = "secret-write"
	ReleasePromote     Perm = "release-promote"
	ReleaseApprove     Perm = "release-approve"
	AuditRead          Perm = "audit-read"
	UserAdmin          Perm = "user-admin"
)

// Role is a built-in role. Custom roles map to a set of [Perm]s (future work).
type Role string

// The roles, in the strict hierarchy viewer < developer < admin < owner:
// every role's permissions are a superset of the role below it.
const (
	Viewer    Role = "viewer"
	Developer Role = "developer"
	Admin     Role = "admin"
	Owner     Role = "owner"
)

var viewerPerms = []Perm{OrgRead, ProjectRead, EnvRead, AppRead, AppLogsRead}

var developerPerms = append(slices.Clone(viewerPerms), AppWrite, AppDeploy, AppExec, SecretRead)

var adminPerms = append(slices.Clone(developerPerms),
	ProjectWrite, EnvWrite, SecretWrite, ReleasePromote, ReleaseApprove, AuditRead, UserAdmin)

var ownerPerms = append(slices.Clone(adminPerms), OrgAdmin, EnvDeleteProtected, EnvProtect)

// ParseRole reads a role name.
func ParseRole(s string) (Role, error) {
	switch r := Role(s); r {
	case Viewer, Developer, Admin, Owner:
		return r, nil
	}
	return "", kerrors.New(kerrors.Validation, "unknown role `%s`", s)
}

// Perms are the permissions the role grants; nil for an unknown role.
func (r Role) Perms() []Perm {
	switch r {
	case Viewer:
		return viewerPerms
	case Developer:
		return developerPerms
	case Admin:
		return adminPerms
	case Owner:
		return ownerPerms
	}
	return nil
}

// Rank is the role's position in the hierarchy; -1 for an unknown role, so
// that it is weaker than every role and grants nothing.
func (r Role) Rank() int {
	switch r {
	case Viewer:
		return 0
	case Developer:
		return 1
	case Admin:
		return 2
	case Owner:
		return 3
	}
	return -1
}

// Weaker is the weaker of two roles: the effective role of an API token is
// the weaker of its own cap and its owner's role.
func (r Role) Weaker(other Role) Role {
	if r.Rank() <= other.Rank() {
		return r
	}
	return other
}

// Grants reports whether the role grants p.
func (r Role) Grants(p Perm) bool { return slices.Contains(r.Perms(), p) }

func (r Role) String() string { return string(r) }

// UnmarshalText accepts only the four role names, as serde did: JSON, TOML
// and configuration decoding all go through it.
func (r *Role) UnmarshalText(text []byte) error {
	parsed, err := ParseRole(string(text))
	if err != nil {
		return err
	}
	*r = parsed
	return nil
}
