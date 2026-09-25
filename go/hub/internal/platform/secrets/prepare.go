package secrets

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/Teamtem-dev/kuben/go/hub/internal/core/ids"
	"github.com/Teamtem-dev/kuben/go/hub/internal/store"
)

// resealBatch is the revisions resealed per transaction.
const resealBatch = 100

// Prepare checks keyring against the keys this installation has used, then
// seals the data keys of older revisions under its current key. It fails
// when the keyring holds another key under a known version: sealing with it
// would make secrets unreadable to the other replicas. It returns the
// number resealed.
func Prepare(ctx context.Context, st *store.Store, keyring *Keyring, logger *slog.Logger) (int, error) {
	check, err := st.CheckKeyring(ctx, keyring.Fingerprints())
	if err != nil {
		return 0, fmt.Errorf("check the keyring: %w", err)
	}
	if len(check.Mismatched) > 0 {
		return 0, fmt.Errorf("the secret keyring holds other keys than this installation's under versions %v: "+
			"every replica must read the same keyring (secrets.keyring_file)", rustList(check.Mismatched))
	}
	if len(check.Missing) > 0 {
		logger.Warn("the secret keyring lacks keys this installation used: revisions sealed with them cannot be delivered",
			"versions", check.Missing)
	}
	current := keyring.Current()
	orgs, err := st.OrgIDs(ctx)
	if err != nil {
		return 0, fmt.Errorf("list organizations: %w", err)
	}
	resealed := 0
	for _, org := range orgs {
		n, err := resealOrg(ctx, st, keyring, org, logger)
		resealed += n
		if err != nil {
			return resealed, err
		}
	}
	if resealed > 0 {
		logger.Info("secret revisions resealed under the current key", "resealed", resealed, "key_version", current)
	}
	return resealed, nil
}

// resealOrg reseals org's stale revisions, a batch per transaction, until a
// batch moves nothing or comes back short.
func resealOrg(ctx context.Context, st *store.Store, keyring *Keyring, org ids.OrgID, logger *slog.Logger) (int, error) {
	resealed := 0
	for {
		moved, stale, err := resealBatchOf(ctx, st, keyring, org, logger)
		resealed += moved
		if err != nil {
			return resealed, err
		}
		if moved == 0 || stale < resealBatch {
			return resealed, nil
		}
	}
}

// resealBatchOf reseals one batch of org in one transaction: the number
// moved and the number found stale.
func resealBatchOf(
	ctx context.Context, st *store.Store, keyring *Keyring, org ids.OrgID, logger *slog.Logger,
) (moved, found int, err error) {
	tenant, err := st.Tenant(ctx, org)
	if err != nil {
		return 0, 0, fmt.Errorf("reseal: %w", err)
	}
	defer func() {
		if rbErr := tenant.Rollback(ctx); rbErr != nil {
			err = errors.Join(err, rbErr)
		}
	}()
	stale, err := tenant.StaleSeals(ctx, keyring.Current(), resealBatch)
	if err != nil {
		return 0, 0, fmt.Errorf("reseal: %w", err)
	}
	for _, s := range stale {
		who := Identity{Org: org.String(), Secret: s.Secret.String(), Revision: s.Revision}
		next, err := keyring.Rewrap(who, s.Sealed)
		if err != nil {
			logger.Warn("a secret revision cannot be resealed",
				"org", org.String(), "secret", s.Secret.String(), "revision", s.Revision, "error", err.Error())
			continue
		}
		ok, err := tenant.Reseal(ctx, s, next)
		if err != nil {
			return moved, len(stale), fmt.Errorf("reseal: %w", err)
		}
		if ok {
			moved++
		}
	}
	if err := tenant.Commit(ctx); err != nil {
		return 0, len(stale), fmt.Errorf("reseal: %w", err)
	}
	return moved, len(stale), nil
}

// rustList is a list of versions as Rust's `{:?}` printed it: `[1, 2]`.
func rustList(versions []uint32) string {
	out := "["
	for i, v := range versions {
		if i > 0 {
			out += ", "
		}
		out += fmt.Sprint(v)
	}
	return out + "]"
}
