package api

// Vulnerability exceptions of an organization (M4.6): one finding may pass
// the scan gates until the exception expires or is revoked. Granting one
// weakens every gate it touches, so only people with organization admin
// rights do it, name an owner and a reason, and never for longer than 90
// days. Tokens cannot grant or revoke (routes/vulnerabilities.rs).

import (
	"context"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/Teamtem-dev/kuben/internal/core/authz"
	"github.com/Teamtem-dev/kuben/internal/core/clock"
	"github.com/Teamtem-dev/kuben/internal/core/ids"
	"github.com/Teamtem-dev/kuben/internal/core/kerrors"
	"github.com/Teamtem-dev/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/internal/core/perm"
	"github.com/Teamtem-dev/kuben/internal/core/scan"
	"github.com/Teamtem-dev/kuben/internal/httpapi/access"
	"github.com/Teamtem-dev/kuben/internal/httpapi/gen"
	"github.com/Teamtem-dev/kuben/internal/store"
)

// exceptionDto is ExceptionDto::of at now.
func exceptionDto(e store.VulnException, now int64) gen.ExceptionDto {
	project := nullUUID()
	if p, ok := e.Project.Get(); ok {
		project = gen.NewOptNilUUID(p.UUID())
	}
	revoked := opt.None[string]()
	if at, ok := e.RevokedAt.Get(); ok {
		revoked = opt.Some(Timestamp(at))
	}
	return gen.ExceptionDto{
		Active:        e.RevokedAt.IsNone() && e.ExpiresAt > now,
		ID:            e.ID,
		Vulnerability: e.Vulnerability,
		Project:       project,
		Reason:        e.Reason,
		Owner:         e.Owner,
		CreatedBy:     e.CreatedBy,
		CreatedAt:     Timestamp(e.CreatedAt),
		ExpiresAt:     Timestamp(e.ExpiresAt),
		RevokedAt:     optNilString(revoked),
	}
}

// isASCIIAlnumByte is u8::is_ascii_alphanumeric.
func isASCIIAlnumByte(b byte) bool {
	return (b >= '0' && b <= '9') || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

// vulnerabilityID reports whether id (trimmed) names a finding: 1 to 64
// bytes of ASCII letters, digits, `.`, `_`, `:` and `-`, starting with a
// letter or digit.
func vulnerabilityID(id string) bool {
	if id == "" || len(id) > 64 || !isASCIIAlnumByte(id[0]) {
		return false
	}
	for i := range len(id) {
		b := id[i]
		if !isASCIIAlnumByte(b) && b != '.' && b != '_' && b != ':' && b != '-' {
			return false
		}
	}
	return true
}

// printable checks a trimmed text of 1 to max characters without control
// characters.
func printable(name, value string, maxChars int) error {
	value = trimSpace(value)
	if value == "" || utf8.RuneCountInString(value) > maxChars || strings.IndexFunc(value, unicode.IsControl) >= 0 {
		return kerrors.New(kerrors.Validation, "%s must be 1 to %d printable characters", name, maxChars)
	}
	return nil
}

// checkException is check: the request's limits.
func checkException(body *gen.CreateException) error {
	id := trimSpace(body.Vulnerability)
	if !vulnerabilityID(id) {
		return kerrors.New(kerrors.Validation, "`%s` is not a vulnerability id", id)
	}
	if err := printable("reason", body.Reason, 1024); err != nil {
		return err
	}
	if err := printable("owner", body.Owner, 256); err != nil {
		return err
	}
	const maxDays = scan.MaxExceptionSecs / 86_400
	if body.Days <= 0 || int64(body.Days) > maxDays {
		return kerrors.New(kerrors.Validation, "an exception lasts 1 to %d days", maxDays)
	}
	return nil
}

// accessOrg is a caller and the organization a route acts on for it.
type accessOrg struct {
	access access.Access
	org    ids.OrgID
}

// callerOrg is the organization an org-level route acts on (the caller's
// first, org_of), with the proof of p there.
func (s *Server) callerOrg(ctx context.Context, p perm.Perm, forbidToken bool) (accessOrg, error) {
	a, err := s.access(ctx)
	if err != nil {
		return accessOrg{}, err
	}
	if forbidToken {
		if err := a.ForbidToken(); err != nil {
			return accessOrg{}, err //nolint:wrapcheck // a kerrors already
		}
	}
	org, err := orgOf(a)
	if err != nil {
		return accessOrg{}, err
	}
	if _, err := a.Require(p, authz.OrgChain(org)); err != nil {
		return accessOrg{}, err //nolint:wrapcheck // a kerrors already
	}
	return accessOrg{access: a, org: org}, nil
}

// ListVulnerabilityExceptions is the organization's vulnerability
// exceptions in force (or all).
func (s *Server) ListVulnerabilityExceptions(
	ctx context.Context, params gen.ListVulnerabilityExceptionsParams,
) (gen.ListVulnerabilityExceptionsRes, error) {
	c, err := s.callerOrg(ctx, perm.OrgRead, false)
	if err != nil {
		return nil, err
	}
	t, err := s.deps.Store.Tenant(ctx, c.org)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	defer t.Rollback(ctx) //nolint:errcheck // read only
	now := s.deps.Clock.NowMs()
	found, err := t.Exceptions(ctx, params.All.Or(false))
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	out := make(gen.ListVulnerabilityExceptionsOKApplicationJSON, 0, len(found))
	for _, e := range found {
		out = append(out, exceptionDto(e, now))
	}
	return &out, nil
}

// CreateVulnerabilityException lets one finding pass the scan gates for a
// while.
func (s *Server) CreateVulnerabilityException(
	ctx context.Context, req *gen.CreateException,
) (gen.CreateVulnerabilityExceptionRes, error) {
	c, err := s.callerOrg(ctx, perm.OrgAdmin, true)
	if err != nil {
		return nil, err
	}
	if err := checkException(req); err != nil {
		return nil, err
	}
	project := opt.None[ids.ProjectID]()
	if slug, ok := req.Project.Get(); ok {
		p, err := s.findProject(ctx, c.access, slug)
		if err != nil {
			return nil, err
		}
		project = opt.Some(p.project.ID)
	}
	_, actor := c.access.Actor()
	now := s.deps.Clock.NowMs()
	n := store.NewException{
		Project:       project,
		Vulnerability: trimSpace(req.Vulnerability),
		Reason:        trimSpace(req.Reason),
		Owner:         trimSpace(req.Owner),
		CreatedBy:     actor,
		ExpiresAt:     clock.SaturatingAdd(now, int64(req.Days)*86_400_000),
	}
	t, err := s.deps.Store.Tenant(ctx, c.org)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	defer t.Rollback(ctx) //nolint:errcheck // a no-op after the commit
	id, err := t.CreateException(ctx, n)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	audit := requestAudit(c.access, "vulnerability.exception.created", "vulnerability", n.Vulnerability)
	audit.Data = opt.Some[any](map[string]any{"id": id.String(), "owner": n.Owner, "expires_at": n.ExpiresAt})
	if err := t.AppendAudit(ctx, audit); err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	all, err := t.Exceptions(ctx, true)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	for _, e := range all {
		if e.ID != id {
			continue
		}
		if err := t.Commit(ctx); err != nil {
			return nil, err //nolint:wrapcheck // a store error, answered as internal
		}
		dto := exceptionDto(e, now)
		return &dto, nil
	}
	return nil, kerrors.New(kerrors.Internal, "the new exception is missing")
}

// RevokeVulnerabilityException revokes an exception for good.
func (s *Server) RevokeVulnerabilityException(
	ctx context.Context, params gen.RevokeVulnerabilityExceptionParams,
) (gen.RevokeVulnerabilityExceptionRes, error) {
	c, err := s.callerOrg(ctx, perm.OrgAdmin, true)
	if err != nil {
		return nil, err
	}
	_, actor := c.access.Actor()
	t, err := s.deps.Store.Tenant(ctx, c.org)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	defer t.Rollback(ctx) //nolint:errcheck // a no-op after the commit
	revoked, err := t.RevokeException(ctx, params.ID, actor)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	if !revoked {
		return nil, kerrors.New(kerrors.NotFound, "exception `%s`", params.ID)
	}
	audit := requestAudit(c.access, "vulnerability.exception.revoked", "vulnerability", params.ID.String())
	if err := t.AppendAudit(ctx, audit); err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	if err := t.Commit(ctx); err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	return &gen.RevokeVulnerabilityExceptionNoContent{}, nil
}
