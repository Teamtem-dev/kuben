package materializer

import (
	"context"
	"fmt"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/keyring"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/store"
)

// WriteOrgRegistry is writeOrgRegistry; "" when it went through, else the
// stop it ended with.
func WriteOrgRegistry(ctx context.Context, w *Worker, m *store.Materialization, ring *keyring.Keyring) string {
	if st := w.writeOrgRegistry(ctx, m, ring); st != nil {
		return fmt.Sprintf("%#v", st)
	}
	return ""
}
