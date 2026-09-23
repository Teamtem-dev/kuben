package materializer

// Managed secrets in the cluster (M4.4, ADR-030).
//
// Before a run writes its App or envelope, every secret revision it is
// bound to is opened and written as an immutable Secret named after the
// revision (SecretObjectName); the rendered objects read from those. A
// rotation is therefore a new object and a new pod template, never a change
// under running pods. Once a run succeeds, the revision objects of its
// environment that no run needs any more are removed.
//
// Opening a revision needs the secret keyring (crates/kuben-platform/src/
// secrets.rs), which arrives with slice S2. Until then this worker is the
// Rust worker without a keyring: a run bound to any secret fails with
// SecretsUnavailable, and there is nothing of its own to collect.

import (
	"context"

	"github.com/Teamtem-dev/kuben/go/hub/internal/store"
)

// writeSecrets writes the Secret of every revision m's run is bound to.
func (w *Worker) writeSecrets(_ context.Context, m *store.Materialization) stop {
	if len(m.Secrets) == 0 {
		return nil
	}
	return refused("SecretsUnavailable")
}
