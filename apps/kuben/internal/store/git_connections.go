package store

// Git connections (2.1, migration 0035): an organization's token for one
// Git provider account. The token and the webhook secret arrive sealed (the
// keyring seals them for the connection's id, which the caller therefore
// chooses): the store never sees either in clear.
//
// A push webhook names its connection before the organization is known, so
// git_connections has no row-level security, like git_installations: every
// tenant query here filters by organization, and [Store.GitConnectionOrg]
// is the one lookup without it.

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/ids"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/source"
)

// GitProvider is a Git hosting service a source reads from.
type GitProvider string

// The providers. Gitea covers Forgejo, which speaks its API.
const (
	GitProviderGitHub GitProvider = GitHub
	GitProviderGitLab GitProvider = "gitlab"
	GitProviderGitea  GitProvider = "gitea"
)

// ParseGitProvider reads a provider's name.
func ParseGitProvider(s string) (GitProvider, error) {
	switch p := GitProvider(s); p {
	case GitProviderGitHub, GitProviderGitLab, GitProviderGitea:
		return p, nil
	}
	return "", fmt.Errorf("unknown Git provider %q", s)
}

// ParseRepository reads a repository of p: a GitLab project may be nested
// in subgroups (`group/subgroup/…/name`), every other repository is
// `owner/name`.
func (p GitProvider) ParseRepository(s string) (source.RepoName, error) {
	if p == GitProviderGitLab {
		return source.ParseNestedRepoName(s) //nolint:wrapcheck // a source.Invalid
	}
	return source.ParseRepoName(s) //nolint:wrapcheck // a source.Invalid
}

// GitAuthToken is the one way a connection authenticates for now.
const GitAuthToken = "token"

const (
	gitConnectionColumns = "id, org_id, provider, name, base_url, auth_kind, token_ciphertext, token_wrapped_key, " +
		"token_key_version, token_hint, username, webhook_secret_ciphertext, webhook_secret_wrapped_key, " +
		"webhook_secret_key_version, default_branch, created_by, created_at, updated_at, last_checked_at, last_error"
	insertGitConnection = "INSERT INTO git_connections (id, org_id, provider, name, base_url, auth_kind, " +
		"token_ciphertext, token_wrapped_key, token_key_version, token_hint, username, webhook_secret_ciphertext, " +
		"webhook_secret_wrapped_key, webhook_secret_key_version, default_branch, created_by, created_at, updated_at) " +
		"VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $17) " +
		"ON CONFLICT DO NOTHING RETURNING " + gitConnectionColumns
	selectGitConnections = "SELECT " + gitConnectionColumns + " FROM git_connections " +
		"WHERE org_id = $1 ORDER BY created_at, id"
	selectGitConnection       = "SELECT " + gitConnectionColumns + " FROM git_connections WHERE id = $1 AND org_id = $2"
	selectGitConnectionByName = "SELECT " + gitConnectionColumns + " FROM git_connections WHERE name = $1 AND org_id = $2"
	gitConnectionNameTaken    = "SELECT EXISTS (SELECT 1 FROM git_connections WHERE org_id = $1 AND name = $2 AND id <> $3)"
	updateGitConnection       = "UPDATE git_connections SET " +
		"name = COALESCE($3::text, name), base_url = COALESCE($4::text, base_url), " +
		"token_ciphertext = COALESCE($5::bytea, token_ciphertext), " +
		"token_wrapped_key = COALESCE($6::bytea, token_wrapped_key), " +
		"token_key_version = COALESCE($7::integer, token_key_version), " +
		"token_hint = COALESCE($8::text, token_hint), " +
		"default_branch = CASE WHEN $9::boolean THEN $10::text ELSE default_branch END, " +
		"updated_at = $11, " +
		"webhook_secret_ciphertext = COALESCE($12::bytea, webhook_secret_ciphertext), " +
		"webhook_secret_wrapped_key = COALESCE($13::bytea, webhook_secret_wrapped_key), " +
		"webhook_secret_key_version = COALESCE($14::integer, webhook_secret_key_version) " +
		"WHERE id = $1 AND org_id = $2 RETURNING " + gitConnectionColumns
	gitConnectionChecked = "UPDATE git_connections SET last_checked_at = $3, last_error = $4, " +
		"username = COALESCE($5, username) WHERE id = $1 AND org_id = $2"
	lockGitConnection   = "SELECT id FROM git_connections WHERE id = $1 AND org_id = $2 FOR UPDATE"
	gitConnectionUsers  = "SELECT count(*) FROM source_bindings WHERE connection_id = $1 AND org_id = $2"
	deleteGitConnection = "DELETE FROM git_connections WHERE id = $1 AND org_id = $2"
	gitConnectionOrg    = "SELECT org_id FROM git_connections WHERE id = $1"
)

// GitConnection is a stored Git connection, its secrets sealed.
type GitConnection struct {
	ID       ids.GitConnectionID
	Org      ids.OrgID
	Provider GitProvider
	Name     string
	// BaseURL is the provider's API root, without a trailing slash.
	BaseURL string
	// AuthKind is [GitAuthToken].
	AuthKind string
	Token    SealedBytes
	// TokenHint is the token's last four characters at most.
	TokenHint     string
	Username      opt.Val[string]
	WebhookSecret SealedBytes
	DefaultBranch opt.Val[string]
	CreatedBy     string
	CreatedAt     int64
	UpdatedAt     int64
	LastCheckedAt opt.Val[int64]
	LastError     opt.Val[string]
}

// NewGitConnection is a connection to add. ID is chosen by the caller: the
// token and webhook secret are sealed for it.
type NewGitConnection struct {
	ID            ids.GitConnectionID
	Provider      GitProvider
	Name          string
	BaseURL       string
	Token         SealedBytes
	TokenHint     string
	Username      opt.Val[string]
	WebhookSecret SealedBytes
	DefaultBranch opt.Val[string]
	CreatedBy     string
}

// SealedToken is a new token, sealed, and its hint.
type SealedToken struct {
	Sealed SealedBytes
	Hint   string
}

// GitConnectionChange is a change of a connection: only what is set
// changes. DefaultBranch set to none clears it.
type GitConnectionChange struct {
	Name          opt.Val[string]
	BaseURL       opt.Val[string]
	Token         opt.Val[SealedToken]
	DefaultBranch opt.Val[opt.Val[string]]
	// WebhookSecret is a new webhook secret, sealed.
	WebhookSecret opt.Val[SealedBytes]
}

// GitConnectionCheck is what a check of a connection's token found.
type GitConnectionCheck struct {
	// Username is the account the token belongs to, when the check read it.
	Username opt.Val[string]
	// Error is why the check failed; none when it passed.
	Error opt.Val[string]
}

// GitConnectionSaved is the outcome of [Tenant.CreateGitConnection] and
// [Tenant.UpdateGitConnection].
//
//sumtype:decl
type GitConnectionSaved interface{ gitConnectionSaved() }

// GitConnectionDeleted is the outcome of [Tenant.DeleteGitConnection].
//
//sumtype:decl
type GitConnectionDeleted interface{ gitConnectionDeleted() }

type (
	// GitConnectionStored is the connection as saved.
	GitConnectionStored struct{ Connection GitConnection }
	// GitConnectionNameTaken means another connection of the organization
	// has the name (or, on create, the id).
	GitConnectionNameTaken struct{}
	// GitConnectionNotFound means the organization has no such connection.
	GitConnectionNotFound struct{}
	// GitConnectionRemoved means the connection is gone.
	GitConnectionRemoved struct{}
	// GitConnectionInUse means Bindings app sources read through the
	// connection: it stays.
	GitConnectionInUse struct{ Bindings int64 }
)

func (GitConnectionStored) gitConnectionSaved()     {}
func (GitConnectionNameTaken) gitConnectionSaved()  {}
func (GitConnectionNotFound) gitConnectionSaved()   {}
func (GitConnectionNotFound) gitConnectionDeleted() {}
func (GitConnectionRemoved) gitConnectionDeleted()  {}
func (GitConnectionInUse) gitConnectionDeleted()    {}

func scanGitConnection(row pgx.CollectableRow) (GitConnection, error) {
	const op = "read a Git connection"
	var (
		c                                  GitConnection
		id                                 uuid.UUID
		org, provider                      string
		tokenVersion, webhookVersion       int32
		username, defaultBranch, lastError *string
		checkedAt                          *int64
	)
	if err := row.Scan(&id, &org, &provider, &c.Name, &c.BaseURL, &c.AuthKind, &c.Token.Ciphertext,
		&c.Token.WrappedKey, &tokenVersion, &c.TokenHint, &username, &c.WebhookSecret.Ciphertext,
		&c.WebhookSecret.WrappedKey, &webhookVersion, &defaultBranch, &c.CreatedBy, &c.CreatedAt, &c.UpdatedAt,
		&checkedAt, &lastError); err != nil {
		return GitConnection{}, err
	}
	c.ID = ids.From[ids.GitConnection](id)
	var err error
	if c.Org, err = orgID(op, org); err != nil {
		return GitConnection{}, err
	}
	if c.Provider, err = parseColumn(op, provider, ParseGitProvider); err != nil {
		return GitConnection{}, err
	}
	if c.Token.KeyVersion, err = keyVersion(op, tokenVersion); err != nil {
		return GitConnection{}, err
	}
	if c.WebhookSecret.KeyVersion, err = keyVersion(op, webhookVersion); err != nil {
		return GitConnection{}, err
	}
	c.Username, c.DefaultBranch = opt.FromPtr(username), opt.FromPtr(defaultBranch)
	c.LastCheckedAt, c.LastError = opt.FromPtr(checkedAt), opt.FromPtr(lastError)
	return c, nil
}

// CreateGitConnection adds a connection to the organization.
func (t *Tenant) CreateGitConnection(ctx context.Context, c NewGitConnection) (GitConnectionSaved, error) {
	const op = "add a Git connection"
	if _, err := ParseGitProvider(string(c.Provider)); err != nil {
		return nil, DatabaseError{Op: op, Err: err}
	}
	tokenVersion, err := keyVersionColumn(op, c.Token.KeyVersion)
	if err != nil {
		return nil, err
	}
	webhookVersion, err := keyVersionColumn(op, c.WebhookSecret.KeyVersion)
	if err != nil {
		return nil, err
	}
	saved, ok, err := queryOpt(ctx, t.tx, op, insertGitConnection, scanGitConnection,
		c.ID, t.org.String(), string(c.Provider), c.Name, c.BaseURL, GitAuthToken,
		c.Token.Ciphertext, c.Token.WrappedKey, tokenVersion, c.TokenHint, c.Username.Ptr(),
		c.WebhookSecret.Ciphertext, c.WebhookSecret.WrappedKey, webhookVersion, c.DefaultBranch.Ptr(),
		c.CreatedBy, t.store.now())
	if err != nil {
		return nil, err
	}
	if !ok {
		return GitConnectionNameTaken{}, nil
	}
	return GitConnectionStored{Connection: saved}, nil
}

// GitConnections is the organization's connections, oldest first.
func (t *Tenant) GitConnections(ctx context.Context) ([]GitConnection, error) {
	return queryAll(ctx, t.tx, "list Git connections", selectGitConnections, scanGitConnection, t.org.String())
}

// GitConnection is the organization's connection id.
func (t *Tenant) GitConnection(ctx context.Context, id ids.GitConnectionID) (GitConnection, bool, error) {
	return queryOpt(ctx, t.tx, "read a Git connection", selectGitConnection, scanGitConnection, id, t.org.String())
}

// GitConnectionByName is the organization's connection called name.
func (t *Tenant) GitConnectionByName(ctx context.Context, name string) (GitConnection, bool, error) {
	return queryOpt(ctx, t.tx, "read a Git connection", selectGitConnectionByName, scanGitConnection,
		name, t.org.String())
}

// UpdateGitConnection applies change to connection id.
func (t *Tenant) UpdateGitConnection(
	ctx context.Context, id ids.GitConnectionID, change GitConnectionChange,
) (GitConnectionSaved, error) {
	const op = "change a Git connection"
	if name, ok := change.Name.Get(); ok {
		var taken bool
		if err := queryOne(ctx, t.tx, op, gitConnectionNameTaken, []any{&taken}, t.org.String(), name, id); err != nil {
			return nil, err
		}
		if taken {
			return GitConnectionNameTaken{}, nil
		}
	}
	var (
		ciphertext, wrapped []byte
		version             *int32
		hint                *string
	)
	if token, ok := change.Token.Get(); ok {
		v, err := keyVersionColumn(op, token.Sealed.KeyVersion)
		if err != nil {
			return nil, err
		}
		ciphertext, wrapped, version, hint = token.Sealed.Ciphertext, token.Sealed.WrappedKey, &v, &token.Hint
	}
	var (
		secretCiphertext, secretWrapped []byte
		secretVersion                   *int32
	)
	if secret, ok := change.WebhookSecret.Get(); ok {
		v, err := keyVersionColumn(op, secret.KeyVersion)
		if err != nil {
			return nil, err
		}
		secretCiphertext, secretWrapped, secretVersion = secret.Ciphertext, secret.WrappedKey, &v
	}
	branch, setBranch := change.DefaultBranch.Get()
	saved, ok, err := queryOpt(ctx, t.tx, op, updateGitConnection, scanGitConnection,
		id, t.org.String(), change.Name.Ptr(), change.BaseURL.Ptr(), ciphertext, wrapped, version, hint,
		setBranch, branch.Ptr(), t.store.now(), secretCiphertext, secretWrapped, secretVersion)
	if err != nil {
		return nil, err
	}
	if !ok {
		return GitConnectionNotFound{}, nil
	}
	return GitConnectionStored{Connection: saved}, nil
}

// RecordGitConnectionCheck records a check of connection id's token; false
// when there is no such connection.
func (t *Tenant) RecordGitConnectionCheck(ctx context.Context, id ids.GitConnectionID, check GitConnectionCheck) (bool, error) {
	var message *string
	if e, ok := check.Error.Get(); ok {
		m := truncateChars(e, 1024)
		message = &m
	}
	n, err := exec(ctx, t.tx, "record a Git connection check", gitConnectionChecked, id, t.org.String(),
		t.store.now(), message, check.Username.Ptr())
	return n == 1, err
}

// DeleteGitConnection deletes connection id unless an app source reads
// through it.
func (t *Tenant) DeleteGitConnection(ctx context.Context, id ids.GitConnectionID) (GitConnectionDeleted, error) {
	const op = "delete a Git connection"
	_, found, err := queryOpt(ctx, t.tx, op, lockGitConnection, pgx.RowTo[uuid.UUID], id, t.org.String())
	if err != nil {
		return nil, err
	}
	if !found {
		return GitConnectionNotFound{}, nil
	}
	var users int64
	if err := queryOne(ctx, t.tx, op, gitConnectionUsers, []any{&users}, id, t.org.String()); err != nil {
		return nil, err
	}
	if users > 0 {
		return GitConnectionInUse{Bindings: users}, nil
	}
	if _, err := exec(ctx, t.tx, op, deleteGitConnection, id, t.org.String()); err != nil {
		return nil, err
	}
	return GitConnectionRemoved{}, nil
}

// GitConnectionOrg is the organization connection id belongs to, for a push
// webhook that names only the connection; false when there is none.
func (s *Store) GitConnectionOrg(ctx context.Context, id ids.GitConnectionID) (ids.OrgID, bool, error) {
	const op = "read a Git connection's organization"
	org, found, err := queryOpt(ctx, s.db, op, gitConnectionOrg, pgx.RowTo[string], id)
	if err != nil || !found {
		return ids.OrgID{}, false, err
	}
	o, err := orgID(op, org)
	if err != nil {
		return ids.OrgID{}, false, err
	}
	return o, true, nil
}
