package agentlink_test

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Teamtem-dev/kuben/internal/agentlink"
	"github.com/Teamtem-dev/kuben/internal/agentlink/protocol"
	"github.com/Teamtem-dev/kuben/internal/core/ids"
	"github.com/Teamtem-dev/kuben/internal/core/model"
	"github.com/Teamtem-dev/kuben/internal/store"
	"github.com/Teamtem-dev/kuben/internal/store/pgtest"
)

// Ported from crates/kuben-platform/src/agentlink.rs.

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func mode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}

func TestTheClusterCAIsMadeOnceAndItsKeyKeptPrivate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "agentlink")
	first, err := agentlink.ClusterCAIn(dir, quiet())
	if err != nil {
		t.Fatal(err)
	}
	kept, err := agentlink.ClusterCAIn(dir, quiet())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(kept.Certificate().Raw, first.Certificate().Raw) {
		t.Fatal("the same trust after a restart")
	}
	if m := mode(t, filepath.Join(dir, agentlink.CAKey)); m != 0o600 {
		t.Fatalf("key mode %o", m)
	}
	if m := mode(t, dir); m != 0o700 {
		t.Fatalf("dir mode %o", m)
	}
}

func must[T any](t *testing.T, what string) func(T, error) T {
	t.Helper()
	return func(v T, err error) T {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		return v
	}
}

func commit(t *testing.T, tn *store.Tenant) {
	t.Helper()
	if err := tn.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
}

// Two clusters of one organization (plan §18.1, I21): the hub files a
// report under the cluster whose agent sent it, and SQL keeps a report on a
// target of the other cluster out, even for a newer generation.
func TestAnAgentReportsOnlyOnTheTargetsOfItsOwnCluster(t *testing.T) {
	st := pgtest.Store(t)
	ctx := t.Context()
	org := must[model.Organization](t, "org")(st.CreateOrg(ctx, "a", "A")).ID
	tn := must[*store.Tenant](t, "tenant")(st.Tenant(ctx, org))
	primary := must[ids.ClusterID](t, "cluster")(tn.CreateCluster(ctx, "primary"))
	secondary := must[ids.ClusterID](t, "cluster")(tn.CreateCluster(ctx, "secondary"))
	project := must[ids.ProjectID](t, "project")(tn.CreateProject(ctx, "shop", "Shop"))
	targets := make([]ids.TargetID, 0, 2)
	for _, c := range []struct {
		slug    string
		cluster ids.ClusterID
	}{{"prod", primary}, {"edge", secondary}} {
		env := must[ids.EnvironmentID](t, "environment")(tn.CreateEnvironment(ctx, project, c.slug, c.slug, false))
		placement := must[ids.PlacementID](t, "placement")(tn.CreatePlacement(ctx, project, env, c.cluster, "kb-shop-"+c.slug))
		application := must[ids.ApplicationID](t, "application")(tn.CreateApplication(ctx, project, "web-"+c.slug, "Web"))
		targets = append(targets, must[ids.TargetID](t, "target")(tn.CreateTarget(ctx, project, application, placement)))
	}
	commit(t, tn)
	for _, cluster := range []ids.ClusterID{primary, secondary} {
		if err := st.RecordAgentCertificate(ctx, org, cluster, "sha256:"+cluster.String(), 1<<62); err != nil {
			t.Fatal(err)
		}
	}
	registry := agentlink.SQLRegistry{Store: st, Logger: quiet()}
	report := func(target ids.TargetID, generation int64, phase protocol.RuntimePhase) protocol.Observation {
		return protocol.Observation{Target: target.String(), Generation: generation, Phase: phase}
	}
	if len(targets) != 2 {
		t.Fatal(targets)
	}
	prod, edge := targets[0], targets[1]
	registry.Observed(ctx, primary.String(), "sha256:p", report(prod, 1, protocol.RuntimePhaseReady))
	registry.Observed(ctx, secondary.String(), "sha256:s", report(edge, 1, protocol.RuntimePhaseReady))
	// The secondary cluster's agent reports on the primary's target.
	registry.Observed(ctx, secondary.String(), "sha256:s", report(prod, 2, protocol.RuntimePhaseFailed))

	tn = must[*store.Tenant](t, "tenant")(st.Tenant(ctx, org))
	defer tn.Rollback(ctx) //nolint:errcheck // a read
	for name, target := range map[string]ids.TargetID{"prod": prod, "edge": edge} {
		o, found, err := tn.RuntimeObservation(ctx, target)
		if err != nil || !found || o.Generation != 1 || o.Phase != "ready" {
			t.Errorf("%s: %+v %v %v (the other cluster's report is kept out)", name, o, found, err)
		}
	}
}

func TestSQLAdmitsOnlyTheDeviceATokenEnrolledUntilItIsRevoked(t *testing.T) {
	st := pgtest.Store(t)
	ctx := t.Context()
	org := must[model.Organization](t, "org")(st.CreateOrg(ctx, "a", "A")).ID
	tn := must[*store.Tenant](t, "tenant")(st.Tenant(ctx, org))
	cluster := must[ids.ClusterID](t, "cluster")(tn.CreateCluster(ctx, "primary"))
	token := must[string](t, "token")(agentlink.NewToken())
	if created := must[bool](t, "token")(tn.CreateAgentToken(ctx, cluster, agentlink.TokenHash(token), 30*time.Minute, "test")); !created {
		t.Fatal("not created")
	}
	commit(t, tn)

	tokens := agentlink.SQLTokens{Store: st, Logger: quiet()}
	registry := agentlink.SQLRegistry{Store: st, Logger: quiet()}
	id := cluster.String()
	now := time.Now()
	if redeemed, err := tokens.Redeem(ctx, agentlink.TokenHash(token), id, "sha256:a", now); err != nil || redeemed != agentlink.RedeemedFirst {
		t.Fatal(redeemed, err)
	}
	if registry.Admits(ctx, id, "sha256:a") {
		t.Fatal("nothing certified yet")
	}
	registry.Certified(ctx, id, "sha256:a", now.Add(24*time.Hour))
	if !registry.Admits(ctx, id, "sha256:a") {
		t.Fatal("certified")
	}
	if registry.Admits(ctx, id, "sha256:b") {
		t.Fatal("another device")
	}
	if registry.Admits(ctx, "not-a-uuid", "sha256:a") {
		t.Fatal("not a cluster id")
	}

	tn = must[*store.Tenant](t, "tenant")(st.Tenant(ctx, org))
	if revoked := must[bool](t, "revoke")(tn.RevokeClusterAgent(ctx, cluster)); !revoked {
		t.Fatal("not revoked")
	}
	commit(t, tn)
	if registry.Admits(ctx, id, "sha256:a") {
		t.Fatal("revoked")
	}
	if _, err := tokens.Redeem(ctx, agentlink.TokenHash(token), id, "sha256:b", now); !errors.Is(err, agentlink.TokenOtherDevice) {
		t.Fatal(err)
	}
}
