package api

// Domain claims, DNS provider accounts and an app's DNS records (M5.2,
// routes/domains.rs).
//
// An organization claims a domain and proves it in one of two ways:
//   - a TXT record `_kuben-challenge.<domain>` holding the claim's token,
//     read through DNS-over-HTTPS;
//   - a DNS provider account that holds the domain's zone.
//
// A verified domain is the organization's alone: no other organization
// can verify an overlapping name or serve a host below it.

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/Teamtem-dev/kuben/internal/core/authz"
	"github.com/Teamtem-dev/kuben/internal/core/dnsname"
	"github.com/Teamtem-dev/kuben/internal/core/ids"
	"github.com/Teamtem-dev/kuben/internal/core/kerrors"
	"github.com/Teamtem-dev/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/internal/core/perm"
	"github.com/Teamtem-dev/kuben/internal/httpapi/access"
	"github.com/Teamtem-dev/kuben/internal/httpapi/gen"
	"github.com/Teamtem-dev/kuben/internal/integrations/dns"
	"github.com/Teamtem-dev/kuben/internal/notify"
	"github.com/Teamtem-dev/kuben/internal/store"
)

// maxProviderToken is the longest DNS provider token accepted, in bytes.
const maxProviderToken = 512

// claimDto is ClaimDto::from.
func claimDto(c store.DomainClaim) gen.ClaimDto {
	stamp := func(ms opt.Val[int64]) gen.OptNilString {
		if v, ok := ms.Get(); ok {
			return gen.NewOptNilString(Timestamp(v))
		}
		return optNilString(opt.None[string]())
	}
	return gen.ClaimDto{
		ID:             c.ID,
		Domain:         c.Domain,
		Status:         c.Status,
		ChallengeName:  dnsname.ChallengeName(c.Domain),
		ChallengeValue: c.Token,
		Method:         optNilString(c.Method),
		CreatedBy:      c.CreatedBy,
		CreatedAt:      Timestamp(c.CreatedAt),
		VerifiedAt:     stamp(c.VerifiedAt),
		LastCheckedAt:  stamp(c.LastCheckedAt),
		LastError:      optNilString(c.LastError),
	}
}

// dnsProviderDto is DnsProviderDto::from.
func dnsProviderDto(p store.DNSProvider) gen.DnsProviderDto {
	return gen.DnsProviderDto{
		ID: p.ID, Name: p.Name, Kind: p.Kind, CreatedBy: p.CreatedBy, CreatedAt: Timestamp(p.CreatedAt),
	}
}

// randomToken is n random bytes, base64url without padding.
func randomToken(n int) string {
	raw := make([]byte, n)
	_, _ = rand.Read(raw) //nolint:errcheck // crypto/rand.Read never fails (Go 1.24+)
	return base64.RawURLEncoding.EncodeToString(raw)
}

// orgAccess is the caller and its organization (its first) with the proof
// of p over it; with forbidToken an API token is refused first.
func (s *Server) orgAccess(ctx context.Context, p perm.Perm, forbidToken bool) (access.Access, ids.OrgID, error) {
	a, err := s.access(ctx)
	if err != nil {
		return access.Access{}, ids.OrgID{}, err
	}
	if forbidToken {
		if err := a.ForbidToken(); err != nil {
			return access.Access{}, ids.OrgID{}, err //nolint:wrapcheck // a kerrors already
		}
	}
	org, err := orgOf(a)
	if err != nil {
		return access.Access{}, ids.OrgID{}, err
	}
	if _, err := a.Require(p, authz.OrgChain(org)); err != nil {
		return access.Access{}, ids.OrgID{}, err //nolint:wrapcheck // a kerrors already
	}
	return a, org, nil
}

// ListDomainClaims is the organization's domain claims.
func (s *Server) ListDomainClaims(ctx context.Context, params gen.ListDomainClaimsParams) ([]gen.ClaimDto, error) {
	_, org, err := s.orgAccess(ctx, perm.OrgRead, false)
	if err != nil {
		return nil, err
	}
	t, err := s.deps.Store.Tenant(ctx, org)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	defer t.Rollback(ctx) //nolint:errcheck // read only
	claims, err := t.Claims(ctx, params.All.Or(false))
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	out := make([]gen.ClaimDto, 0, len(claims))
	for _, c := range claims {
		out = append(out, claimDto(c))
	}
	return out, nil
}

// CreateDomainClaim claims a domain; verify it next.
func (s *Server) CreateDomainClaim(ctx context.Context, req *gen.CreateClaim) (gen.CreateDomainClaimRes, error) {
	a, org, err := s.orgAccess(ctx, perm.OrgAdmin, false)
	if err != nil {
		return nil, err
	}
	name, err := dnsname.Canonical(req.Domain)
	if err != nil {
		return nil, kerrors.New(kerrors.Validation, "%s", err.Error())
	}
	if _, owner, found, err := s.deps.Store.DomainOwner(ctx, name); err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	} else if found && owner != org {
		return nil, kerrors.New(kerrors.Conflict, "`%s` is claimed by another organization", name)
	}
	id := uuid.Must(uuid.NewV7())
	token := "kuben-" + randomToken(32)
	_, actor := a.Actor()
	t, err := s.deps.Store.Tenant(ctx, org)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	defer t.Rollback(ctx) //nolint:errcheck // a no-op after the commit
	if err := t.CreateClaim(ctx, id, name, token, actor); err != nil {
		return nil, duplicate(err, "a claim on `"+name+"`")
	}
	if err := t.AppendAudit(ctx, requestAudit(a, "domain.claimed", "domain", name)); err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	claim, found, err := t.Claim(ctx, id)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	if !found {
		return nil, kerrors.New(kerrors.Internal, "the new claim is missing")
	}
	if err := t.Commit(ctx); err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	dto := claimDto(claim)
	return &dto, nil
}

// openedProvider is a DNS provider account of the organization, ready to
// call.
type openedProvider struct {
	id   uuid.UUID
	kind string
	api  dns.Provider
}

// provider is the provider name of org, opened with the keyring.
func (s *Server) provider(ctx context.Context, t *store.Tenant, org ids.OrgID, name string) (openedProvider, error) {
	notFound := kerrors.New(kerrors.NotFound, "DNS provider `%s`", name)
	all, err := t.DNSProviders(ctx)
	if err != nil {
		return openedProvider{}, err //nolint:wrapcheck // a store error, answered as internal
	}
	i := slices.IndexFunc(all, func(p store.DNSProvider) bool { return p.Name == name })
	if i < 0 {
		return openedProvider{}, notFound
	}
	id := all[i].ID
	kind, sealed, found, err := t.DNSProviderSecret(ctx, id)
	if err != nil {
		return openedProvider{}, err //nolint:wrapcheck // a store error, answered as internal
	}
	if !found {
		return openedProvider{}, notFound
	}
	keyring, err := s.keyring()
	if err != nil {
		return openedProvider{}, err
	}
	token, err := notify.OpenSecret(keyring, org, id, sealed)
	if err != nil {
		return openedProvider{}, kerrors.New(kerrors.Internal, "%s", err.Error())
	}
	if !utf8.Valid(token) {
		return openedProvider{}, kerrors.New(kerrors.Internal, "a provider token is not text")
	}
	api, ok := s.deps.DNS.Provider(kind, string(token))
	if !ok {
		return openedProvider{}, kerrors.New(kerrors.Internal, "unknown DNS provider kind `%s`", kind)
	}
	return openedProvider{id: id, kind: kind, api: api}, nil
}

// proof is how a claim was proven: `txt` or a provider kind, and the
// provider.
type proof struct {
	method   string
	provider opt.Val[uuid.UUID]
}

// prove is how claim is proven now, with the provider account named by
// with or else its TXT record; false with the reason when it is not.
func (s *Server) prove(ctx context.Context, t *store.Tenant, org ids.OrgID, claim store.DomainClaim, with opt.Val[string]) (proof, string, bool, error) {
	if name, ok := with.Get(); ok {
		p, err := s.provider(ctx, t, org, name)
		if err != nil {
			return proof{}, "", false, err
		}
		zone, found, err := p.api.ZoneFor(ctx, claim.Domain)
		switch {
		case err != nil:
			return proof{}, err.Error(), false, nil //nolint:nilerr // a failed lookup is why the claim stays pending
		case found && dnsname.Covers(zone.Name, claim.Domain):
			return proof{method: p.kind, provider: opt.Some(p.id)}, "", true, nil
		}
		return proof{}, "the `" + name + "` account holds no zone for " + claim.Domain, false, nil
	}
	challenge := dnsname.ChallengeName(claim.Domain)
	values, err := s.deps.DNS.TXT(ctx, challenge)
	switch {
	case err != nil:
		return proof{}, err.Error(), false, nil //nolint:nilerr // a failed lookup is why the claim stays pending
	case slices.Contains(values, claim.Token):
		return proof{method: "txt"}, "", true, nil
	case len(values) == 0:
		return proof{}, "no TXT record " + challenge, false, nil
	}
	return proof{}, challenge + " does not hold the claim's value", false, nil
}

// VerifyDomainClaim verifies a claim through its TXT record or a DNS
// provider account; a claim still pending says why in lastError.
func (s *Server) VerifyDomainClaim(ctx context.Context, req *gen.VerifyClaim, params gen.VerifyDomainClaimParams) (gen.VerifyDomainClaimRes, error) {
	a, org, err := s.orgAccess(ctx, perm.OrgAdmin, false)
	if err != nil {
		return nil, err
	}
	t, err := s.deps.Store.Tenant(ctx, org)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	defer t.Rollback(ctx) //nolint:errcheck // a no-op after the commit
	claim, found, err := t.Claim(ctx, params.ID)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	if !found {
		return nil, kerrors.New(kerrors.NotFound, "claim `%s`", params.ID)
	}
	if claim.Status != "pending" {
		dto := claimDto(claim)
		return &dto, nil
	}
	with := opt.None[string]()
	if name, ok := req.Provider.Get(); ok {
		with = opt.Some(name)
	}
	p, why, proved, err := s.prove(ctx, t, org, claim, with)
	if err != nil {
		return nil, err
	}
	if proved {
		if err := s.markVerified(ctx, t, a, claim, p); err != nil {
			return nil, err
		}
	} else if err := t.ClaimChecked(ctx, params.ID, opt.Some(why)); err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	claim, found, err = t.Claim(ctx, params.ID)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	if !found {
		return nil, kerrors.New(kerrors.Internal, "the claim is missing")
	}
	if err := t.Commit(ctx); err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	dto := claimDto(claim)
	return &dto, nil
}

// markVerified records claim verified by p, unless another organization
// verified an overlapping domain first.
func (s *Server) markVerified(ctx context.Context, t *store.Tenant, a access.Access, claim store.DomainClaim, p proof) error {
	verified, err := t.VerifyClaim(ctx, claim.ID, p.method, p.provider)
	if err != nil {
		return err //nolint:wrapcheck // a store error, answered as internal
	}
	switch v := verified.(type) {
	case store.ClaimVerified, store.ClaimNotPending:
		return t.AppendAudit(ctx, requestAudit(a, "domain.verified", "domain", claim.Domain)) //nolint:wrapcheck // a store error, answered as internal
	case store.ClaimTaken:
		return kerrors.New(kerrors.Conflict, "`%s` is verified by another organization", v.Domain)
	case nil:
	}
	return kerrors.New(kerrors.Internal, "no verification outcome")
}

// RevokeDomainClaim revokes a claim: its apps keep their domains, but
// nothing protects them.
func (s *Server) RevokeDomainClaim(ctx context.Context, params gen.RevokeDomainClaimParams) (gen.RevokeDomainClaimRes, error) {
	a, org, err := s.orgAccess(ctx, perm.OrgAdmin, false)
	if err != nil {
		return nil, err
	}
	_, actor := a.Actor()
	t, err := s.deps.Store.Tenant(ctx, org)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	defer t.Rollback(ctx) //nolint:errcheck // a no-op after the commit
	claim, found, err := t.Claim(ctx, params.ID)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	if !found {
		return nil, kerrors.New(kerrors.NotFound, "claim `%s`", params.ID)
	}
	revoked, err := t.RevokeClaim(ctx, params.ID, actor)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	if !revoked {
		return nil, kerrors.New(kerrors.NotFound, "open claim `%s`", params.ID)
	}
	if err := t.AppendAudit(ctx, requestAudit(a, "domain.revoked", "domain", claim.Domain)); err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	if err := t.Commit(ctx); err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	return &gen.RevokeDomainClaimNoContent{}, nil
}

// ListDnsProviders is the organization's DNS provider accounts.
func (s *Server) ListDnsProviders(ctx context.Context) ([]gen.DnsProviderDto, error) { //nolint:revive // the generated interface's name
	_, org, err := s.orgAccess(ctx, perm.OrgRead, false)
	if err != nil {
		return nil, err
	}
	t, err := s.deps.Store.Tenant(ctx, org)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	defer t.Rollback(ctx) //nolint:errcheck // read only
	found, err := t.DNSProviders(ctx)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	out := make([]gen.DnsProviderDto, 0, len(found))
	for _, p := range found {
		out = append(out, dnsProviderDto(p))
	}
	return out, nil
}

// checkProviderToken checks a new account's token with the provider: an
// unreachable provider is unavailable, anything else refuses the request.
func (s *Server) checkProviderToken(ctx context.Context, kind, token string) error {
	api, ok := s.deps.DNS.Provider(kind, token)
	if !ok {
		return kerrors.New(kerrors.Validation, "unknown DNS provider kind `%s`", kind)
	}
	err := api.Verify(ctx)
	var dnsErr dns.Error
	switch {
	case err == nil:
		return nil
	case errors.As(err, &dnsErr) && dnsErr.Kind == dns.Unavailable:
		return kerrors.New(kerrors.Unavailable, "%s", dnsErr.Detail)
	}
	return kerrors.New(kerrors.Validation, "%s", err.Error())
}

// CreateDnsProvider adds a DNS provider account; its token is checked
// first and kept sealed.
func (s *Server) CreateDnsProvider(ctx context.Context, req *gen.CreateDnsProvider) (gen.CreateDnsProviderRes, error) { //nolint:revive // the generated interface's name
	a, org, err := s.orgAccess(ctx, perm.OrgAdmin, true)
	if err != nil {
		return nil, err
	}
	if err := DNSLabel("name", req.Name, 32); err != nil {
		return nil, err
	}
	token := strings.TrimSpace(req.Token)
	if token == "" || len(req.Token) > maxProviderToken {
		return nil, kerrors.New(kerrors.Validation, "token must be 1 to %d characters", maxProviderToken)
	}
	if err := s.checkProviderToken(ctx, req.Kind, token); err != nil {
		return nil, err
	}
	keyring, err := s.keyring()
	if err != nil {
		return nil, err
	}
	id := uuid.Must(uuid.NewV7())
	sealed, err := notify.SealSecret(keyring, org, id, []byte(token))
	if err != nil {
		return nil, kerrors.New(kerrors.Internal, "%s", err.Error())
	}
	_, actor := a.Actor()
	t, err := s.deps.Store.Tenant(ctx, org)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	defer t.Rollback(ctx) //nolint:errcheck // a no-op after the commit
	if err := t.CreateDNSProvider(ctx, id, req.Name, req.Kind, sealed, actor); err != nil {
		return nil, duplicate(err, "DNS provider `"+req.Name+"`")
	}
	if err := t.AppendAudit(ctx, requestAudit(a, "dns.provider.created", "dns-provider", req.Name)); err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	if err := t.Commit(ctx); err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	return &gen.DnsProviderDto{
		ID: id, Name: req.Name, Kind: req.Kind, CreatedBy: actor, CreatedAt: Timestamp(s.deps.Clock.NowMs()),
	}, nil
}

// DeleteDnsProvider removes a DNS provider account (its records stay at
// the provider).
func (s *Server) DeleteDnsProvider(ctx context.Context, params gen.DeleteDnsProviderParams) (gen.DeleteDnsProviderRes, error) { //nolint:revive // the generated interface's name
	a, org, err := s.orgAccess(ctx, perm.OrgAdmin, false)
	if err != nil {
		return nil, err
	}
	t, err := s.deps.Store.Tenant(ctx, org)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	defer t.Rollback(ctx) //nolint:errcheck // a no-op after the commit
	deleted, err := t.DeleteDNSProvider(ctx, params.ID)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	if !deleted {
		return nil, kerrors.New(kerrors.NotFound, "DNS provider `%s`", params.ID)
	}
	if err := t.AppendAudit(ctx, requestAudit(a, "dns.provider.deleted", "dns-provider", params.ID.String())); err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	if err := t.Commit(ctx); err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	return &gen.DeleteDnsProviderNoContent{}, nil
}
