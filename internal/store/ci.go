package store

// CI trust policies and token exchange (M4.2, migration 0020; repo/ci.rs).
//
// An exchange records the provider token's id and issues the Kuben token in
// one transaction that holds the policy row: a replayed provider token gets
// nothing, and a revocation either precedes the exchange (nothing issued) or
// follows it (the new token is revoked with the others).

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Teamtem-dev/kuben/internal/core/ci"
	"github.com/Teamtem-dev/kuben/internal/core/ids"
	"github.com/Teamtem-dev/kuben/internal/core/model"
	"github.com/Teamtem-dev/kuben/internal/core/opt"
)

const ciProvider = "github-actions"

// The Rust built the selects from POLICY_COLUMNS with format!; the text is
// the same.
const (
	selectCIPolicies = "SELECT id, org_id, project_id, environment_id, name, repository, repository_id, " +
		"repository_owner_id, refs, environments, events, role, token_ttl_secs, created_by, created_at, revoked_at " +
		"FROM ci_trust_policies WHERE org_id = $1 ORDER BY created_at, id"
	selectCIPolicy = "SELECT id, org_id, project_id, environment_id, name, repository, repository_id, " +
		"repository_owner_id, refs, environments, events, role, token_ttl_secs, created_by, created_at, revoked_at " +
		"FROM ci_trust_policies WHERE id = $1"
	insertCIPolicy = "INSERT INTO ci_trust_policies " +
		"(id, org_id, project_id, environment_id, name, provider, repository_id, repository_owner_id, repository, " +
		"refs, environments, events, role, token_ttl_secs, created_by, created_at) " +
		"VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)"
	revokeCIPolicy = "UPDATE ci_trust_policies SET revoked_at = $3 " +
		"WHERE id = $1 AND org_id = $2 AND revoked_at IS NULL"
	revokeCIPolicyTokens = "UPDATE api_tokens SET revoked_at = $2 " +
		"WHERE ci_policy_id = $1 AND revoked_at IS NULL"
	holdCIPolicy = "SELECT revoked_at FROM ci_trust_policies WHERE id = $1 FOR SHARE"
	purgeCIUses  = "DELETE FROM ci_token_uses WHERE expires_at < $1"
	recordCIUse  = "INSERT INTO ci_token_uses (issuer, jti, policy_id, expires_at, used_at) " +
		"VALUES ($1, $2, $3, $4, $5) ON CONFLICT (issuer, jti) DO NOTHING"
	insertCIToken = "INSERT INTO api_tokens " +
		"(id, org_id, owner_user_id, name, prefix, secret_hash, scopes, expires_at, created_at, ci_policy_id) " +
		"VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)"
)

// NewCIPolicy is a new trust policy. The caller validates Policy.
type NewCIPolicy struct {
	Org         ids.OrgID
	Project     ids.ProjectID
	Environment opt.Val[ids.EnvironmentID]
	Name        string
	// Repository is `owner/name` when the policy is made.
	Repository string
	Policy     ci.TrustPolicy
	// CreatedBy is whose authority exchanged tokens act under.
	CreatedBy ids.UserID
}

// CIPolicy is a stored trust policy.
type CIPolicy struct {
	ID          uuid.UUID
	Org         ids.OrgID
	Project     ids.ProjectID
	Environment opt.Val[ids.EnvironmentID]
	Name        string
	Repository  string
	Policy      ci.TrustPolicy
	CreatedBy   ids.UserID
	CreatedAt   int64
	RevokedAt   opt.Val[int64]
}

// CIExchange is one accepted provider token and the Kuben token it becomes.
type CIExchange struct {
	Policy uuid.UUID
	Issuer string
	Jti    string
	// ProviderExpiresAt is when the provider token expires (Unix
	// milliseconds).
	ProviderExpiresAt int64
	Token             NewToken
}

// Exchanged is the outcome of [Store.ExchangeCIToken].
//
//sumtype:decl
type Exchanged interface{ exchanged() }

// ExchangedIssued is a token issued.
type ExchangedIssued struct{ Token model.APIToken }

// ExchangedReplayed is a provider token that was exchanged before.
type ExchangedReplayed struct{}

// ExchangedPolicyRevoked is a policy that was revoked or is gone.
type ExchangedPolicyRevoked struct{}

func (ExchangedIssued) exchanged()        {}
func (ExchangedReplayed) exchanged()      {}
func (ExchangedPolicyRevoked) exchanged() {}

func scanCIPolicy(row pgx.CollectableRow) (CIPolicy, error) {
	const op = "read a CI trust policy"
	var (
		p                  CIPolicy
		environment        *ids.EnvironmentID
		repoID, ownerID    int64
		role               string
		ttl                int32
		revoked            *int64
		refs, envs, events []string
	)
	if err := row.Scan(&p.ID, &p.Org, &p.Project, &environment, &p.Name, &p.Repository, &repoID, &ownerID,
		&refs, &envs, &events, &role, &ttl, &p.CreatedBy, &p.CreatedAt, &revoked); err != nil {
		return CIPolicy{}, err
	}
	var err error
	if p.Policy.RepositoryID, err = counter(op, repoID); err != nil {
		return CIPolicy{}, err
	}
	if p.Policy.RepositoryOwnerID, err = counter(op, ownerID); err != nil {
		return CIPolicy{}, err
	}
	if p.Policy.Role, err = parseRole(op, role); err != nil {
		return CIPolicy{}, err
	}
	if ttl < 0 {
		return CIPolicy{}, decodeErr(op, errOutOfRange)
	}
	p.Policy.TokenTTLSecs = uint32(ttl)
	p.Policy.Refs, p.Policy.Environments, p.Policy.Events = refs, envs, events
	p.Environment = opt.FromPtr(environment)
	p.RevokedAt = opt.FromPtr(revoked)
	return p, nil
}

// textArray is a list as a TEXT[] value: never NULL (sqlx bound an empty
// Vec as `{}`).
func textArray(list []string) []string {
	if list == nil {
		return []string{}
	}
	return list
}

// CreateCIPolicy records n. The caller validates the policy.
func (s *Store) CreateCIPolicy(ctx context.Context, n NewCIPolicy) (CIPolicy, error) {
	const op = "create a CI trust policy"
	id := uuid.Must(uuid.NewV7())
	createdAt := s.now()
	p := n.Policy
	p.Refs, p.Environments, p.Events = textArray(p.Refs), textArray(p.Environments), textArray(p.Events)
	repoID, err := signed(op, p.RepositoryID)
	if err != nil {
		return CIPolicy{}, err
	}
	ownerID, err := signed(op, p.RepositoryOwnerID)
	if err != nil {
		return CIPolicy{}, err
	}
	ttl, err := int4(op, p.TokenTTLSecs)
	if err != nil {
		return CIPolicy{}, err
	}
	var environment *uuid.UUID
	if e, ok := n.Environment.Get(); ok {
		u := e.UUID()
		environment = &u
	}
	_, err = exec(ctx, s.db, op, insertCIPolicy,
		id, n.Org.String(), n.Project.UUID(), environment, n.Name, ciProvider, repoID, ownerID, n.Repository,
		p.Refs, p.Environments, p.Events, string(p.Role), ttl,
		n.CreatedBy.String(), createdAt)
	if err != nil {
		return CIPolicy{}, err
	}
	return CIPolicy{
		ID:          id,
		Org:         n.Org,
		Project:     n.Project,
		Environment: n.Environment,
		Name:        n.Name,
		Repository:  n.Repository,
		Policy:      p,
		CreatedBy:   n.CreatedBy,
		CreatedAt:   createdAt,
	}, nil
}

// CIPolicies are the trust policies of org, oldest first.
func (s *Store) CIPolicies(ctx context.Context, org ids.OrgID) ([]CIPolicy, error) {
	return queryAll(ctx, s.db, "list CI trust policies", selectCIPolicies, scanCIPolicy, org.String())
}

// CIPolicy is a trust policy by id, whatever its organization (the
// exchange).
func (s *Store) CIPolicy(ctx context.Context, id uuid.UUID) (CIPolicy, bool, error) {
	return queryOpt(ctx, s.db, "read a CI trust policy", selectCIPolicy, scanCIPolicy, id)
}

// RevokeCIPolicy revokes policy id of org and every token issued under it.
// False when there is no such unrevoked policy.
func (s *Store) RevokeCIPolicy(ctx context.Context, org ids.OrgID, id uuid.UUID) (revoked bool, err error) {
	const op = "revoke a CI trust policy"
	now := s.now()
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return false, dbErr(op, err)
	}
	defer func() { err = errors.Join(err, rollback(ctx, tx)) }()
	n, err := exec(ctx, tx, op, revokeCIPolicy, id, org.String(), now)
	if err != nil || n == 0 {
		return false, err
	}
	if _, err := exec(ctx, tx, op, revokeCIPolicyTokens, id, now); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, dbErr(op, err)
	}
	return true, nil
}

// ExchangeCIToken accepts provider token e.Jti once and issues e.Token
// under policy e.Policy.
func (s *Store) ExchangeCIToken(ctx context.Context, e CIExchange) (out Exchanged, err error) {
	const op = "exchange a CI token"
	now := s.now()
	t := e.Token
	scopes, err := json.Marshal(t.Scope)
	if err != nil {
		return nil, dbErr(op, err)
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return nil, dbErr(op, err)
	}
	defer func() { err = errors.Join(err, rollback(ctx, tx)) }()
	held, found, err := queryOpt(ctx, tx, op, holdCIPolicy, pgx.RowTo[*int64], e.Policy)
	if err != nil {
		return nil, err
	}
	if !found || held != nil {
		return ExchangedPolicyRevoked{}, nil
	}
	if _, err := exec(ctx, tx, op, purgeCIUses, now); err != nil {
		return nil, err
	}
	recorded, err := exec(ctx, tx, op, recordCIUse, e.Issuer, e.Jti, e.Policy, e.ProviderExpiresAt, now)
	if err != nil {
		return nil, err
	}
	if recorded == 0 {
		return ExchangedReplayed{}, nil
	}
	if _, err := exec(ctx, tx, op, insertCIToken, t.ID.String(), t.OrgID.String(), t.Owner.String(), t.Name,
		t.Prefix, t.SecretHash, string(scopes), t.ExpiresAt.Ptr(), now, e.Policy); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, dbErr(op, err)
	}
	return ExchangedIssued{Token: model.APIToken{
		ID:         t.ID,
		OrgID:      t.OrgID,
		Owner:      opt.Some(t.Owner),
		Name:       t.Name,
		Prefix:     t.Prefix,
		SecretHash: t.SecretHash,
		Scope:      t.Scope,
		ExpiresAt:  t.ExpiresAt,
		CreatedAt:  now,
	}}, nil
}
