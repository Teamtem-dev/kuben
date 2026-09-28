package notify

import (
	"context"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/ids"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/store"
)

// ReportStatus exposes reportStatus to the tests.
func ReportStatus(ctx context.Context, n *Notifier, org ids.OrgID, c store.OperationContext, plan Plan, state string) {
	n.reportStatus(ctx, org, c, plan, state)
}
