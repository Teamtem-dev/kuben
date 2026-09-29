package store

// Organization registries (2.1, migration 0035): a login for one registry
// host that every environment of the organization pulls with, unless the
// environment has its own login for that host (a `registry` secret,
// migration 0023), which wins. The password arrives sealed for the
// registry's id, which the caller therefore chooses.

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/ids"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/opt"
)

// RegistryPreset is the kind of registry a login was made for.
type RegistryPreset string

// The presets.
const (
	PresetDockerHub RegistryPreset = "dockerhub"
	PresetGHCR      RegistryPreset = "ghcr"
	PresetGitLab    RegistryPreset = "gitlab"
	PresetQuay      RegistryPreset = "quay"
	PresetHarbor    RegistryPreset = "harbor"
	PresetCustom    RegistryPreset = "custom"
)

// ParseRegistryPreset reads a preset's name.
func ParseRegistryPreset(s string) (RegistryPreset, error) {
	switch p := RegistryPreset(s); p {
	case PresetDockerHub, PresetGHCR, PresetGitLab, PresetQuay, PresetHarbor, PresetCustom:
		return p, nil
	}
	return "", fmt.Errorf("unknown registry preset %q", s)
}

const (
	orgRegistryColumns = "id, org_id, name, preset, server, username, password_ciphertext, password_wrapped_key, " +
		"password_key_version, created_by, created_at, updated_at, last_checked_at, last_error"
	insertOrgRegistry = "INSERT INTO org_registries (id, org_id, name, preset, server, username, " +
		"password_ciphertext, password_wrapped_key, password_key_version, created_by, created_at, updated_at) " +
		"VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $11) " +
		"ON CONFLICT DO NOTHING RETURNING " + orgRegistryColumns
	selectOrgRegistries = "SELECT " + orgRegistryColumns + " FROM org_registries " +
		"WHERE org_id = $1 ORDER BY name"
	selectOrgRegistry          = "SELECT " + orgRegistryColumns + " FROM org_registries WHERE id = $1 AND org_id = $2"
	selectOrgRegistryByName    = "SELECT " + orgRegistryColumns + " FROM org_registries WHERE name = $1 AND org_id = $2"
	selectOrgRegistryForServer = "SELECT " + orgRegistryColumns + " FROM org_registries WHERE server = $1 AND org_id = $2"
	orgRegistryTaken           = "SELECT EXISTS (SELECT 1 FROM org_registries WHERE org_id = $1 AND id <> $2 " +
		"AND (name = $3 OR server = $4))"
	updateOrgRegistry = "UPDATE org_registries SET " +
		"name = COALESCE($3::text, name), server = COALESCE($4::text, server), " +
		"username = COALESCE($5::text, username), " +
		"password_ciphertext = COALESCE($6::bytea, password_ciphertext), " +
		"password_wrapped_key = COALESCE($7::bytea, password_wrapped_key), " +
		"password_key_version = COALESCE($8::integer, password_key_version), " +
		"updated_at = $9 " +
		"WHERE id = $1 AND org_id = $2 RETURNING " + orgRegistryColumns
	orgRegistryChecked = "UPDATE org_registries SET last_checked_at = $3, last_error = $4 WHERE id = $1 AND org_id = $2"
	deleteOrgRegistry  = "DELETE FROM org_registries WHERE id = $1 AND org_id = $2"
)

// OrgRegistry is a stored organization registry login, its password
// sealed.
type OrgRegistry struct {
	ID     ids.OrgRegistryID
	Org    ids.OrgID
	Name   string
	Preset RegistryPreset
	// Server is the registry as image references name it: `ghcr.io`,
	// `docker.io`, `registry.example.com:5000`.
	Server        string
	Username      string
	Password      SealedBytes
	CreatedBy     string
	CreatedAt     int64
	UpdatedAt     int64
	LastCheckedAt opt.Val[int64]
	LastError     opt.Val[string]
}

// NewOrgRegistry is a login to add. ID is chosen by the caller: the
// password is sealed for it.
type NewOrgRegistry struct {
	ID        ids.OrgRegistryID
	Name      string
	Preset    RegistryPreset
	Server    string
	Username  string
	Password  SealedBytes
	CreatedBy string
}

// OrgRegistryChange is a change of a login: only what is set changes; a
// new password rotates it.
type OrgRegistryChange struct {
	Name     opt.Val[string]
	Server   opt.Val[string]
	Username opt.Val[string]
	Password opt.Val[SealedBytes]
}

// OrgRegistrySaved is the outcome of [Tenant.CreateOrgRegistry] and
// [Tenant.UpdateOrgRegistry].
//
//sumtype:decl
type OrgRegistrySaved interface{ orgRegistrySaved() }

type (
	// OrgRegistryStored is the login as saved.
	OrgRegistryStored struct{ Registry OrgRegistry }
	// OrgRegistryTaken means another login of the organization has the
	// name or the server.
	OrgRegistryTaken struct{}
	// OrgRegistryNotFound means the organization has no such login.
	OrgRegistryNotFound struct{}
)

func (OrgRegistryStored) orgRegistrySaved()   {}
func (OrgRegistryTaken) orgRegistrySaved()    {}
func (OrgRegistryNotFound) orgRegistrySaved() {}

func scanOrgRegistry(row pgx.CollectableRow) (OrgRegistry, error) {
	const op = "read a registry login"
	var (
		r           OrgRegistry
		id          uuid.UUID
		org, preset string
		version     int32
		checkedAt   *int64
		lastError   *string
	)
	if err := row.Scan(&id, &org, &r.Name, &preset, &r.Server, &r.Username, &r.Password.Ciphertext,
		&r.Password.WrappedKey, &version, &r.CreatedBy, &r.CreatedAt, &r.UpdatedAt, &checkedAt, &lastError); err != nil {
		return OrgRegistry{}, err
	}
	r.ID = ids.From[ids.OrgRegistry](id)
	var err error
	if r.Org, err = orgID(op, org); err != nil {
		return OrgRegistry{}, err
	}
	if r.Preset, err = parseColumn(op, preset, ParseRegistryPreset); err != nil {
		return OrgRegistry{}, err
	}
	if r.Password.KeyVersion, err = keyVersion(op, version); err != nil {
		return OrgRegistry{}, err
	}
	r.LastCheckedAt, r.LastError = opt.FromPtr(checkedAt), opt.FromPtr(lastError)
	return r, nil
}

// CreateOrgRegistry adds a login to the organization.
func (t *Tenant) CreateOrgRegistry(ctx context.Context, r NewOrgRegistry) (OrgRegistrySaved, error) {
	const op = "add a registry login"
	if _, err := ParseRegistryPreset(string(r.Preset)); err != nil {
		return nil, DatabaseError{Op: op, Err: err}
	}
	version, err := keyVersionColumn(op, r.Password.KeyVersion)
	if err != nil {
		return nil, err
	}
	saved, ok, err := queryOpt(ctx, t.tx, op, insertOrgRegistry, scanOrgRegistry,
		r.ID, t.org.String(), r.Name, string(r.Preset), r.Server, r.Username,
		r.Password.Ciphertext, r.Password.WrappedKey, version, r.CreatedBy, t.store.now())
	if err != nil {
		return nil, err
	}
	if !ok {
		return OrgRegistryTaken{}, nil
	}
	return OrgRegistryStored{Registry: saved}, nil
}

// OrgRegistries is the organization's logins by name.
func (t *Tenant) OrgRegistries(ctx context.Context) ([]OrgRegistry, error) {
	return queryAll(ctx, t.tx, "list registry logins", selectOrgRegistries, scanOrgRegistry, t.org.String())
}

// OrgRegistry is the organization's login id.
func (t *Tenant) OrgRegistry(ctx context.Context, id ids.OrgRegistryID) (OrgRegistry, bool, error) {
	return queryOpt(ctx, t.tx, "read a registry login", selectOrgRegistry, scanOrgRegistry, id, t.org.String())
}

// OrgRegistryByName is the organization's login called name.
func (t *Tenant) OrgRegistryByName(ctx context.Context, name string) (OrgRegistry, bool, error) {
	return queryOpt(ctx, t.tx, "read a registry login", selectOrgRegistryByName, scanOrgRegistry,
		name, t.org.String())
}

// OrgRegistryForServer is the organization's login for server (a registry
// as image references name it): the fallback when an environment has no
// login of its own for it.
func (t *Tenant) OrgRegistryForServer(ctx context.Context, server string) (OrgRegistry, bool, error) {
	return queryOpt(ctx, t.tx, "read a registry login", selectOrgRegistryForServer, scanOrgRegistry,
		server, t.org.String())
}

// UpdateOrgRegistry applies change to login id.
func (t *Tenant) UpdateOrgRegistry(ctx context.Context, id ids.OrgRegistryID, change OrgRegistryChange) (OrgRegistrySaved, error) {
	const op = "change a registry login"
	if change.Name.IsSome() || change.Server.IsSome() {
		var taken bool
		if err := queryOne(ctx, t.tx, op, orgRegistryTaken, []any{&taken}, t.org.String(), id,
			change.Name.Ptr(), change.Server.Ptr()); err != nil {
			return nil, err
		}
		if taken {
			return OrgRegistryTaken{}, nil
		}
	}
	var (
		ciphertext, wrapped []byte
		version             *int32
	)
	if password, ok := change.Password.Get(); ok {
		v, err := keyVersionColumn(op, password.KeyVersion)
		if err != nil {
			return nil, err
		}
		ciphertext, wrapped, version = password.Ciphertext, password.WrappedKey, &v
	}
	saved, ok, err := queryOpt(ctx, t.tx, op, updateOrgRegistry, scanOrgRegistry,
		id, t.org.String(), change.Name.Ptr(), change.Server.Ptr(), change.Username.Ptr(),
		ciphertext, wrapped, version, t.store.now())
	if err != nil {
		return nil, err
	}
	if !ok {
		return OrgRegistryNotFound{}, nil
	}
	return OrgRegistryStored{Registry: saved}, nil
}

// RecordOrgRegistryCheck records a login check of id; failure is why it
// failed, none when it passed. False when there is no such login.
func (t *Tenant) RecordOrgRegistryCheck(ctx context.Context, id ids.OrgRegistryID, failure opt.Val[string]) (bool, error) {
	var message *string
	if e, ok := failure.Get(); ok {
		m := truncateChars(e, 1024)
		message = &m
	}
	n, err := exec(ctx, t.tx, "record a registry login check", orgRegistryChecked, id, t.org.String(),
		t.store.now(), message)
	return n == 1, err
}

// DeleteOrgRegistry deletes login id; false when there is none.
func (t *Tenant) DeleteOrgRegistry(ctx context.Context, id ids.OrgRegistryID) (bool, error) {
	n, err := exec(ctx, t.tx, "delete a registry login", deleteOrgRegistry, id, t.org.String())
	return n == 1, err
}
