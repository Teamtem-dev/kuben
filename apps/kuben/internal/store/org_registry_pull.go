package store

// Which organization login a deployment run pulls with (2.1): the
// organization's login for the registry of the release's image, unless the
// run is bound to its environment's own login for that registry (a
// `registry` secret, bound when the run was accepted), which wins.

import (
	"context"
	"strings"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/opt"
)

// ImageRegistry is the registry of an image repository as releases store
// it (`ghcr.io/acme/web` → `ghcr.io`): its first segment, as the binding
// of an environment's login reads it (migration 0023).
func ImageRegistry(repository string) string {
	registry, _, _ := strings.Cut(repository, "/")
	return registry
}

// pullRegistry is the organization's login a run of image repository,
// bound to secrets, pulls with; none when it has none for the registry or
// the run has its environment's login for it.
func (t *Tenant) pullRegistry(ctx context.Context, repository opt.Val[string], secrets []SecretBinding) (opt.Val[OrgRegistry], error) {
	registry := ImageRegistry(repository.Or(""))
	if registry == "" {
		return opt.None[OrgRegistry](), nil
	}
	for _, b := range secrets {
		if b.Registry.Or("") == registry {
			return opt.None[OrgRegistry](), nil
		}
	}
	r, found, err := t.OrgRegistryForServer(ctx, registry)
	if err != nil || !found {
		return opt.None[OrgRegistry](), err
	}
	return opt.Some(r), nil
}
