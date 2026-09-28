package keyring

// The sealed values of integrations (2.1): a Git connection's token and
// webhook secret, an organization registry's password. Each is sealed for
// its organization, its integration's id and what it is, so one cannot be
// passed off as another; the ids are chosen before the row is written.

import (
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/ids"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/kerrors"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/store"
)

// integrationIdentity is who an integration's sealed value belongs to. The
// Secret part never parses as a secret's UUID, and revision 0 is no
// secret's revision.
func integrationIdentity(org ids.OrgID, kind store.IntegrationSealKind, id string) Identity {
	return Identity{Org: org.String(), Secret: string(kind) + "/" + id, Revision: 0}
}

// GitTokenIdentity is who a Git connection's token is sealed for.
func GitTokenIdentity(org ids.OrgID, connection ids.GitConnectionID) Identity {
	return integrationIdentity(org, store.SealGitToken, connection.String())
}

// GitWebhookIdentity is who a Git connection's webhook secret is sealed
// for.
func GitWebhookIdentity(org ids.OrgID, connection ids.GitConnectionID) Identity {
	return integrationIdentity(org, store.SealGitWebhook, connection.String())
}

// OrgRegistryIdentity is who an organization registry's password is sealed
// for.
func OrgRegistryIdentity(org ids.OrgID, registry ids.OrgRegistryID) Identity {
	return integrationIdentity(org, store.SealRegistryPassword, registry.String())
}

// IntegrationIdentity is who a stale integration seal of org belongs to.
func IntegrationIdentity(org ids.OrgID, seal store.IntegrationSeal) Identity {
	return integrationIdentity(org, seal.Kind, seal.ID.String())
}

// OpenOrgRegistry is the login of an organization registry, opened with k.
func (k *Keyring) OpenOrgRegistry(r store.OrgRegistry) (RegistryLogin, error) {
	password, err := k.Open(OrgRegistryIdentity(r.Org, r.ID), r.Password)
	if err != nil {
		return RegistryLogin{}, kerrors.New(kerrors.Internal, "opening a registry login failed: %s", err.Error())
	}
	defer clear(password)
	return RegistryLogin{Username: r.Username, Password: string(password)}, nil
}
