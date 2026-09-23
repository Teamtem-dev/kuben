package store

// Verified domains (M5.2); a partial port of repo/domains.rs: which
// organization verified a hostname, which app creation checks. Claims, DNS
// providers and the 2 tests follow with the domain work (S2).

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/Teamtem-dev/kuben/go/hub/internal/core/domain"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/ids"
)

const domainOwner = "SELECT domain, org_id FROM verified_domains WHERE domain = ANY($1) " +
	"ORDER BY char_length(domain) DESC LIMIT 1"

// DomainOwner is the verified domain covering host (host itself or the
// closest parent) and the organization that verified it, if any.
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
