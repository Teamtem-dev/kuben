// Package authz has the authorization primitives.
//
// Invariant I-1: no subscription (log, terminal, event stream) and no
// mutation runs without a [Proof]. Only a [Policy] in this package mints a
// valid one; the zero Proof is invalid and [Proof.Valid] says so.
package authz

import (
	"slices"

	"github.com/google/uuid"

	"github.com/Teamtem-dev/kuben/internal/core/ids"
	kerr "github.com/Teamtem-dev/kuben/internal/core/kerrors"
	"github.com/Teamtem-dev/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/internal/core/perm"
)

// Level is a level of the resource hierarchy.
type Level string

// The levels, outermost first.
const (
	LevelOrg         Level = "org"
	LevelProject     Level = "project"
	LevelEnvironment Level = "environment"
	LevelApp         Level = "app"
)

// ScopeRef is one node of the resource hierarchy. It is comparable.
type ScopeRef struct {
	Level Level
	ID    uuid.UUID
}

// OrgScope is the node of an organization.
func OrgScope(org ids.OrgID) ScopeRef { return ScopeRef{Level: LevelOrg, ID: org.UUID()} }

// ScopeChain is the ancestry of the resource being accessed: Org → Project →
// Environment → App. A role binding on any ancestor applies to the leaf.
type ScopeChain struct {
	Org         ids.OrgID
	Project     opt.Val[uuid.UUID]
	Environment opt.Val[uuid.UUID]
	App         opt.Val[uuid.UUID]
	// Aliases are other names of nodes on this chain: the Kubernetes UIDs of
	// resources whose SQL rows now name them (ADR-032), so role bindings made
	// on those UIDs keep applying. Empty in practice.
	Aliases []ScopeRef
}

// OrgChain is the chain of an organization itself.
func OrgChain(org ids.OrgID) ScopeChain { return ScopeChain{Org: org} }

// ProjectChain is the chain of a project.
func ProjectChain(org ids.OrgID, project uuid.UUID) ScopeChain {
	return ScopeChain{Org: org, Project: opt.Some(project)}
}

func (c ScopeChain) nodes() []ScopeRef {
	nodes := []ScopeRef{OrgScope(c.Org)}
	for _, n := range []struct {
		level Level
		id    opt.Val[uuid.UUID]
	}{{LevelProject, c.Project}, {LevelEnvironment, c.Environment}, {LevelApp, c.App}} {
		if id, ok := n.id.Get(); ok {
			nodes = append(nodes, ScopeRef{Level: n.level, ID: id})
		}
	}
	return nodes
}

// Contains reports whether scope is a node of the chain, by its name of
// record or by an alias of the same level.
func (c ScopeChain) Contains(scope ScopeRef) bool {
	return slices.Contains(c.nodes(), scope) || slices.Contains(c.Aliases, scope)
}

// Leaf is the most specific node of the chain.
func (c ScopeChain) Leaf() ScopeRef {
	nodes := c.nodes()
	return nodes[len(nodes)-1]
}

// Binding is a resolved role binding: Role on Scope.
type Binding struct {
	Scope ScopeRef
	Role  perm.Role
}

// Subject is the authenticated principal with its resolved bindings.
type Subject struct {
	User     ids.UserID
	Bindings []Binding
}

// Proof says that a user holds a permission on a scope.
type Proof struct {
	user   ids.UserID
	perm   perm.Perm
	scope  ScopeRef
	minted bool
}

// Valid is false for a Proof no Policy minted (the zero Proof).
func (p Proof) Valid() bool { return p.minted }

// User is who was authorized.
func (p Proof) User() ids.UserID { return p.user }

// Perm is what was authorized.
func (p Proof) Perm() perm.Perm { return p.perm }

// Scope is where it was authorized.
func (p Proof) Scope() ScopeRef { return p.scope }

// Policy decides whether a subject may perform an action.
type Policy interface {
	Allowed(subject Subject, p perm.Perm, chain ScopeChain) bool
}

// Check mints a Proof when policy allows the action, and is forbidden
// otherwise. It is the only way to a valid Proof.
func Check(policy Policy, subject Subject, p perm.Perm, chain ScopeChain) (Proof, error) {
	if !policy.Allowed(subject, p, chain) {
		return Proof{}, kerr.ErrForbidden
	}
	return Proof{user: subject.User, perm: p, scope: chain.Leaf(), minted: true}, nil
}

// RolePolicy is role based: a binding on any ancestor of the target applies.
type RolePolicy struct{}

// Allowed implements [Policy].
func (RolePolicy) Allowed(subject Subject, p perm.Perm, chain ScopeChain) bool {
	return slices.ContainsFunc(subject.Bindings, func(b Binding) bool {
		return chain.Contains(b.Scope) && b.Role.Grants(p)
	})
}

// EffectiveRole is the strongest role subject holds on any node of chain:
// what an environment policy compares with its deploy and approve roles.
func EffectiveRole(subject Subject, chain ScopeChain) opt.Val[perm.Role] {
	best := opt.None[perm.Role]()
	for _, b := range subject.Bindings {
		if !chain.Contains(b.Scope) {
			continue
		}
		if cur, ok := best.Get(); !ok || b.Role.Rank() > cur.Rank() {
			best = opt.Some(b.Role)
		}
	}
	return best
}
