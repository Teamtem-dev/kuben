package notify_test

import (
	"context"
	"log/slog"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/clock"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/ids"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/source"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/gitprovider"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/notify"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/store"
)

type reported struct {
	org        string
	connection string
	repository string
	commit     string
	status     gitprovider.CommitStatus
}

type reporter struct{ got []reported }

func (r *reporter) CommitStatus(
	_ context.Context, org ids.OrgID, connection ids.GitConnectionID, repository source.RepoName, commit string,
	status gitprovider.CommitStatus,
) error {
	r.got = append(r.got, reported{org.String(), connection.String(), repository.String(), commit, status})
	return nil
}

func TestConnectionSourcesReportTheirStatusesThroughTheConnection(t *testing.T) {
	const commit = "0123456789abcdef0123456789abcdef01234567"
	r := &reporter{}
	n := notify.New(notify.Deps{
		Connections: opt.Some[notify.StatusReporter](r), PublicURL: opt.Some("https://kuben.example.com"),
		Clock: clock.System{}, Logger: slog.New(slog.DiscardHandler),
	})
	org, connection := ids.New[ids.Org](), ids.New[ids.GitConnection]()
	c := store.OperationContext{
		Project: "shop", Environment: "prod", App: "web", Connection: opt.Some(connection),
		Source: opt.Some(`{"repository":"acme/platform/shop","commit":"` + commit + `"}`),
	}
	plan := notify.Plan{Event: "build.failed", Build: true, Code: opt.Some("buildFailed")}
	notify.ReportStatus(t.Context(), n, org, c, plan, "failure")
	want := []reported{{
		org: org.String(), connection: connection.String(), repository: "acme/platform/shop", commit: commit,
		status: gitprovider.CommitStatus{
			State: "failure", Context: "kuben/build", Description: "build.failed buildFailed",
			TargetURL: opt.Some("https://kuben.example.com/projects/shop/prod/web"),
		},
	}}
	if diff := cmp.Diff(want, r.got, cmp.AllowUnexported(reported{}, opt.Val[string]{})); diff != "" {
		t.Errorf("reported (-want +got):\n%s", diff)
	}
	// A source of the GitHub App, without an App, reports nothing.
	c.Connection = opt.None[ids.GitConnectionID]()
	c.InstallationID = opt.Some[int64](7)
	notify.ReportStatus(t.Context(), n, org, c, plan, "failure")
	if len(r.got) != 1 {
		t.Errorf("reported %+v", r.got)
	}
}
