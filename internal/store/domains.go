package store

// Domain claims, DNS provider accounts and the records Kuben wrote through
// them (M5.2, migration 0031); the port of repo/domains.rs.
//
// verified_domains has no row-level security: which organization verified
// a name must be visible to every organization, so claims that could
// overlap are serialized by an advisory lock on the domain's last two
// labels and checked against every organization's verified domains.

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	domain "github.com/Teamtem-dev/kuben/internal/core/dnsname"
	"github.com/Teamtem-dev/kuben/internal/core/ids"
	"github.com/Teamtem-dev/kuben/internal/core/opt"
)

// claimsSelect is the claims, with a filter (the claims! macro).
const claimsSelect = "SELECT id, domain, token, status, method, provider_id, created_by, created_at, verified_at, " +
	"revoked_at, revoked_by, last_checked_at, last_error FROM domain_claims "

const (
	selectClaims = claimsSelect +
		"WHERE org_id = $1 AND ($2 OR status <> 'revoked') ORDER BY domain, created_at DESC"
	selectClaim = claimsSelect + "WHERE org_id = $1 AND id = $2"
	lockClaim   = claimsSelect + "WHERE org_id = $1 AND id = $2 FOR UPDATE"
	insertClaim = "INSERT INTO domain_claims (id, org_id, domain, token, created_by, created_at) " +
		"VALUES ($1, $2, $3, $4, $5, $6)"
	claimChecked = "UPDATE domain_claims SET last_checked_at = $3, last_error = $4 " +
		"WHERE org_id = $1 AND id = $2"
	verifyClaim = "UPDATE domain_claims SET status = 'verified', method = $3, provider_id = $4, " +
		"verified_at = $5, last_checked_at = $5, last_error = NULL " +
		"WHERE org_id = $1 AND id = $2 AND status = 'pending'"
	revokeClaim = "UPDATE domain_claims SET status = 'revoked', revoked_at = $3, revoked_by = $4, " +
		"verified_at = NULL WHERE org_id = $1 AND id = $2 AND status <> 'revoked'"
	lockSuffix  = "SELECT pg_advisory_xact_lock(hashtextextended($1, 5))"
	overlapping = "SELECT domain, org_id FROM verified_domains " +
		"WHERE domain = $1 OR domain LIKE '%.' || $1 OR $1 LIKE '%.' || domain"
	registerDomain = "INSERT INTO verified_domains (domain, org_id, claim_id, since) VALUES ($1, $2, $3, $4)"
	unregister     = "DELETE FROM verified_domains WHERE claim_id = $1 AND org_id = $2"
	domainOwner    = "SELECT domain, org_id FROM verified_domains WHERE domain = ANY($1) " +
		"ORDER BY char_length(domain) DESC LIMIT 1"
	selectProviders = "SELECT id, name, kind, created_by, created_at FROM dns_providers " +
		"WHERE org_id = $1 AND deleted_at IS NULL ORDER BY name"
	insertProvider = "INSERT INTO dns_providers " +
		"(id, org_id, name, kind, secret, wrapped_key, key_version, created_by, created_at) " +
		"VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)"
	providerSecret = "SELECT kind, secret, wrapped_key, key_version FROM dns_providers " +
		"WHERE org_id = $1 AND id = $2 AND deleted_at IS NULL"
	deleteProvider = "UPDATE dns_providers SET deleted_at = $3 " +
		"WHERE org_id = $1 AND id = $2 AND deleted_at IS NULL"
	recordsOf = "SELECT id, provider_id, target_id, name, record_type, content, zone_id, provider_ref " +
		"FROM dns_records WHERE org_id = $1 AND target_id = $2 ORDER BY name, record_type"
	upsertRecord = "INSERT INTO dns_records " +
		"(id, org_id, provider_id, target_id, name, record_type, content, zone_id, provider_ref, created_at, updated_at) " +
		"VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $10) " +
		"ON CONFLICT (provider_id, name, record_type, content) DO UPDATE " +
		"SET target_id = EXCLUDED.target_id, zone_id = EXCLUDED.zone_id, provider_ref = EXCLUDED.provider_ref, " +
		"updated_at = EXCLUDED.updated_at " +
		"RETURNING id"
	forgetRecord = "DELETE FROM dns_records WHERE org_id = $1 AND id = $2"
)

// maxClaimError is the longest failure text kept on a claim, in characters
// (the column's CHECK).
const maxClaimError = 1024

// DomainClaim is an organization's claim on a domain.
type DomainClaim struct {
	ID     uuid.UUID
	Domain string
	// Token is the TXT value that proves the claim.
	Token string
	// Status is `pending`, `verified` or `revoked`.
	Status string
	// Method is `txt` or the provider kind that proved it.
	Method        opt.Val[string]
	ProviderID    opt.Val[uuid.UUID]
	CreatedBy     string
	CreatedAt     int64
	VerifiedAt    opt.Val[int64]
	RevokedAt     opt.Val[int64]
	RevokedBy     opt.Val[string]
	LastCheckedAt opt.Val[int64]
	LastError     opt.Val[string]
}

// Verified is the outcome of [Tenant.VerifyClaim].
//
//sumtype:decl
type Verified interface{ isVerified() }

type (
	// ClaimVerified is a claim verified now.
	ClaimVerified struct{}
	// ClaimNotPending is a claim that is not pending (verified or revoked
	// already), or missing.
	ClaimNotPending struct{}
	// ClaimTaken is a claim another organization verified the overlapping
	// Domain of first.
	ClaimTaken struct{ Domain string }
)

func (ClaimVerified) isVerified()   {}
func (ClaimNotPending) isVerified() {}
func (ClaimTaken) isVerified()      {}

// DNSProvider is a DNS provider account, without its token.
type DNSProvider struct {
	ID        uuid.UUID
	Name      string
	Kind      string
	CreatedBy string
	CreatedAt int64
}

// DNSRecord is a DNS record Kuben wrote.
type DNSRecord struct {
	ID          uuid.UUID
	ProviderID  uuid.UUID
	TargetID    opt.Val[uuid.UUID]
	Name        string
	RecordType  string
	Content     string
	ZoneID      string
	ProviderRef opt.Val[string]
}

// NewDNSRecord is a record to remember.
type NewDNSRecord struct {
	Provider    uuid.UUID
	Target      opt.Val[ids.TargetID]
	Name        string
	RecordType  string
	Content     string
	ZoneID      string
	ProviderRef string
}

func scanClaim(row pgx.CollectableRow) (DomainClaim, error) {
	var c DomainClaim
	var method, revokedBy, lastError *string
	var provider *uuid.UUID
	var verifiedAt, revokedAt, lastChecked *int64
	if err := row.Scan(&c.ID, &c.Domain, &c.Token, &c.Status, &method, &provider, &c.CreatedBy, &c.CreatedAt,
		&verifiedAt, &revokedAt, &revokedBy, &lastChecked, &lastError); err != nil {
		return DomainClaim{}, err
	}
	c.Method, c.ProviderID, c.RevokedBy, c.LastError = opt.FromPtr(method), opt.FromPtr(provider),
		opt.FromPtr(revokedBy), opt.FromPtr(lastError)
	c.VerifiedAt, c.RevokedAt, c.LastCheckedAt = opt.FromPtr(verifiedAt), opt.FromPtr(revokedAt), opt.FromPtr(lastChecked)
	return c, nil
}

// CreateClaim claims domain (canonical) with the TXT value token.
func (t *Tenant) CreateClaim(ctx context.Context, id uuid.UUID, domainName, token, by string) error {
	_, err := exec(ctx, t.tx, "create a domain claim", insertClaim,
		id, t.org.String(), domainName, token, by, t.store.now())
	return err
}

// Claims are the organization's claims; revoked ones only with all.
func (t *Tenant) Claims(ctx context.Context, all bool) ([]DomainClaim, error) {
	return queryAll(ctx, t.tx, "list domain claims", selectClaims, scanClaim, t.org.String(), all)
}

// Claim is claim id, if it exists.
func (t *Tenant) Claim(ctx context.Context, id uuid.UUID) (DomainClaim, bool, error) {
	return queryOpt(ctx, t.tx, "read a domain claim", selectClaim, scanClaim, t.org.String(), id)
}

// ClaimChecked records a verification attempt of claim id that failed with
// failure (at most 1024 characters of it are kept).
func (t *Tenant) ClaimChecked(ctx context.Context, id uuid.UUID, failure opt.Val[string]) error {
	if text, ok := failure.Get(); ok {
		if runes := []rune(text); len(runes) > maxClaimError {
			failure = opt.Some(string(runes[:maxClaimError]))
		}
	}
	_, err := exec(ctx, t.tx, "record a claim check", claimChecked,
		t.org.String(), id, t.store.now(), failure.Ptr())
	return err
}

// heldDomain is a verified domain and the organization holding it.
type heldDomain struct{ domain, org string }

// VerifyClaim marks claim id verified by method (and provider), unless
// another organization holds an overlapping verified domain. Claims that
// could overlap are serialized by a lock on the domain's last two labels.
func (t *Tenant) VerifyClaim(ctx context.Context, id uuid.UUID, method string, provider opt.Val[uuid.UUID]) (Verified, error) {
	const op = "verify a domain claim"
	org := t.org.String()
	claim, found, err := queryOpt(ctx, t.tx, op, lockClaim, scanClaim, org, id)
	if err != nil {
		return nil, err
	}
	if !found || claim.Status != "pending" {
		return ClaimNotPending{}, nil
	}
	if _, err := exec(ctx, t.tx, op, lockSuffix, domain.LockKey(claim.Domain)); err != nil {
		return nil, err
	}
	held, err := queryAll(ctx, t.tx, op, overlapping, func(row pgx.CollectableRow) (heldDomain, error) {
		var h heldDomain
		err := row.Scan(&h.domain, &h.org)
		return h, err
	}, claim.Domain)
	if err != nil {
		return nil, err
	}
	for _, h := range held {
		if h.org != org && domain.Overlaps(h.domain, claim.Domain) {
			return ClaimTaken{Domain: h.domain}, nil
		}
	}
	now := t.store.now()
	if _, err := exec(ctx, t.tx, op, verifyClaim, org, id, method, provider.Ptr(), now); err != nil {
		return nil, err
	}
	registered := false
	for _, h := range held {
		registered = registered || h.domain == claim.Domain
	}
	if !registered {
		if _, err := exec(ctx, t.tx, op, registerDomain, claim.Domain, org, id, now); err != nil {
			return nil, err
		}
	}
	return ClaimVerified{}, nil
}

// RevokeClaim revokes claim id. False when it was revoked already or is
// missing.
func (t *Tenant) RevokeClaim(ctx context.Context, id uuid.UUID, by string) (bool, error) {
	const op = "revoke a domain claim"
	org := t.org.String()
	rows, err := exec(ctx, t.tx, op, revokeClaim, org, id, t.store.now(), by)
	if err != nil {
		return false, err
	}
	if _, err := exec(ctx, t.tx, op, unregister, id, org); err != nil {
		return false, err
	}
	return rows == 1, nil
}

// CreateDNSProvider adds a DNS provider account whose API token is secret
// (sealed for id).
func (t *Tenant) CreateDNSProvider(ctx context.Context, id uuid.UUID, name, kind string, secret SealedBytes, by string) error {
	const op = "create a DNS provider"
	version, err := keyVersionColumn(op, secret.KeyVersion)
	if err != nil {
		return err
	}
	_, err = exec(ctx, t.tx, op, insertProvider, id, t.org.String(), name, kind,
		secret.Ciphertext, secret.WrappedKey, version, by, t.store.now())
	return err
}

// DNSProviders are the organization's DNS provider accounts.
func (t *Tenant) DNSProviders(ctx context.Context) ([]DNSProvider, error) {
	return queryAll(ctx, t.tx, "list DNS providers", selectProviders, func(row pgx.CollectableRow) (DNSProvider, error) {
		var p DNSProvider
		err := row.Scan(&p.ID, &p.Name, &p.Kind, &p.CreatedBy, &p.CreatedAt)
		return p, err
	}, t.org.String())
}

// DNSProviderSecret is the kind and sealed token of provider id, if it
// exists.
func (t *Tenant) DNSProviderSecret(ctx context.Context, id uuid.UUID) (string, SealedBytes, bool, error) {
	const op = "read a DNS provider's token"
	type secret struct {
		kind   string
		sealed SealedBytes
	}
	s, found, err := queryOpt(ctx, t.tx, op, providerSecret, func(row pgx.CollectableRow) (secret, error) {
		var s secret
		var version int32
		if err := row.Scan(&s.kind, &s.sealed.Ciphertext, &s.sealed.WrappedKey, &version); err != nil {
			return secret{}, err
		}
		v, err := keyVersion(op, version)
		s.sealed.KeyVersion = v
		return s, err
	}, t.org.String(), id)
	return s.kind, s.sealed, found, err
}

// DeleteDNSProvider removes provider id. False when there is none.
func (t *Tenant) DeleteDNSProvider(ctx context.Context, id uuid.UUID) (bool, error) {
	rows, err := exec(ctx, t.tx, "delete a DNS provider", deleteProvider, t.org.String(), id, t.store.now())
	return rows == 1, err
}

// DNSRecords are the records Kuben wrote for tgt.
func (t *Tenant) DNSRecords(ctx context.Context, tgt ids.TargetID) ([]DNSRecord, error) {
	return queryAll(ctx, t.tx, "list DNS records", recordsOf, func(row pgx.CollectableRow) (DNSRecord, error) {
		var r DNSRecord
		var target *uuid.UUID
		var ref *string
		if err := row.Scan(&r.ID, &r.ProviderID, &target, &r.Name, &r.RecordType, &r.Content, &r.ZoneID, &ref); err != nil {
			return DNSRecord{}, err
		}
		r.TargetID, r.ProviderRef = opt.FromPtr(target), opt.FromPtr(ref)
		return r, nil
	}, t.org.String(), tgt)
}

// RecordDNS remembers a record written through a provider; its id (the
// existing row's for the same provider, name, type and content).
func (t *Tenant) RecordDNS(ctx context.Context, r NewDNSRecord) (uuid.UUID, error) {
	var target *uuid.UUID
	if tgt, ok := r.Target.Get(); ok {
		u := tgt.UUID()
		target = &u
	}
	var id uuid.UUID
	err := queryOne(ctx, t.tx, "record a DNS record", upsertRecord, []any{&id},
		uuid.Must(uuid.NewV7()), t.org.String(), r.Provider, target, r.Name, r.RecordType, r.Content,
		r.ZoneID, r.ProviderRef, t.store.now())
	return id, err
}

// ForgetDNSRecord forgets record id (it was deleted at the provider).
func (t *Tenant) ForgetDNSRecord(ctx context.Context, id uuid.UUID) error {
	_, err := exec(ctx, t.tx, "forget a DNS record", forgetRecord, t.org.String(), id)
	return err
}

// DomainOwner is the verified domain covering host (canonical; host itself
// or the closest parent) and the organization that verified it, if any:
// the most specific claim wins.
func (s *Store) DomainOwner(ctx context.Context, host string) (string, ids.OrgID, bool, error) {
	const op = "find a domain's owner"
	type owner struct {
		domain string
		org    ids.OrgID
	}
	o, found, err := queryOpt(ctx, s.db, op, domainOwner, func(row pgx.CollectableRow) (owner, error) {
		var o owner
		var org string
		if err := row.Scan(&o.domain, &org); err != nil {
			return owner{}, err
		}
		id, err := orgID(op, org)
		o.org = id
		return o, err
	}, domain.Ancestors(host))
	return o.domain, o.org, found, err
}
