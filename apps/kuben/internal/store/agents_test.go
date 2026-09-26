package store_test

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/ids"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/store"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/store/pgtest"
)

// Ported from repo/agents.rs.

const grace = time.Hour

func agentCluster(t *testing.T, s *store.Store, slug string) (ids.OrgID, ids.ClusterID) {
	t.Helper()
	o := org(t, s, slug, slug)
	tn := tenant(t, s, o)
	cluster := must[ids.ClusterID](t, "cluster")(tn.CreateCluster(t.Context(), "primary"))
	commit(t, tn)
	return o, cluster
}

func agentToken(t *testing.T, s *store.Store, o ids.OrgID, cluster ids.ClusterID, n byte, ttl time.Duration) bool {
	t.Helper()
	tn := tenant(t, s, o)
	created := must[bool](t, "create")(tn.CreateAgentToken(t.Context(), cluster, hashOf(n), ttl, "user:alice"))
	commit(t, tn)
	return created
}

func hashOf(n byte) [32]byte {
	var h [32]byte
	for i := range h {
		h[i] = n
	}
	return h
}

func TestATokenIsRedeemedOnceAndResumedByItsDevice(t *testing.T) {
	s := pgtest.Store(t)
	ctx := t.Context()
	o, cluster := agentCluster(t, s, "a")
	if !agentToken(t, s, o, cluster, 1, 30*time.Minute) {
		t.Fatal("not created")
	}
	first, err := s.RedeemAgentToken(ctx, hashOf(1), cluster, "sha256:device-a", grace)
	if err != nil {
		t.Fatal(err)
	}
	if first != (store.RedeemedToken{Org: o, Cluster: cluster, Kind: store.TokenFirst}) {
		t.Fatalf("%+v", first)
	}
	again, err := s.RedeemAgentToken(ctx, hashOf(1), cluster, "sha256:device-a", grace)
	if err != nil || again.Kind != store.TokenResumed {
		t.Fatalf("%+v %v", again, err)
	}
	if _, err := s.RedeemAgentToken(ctx, hashOf(1), cluster, "sha256:device-b", grace); !errors.Is(err, store.TokenOtherDevice) {
		t.Fatal(err)
	}
	if _, err := s.TestExec(ctx, "UPDATE agent_tokens SET device_id = 'sha256:device-b'"); err == nil {
		t.Fatal("a redeemed token keeps its device")
	}
}

func TestARedeemedTokenNamesTheOrganizationOfItsDevice(t *testing.T) {
	s := pgtest.Store(t)
	ctx := t.Context()
	o, cluster := agentCluster(t, s, "a")
	if !agentToken(t, s, o, cluster, 4, 30*time.Minute) {
		t.Fatal("not created")
	}
	if _, found, err := s.AgentTokenOrg(ctx, cluster, "sha256:device-a"); err != nil || found {
		t.Fatalf("not redeemed yet: %v %v", found, err)
	}
	if _, err := s.RedeemAgentToken(ctx, hashOf(4), cluster, "sha256:device-a", grace); err != nil {
		t.Fatal(err)
	}
	got, found, err := s.AgentTokenOrg(ctx, cluster, "sha256:device-a")
	if err != nil || !found || got != o {
		t.Fatalf("%v %v %v", got, found, err)
	}
}

// placementOn is a project with one environment on cluster: its id and
// placement.
func placementOn(t *testing.T, s *store.Store, o ids.OrgID, cluster ids.ClusterID) (ids.ProjectID, ids.PlacementID) {
	t.Helper()
	ctx := t.Context()
	tn := tenant(t, s, o)
	project := must[ids.ProjectID](t, "project")(tn.CreateProject(ctx, "shop", "Shop"))
	env := must[ids.EnvironmentID](t, "environment")(tn.CreateEnvironment(ctx, project, "production", "Production", true))
	placement := must[ids.PlacementID](t, "placement")(tn.CreatePlacement(ctx, project, env, cluster, "kb-shop-production"))
	commit(t, tn)
	return project, placement
}

func agentTarget(t *testing.T, s *store.Store, o ids.OrgID, project ids.ProjectID, placement ids.PlacementID, slug string) ids.TargetID {
	t.Helper()
	ctx := t.Context()
	tn := tenant(t, s, o)
	application := must[ids.ApplicationID](t, "application")(tn.CreateApplication(ctx, project, slug, slug))
	tgt := must[ids.TargetID](t, "target")(tn.CreateTarget(ctx, project, application, placement))
	commit(t, tn)
	return tgt
}

func delivery(t *testing.T, s *store.Store, o ids.OrgID, tgt ids.TargetID) store.Delivery {
	t.Helper()
	tn := tenant(t, s, o)
	d, found, err := tn.TargetDelivery(t.Context(), tgt)
	if err != nil || !found {
		t.Fatalf("delivery: %v %v", found, err)
	}
	// Rust dropped the transaction here; the pool has 4 connections.
	if err := tn.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestNewTargetsOfAClusterWithALinkedAgentAreDeliveredByIt(t *testing.T) {
	s := pgtest.Store(t)
	ctx := t.Context()
	o, cluster := agentCluster(t, s, "a")
	project, placement := placementOn(t, s, o, cluster)
	before := agentTarget(t, s, o, project, placement, "before")
	if delivery(t, s, o, before) != store.DeliveryController {
		t.Fatal("no agent yet")
	}
	if err := s.RecordAgentCertificate(ctx, o, cluster, "sha256:device-a", 1); err != nil {
		t.Fatal(err)
	}
	without := agentTarget(t, s, o, project, placement, "without")
	if delivery(t, s, o, without) != store.DeliveryController {
		t.Fatal("enrolled, never linked")
	}
	if linked, err := s.RecordAgentLink(ctx, cluster, "sha256:device-a", 1, []string{store.RuntimeFeature}, "1.1.2"); err != nil || !linked {
		t.Fatal(linked, err)
	}
	linked := agentTarget(t, s, o, project, placement, "linked")
	if delivery(t, s, o, linked) != store.DeliveryAgent {
		t.Fatal("a linked agent delivers new targets")
	}
	if delivery(t, s, o, before) != store.DeliveryController {
		t.Fatal("existing targets stay")
	}

	tn := tenant(t, s, o)
	if _, err := tn.TestExec(ctx, "UPDATE application_targets SET delivery = 'controller' WHERE id = $1", linked); err == nil {
		t.Fatal("a target delivered by its agent stays so")
	}
	if err := tn.Rollback(ctx); err != nil {
		t.Fatal(err)
	}

	tn = tenant(t, s, o)
	if revoked, err := tn.RevokeClusterAgent(ctx, cluster); err != nil || !revoked {
		t.Fatal(revoked, err)
	}
	commit(t, tn)
	revoked := agentTarget(t, s, o, project, placement, "revoked")
	if delivery(t, s, o, revoked) != store.DeliveryController {
		t.Fatal("a revoked agent")
	}
}

func TestATargetIsHandedOverOnlyToALinkedAgentThatCarriesApplications(t *testing.T) {
	s := pgtest.Store(t)
	ctx := t.Context()
	o, cluster := agentCluster(t, s, "a")
	project, placement := placementOn(t, s, o, cluster)
	web := agentTarget(t, s, o, project, placement, "web")
	handOver := func(tgt ids.TargetID) bool {
		tn := tenant(t, s, o)
		moved := must[bool](t, "hand over")(tn.HandOverToAgent(t.Context(), tgt))
		commit(t, tn)
		return moved
	}
	if handOver(web) {
		t.Fatal("no agent")
	}
	if err := s.RecordAgentCertificate(ctx, o, cluster, "sha256:device-a", 1); err != nil {
		t.Fatal(err)
	}
	if linked, err := s.RecordAgentLink(ctx, cluster, "sha256:device-a", 1, nil, "1.1.2"); err != nil || !linked {
		t.Fatal(linked, err)
	}
	if handOver(web) {
		t.Fatal("an agent without the runtime feature")
	}
	if linked, err := s.RecordAgentLink(ctx, cluster, "sha256:device-a", 1, []string{store.RuntimeFeature}, "1.1.2"); err != nil || !linked {
		t.Fatal(linked, err)
	}
	if !handOver(web) {
		t.Fatal("handed over")
	}
	if delivery(t, s, o, web) != store.DeliveryAgent {
		t.Fatal("delivered by the agent")
	}
	if handOver(web) {
		t.Fatal("handed over once")
	}
	other, _ := agentCluster(t, s, "b")
	tn := tenant(t, s, other)
	if moved := must[bool](t, "hand over")(tn.HandOverToAgent(ctx, web)); moved {
		t.Fatal("another organization's target")
	}
}

func TestAnAgentReportsOnlyOnTargetsOfItsClusterAndNeverBackwards(t *testing.T) {
	s := pgtest.Store(t)
	ctx := t.Context()
	o, cluster := agentCluster(t, s, "a")
	tn := tenant(t, s, o)
	elsewhere := must[ids.ClusterID](t, "cluster")(tn.CreateCluster(ctx, "secondary"))
	commit(t, tn)
	project, placement := placementOn(t, s, o, cluster)
	web := agentTarget(t, s, o, project, placement, "web")
	record := func(c ids.ClusterID, generation int64, phase string) bool {
		tn := tenant(t, s, o)
		recorded := must[bool](t, "record")(tn.RecordRuntimeObservation(t.Context(), c, web, generation, phase,
			opt.None[string](), opt.None[string]()))
		commit(t, tn)
		return recorded
	}
	if record(elsewhere, 2, "ready") {
		t.Fatal("another cluster's agent")
	}
	if !record(cluster, 2, "applying") {
		t.Fatal("applying")
	}
	if !record(cluster, 2, "ready") {
		t.Fatal("the same generation moves on")
	}
	if record(cluster, 1, "failed") {
		t.Fatal("an older generation")
	}
	tn = tenant(t, s, o)
	seen, found, err := tn.RuntimeObservation(ctx, web)
	if err != nil || !found || seen.Generation != 2 || seen.Phase != "ready" {
		t.Fatalf("%+v %v %v", seen, found, err)
	}
	other, _ := agentCluster(t, s, "b")
	tn = tenant(t, s, other)
	if _, found, err := tn.RuntimeObservation(ctx, web); err != nil || found {
		t.Fatal("another organization", found, err)
	}
}

func TestTokensAreBoundToTheirClusterAndExpire(t *testing.T) {
	s := pgtest.Store(t)
	ctx := t.Context()
	o, cluster := agentCluster(t, s, "a")
	_, other := agentCluster(t, s, "b")
	if !agentToken(t, s, o, cluster, 1, 30*time.Minute) || !agentToken(t, s, o, cluster, 2, 0) {
		t.Fatal("not created")
	}
	for _, c := range []struct {
		hash    byte
		cluster ids.ClusterID
		want    store.TokenRefusal
	}{
		{1, other, store.TokenOtherCluster},
		{2, cluster, store.TokenExpired},
		{9, cluster, store.TokenUnknown},
	} {
		if _, err := s.RedeemAgentToken(ctx, hashOf(c.hash), c.cluster, "sha256:d", grace); !errors.Is(err, c.want) {
			t.Errorf("token %d: %v, want %v", c.hash, err, c.want)
		}
	}
	// Another organization cannot mint a token for this cluster.
	otherOrg, _ := agentCluster(t, s, "c")
	if agentToken(t, s, otherOrg, cluster, 3, 30*time.Minute) {
		t.Fatal("minted for another organization's cluster")
	}
}

func TestARevokedDeviceStaysRevokedUntilAnotherEnrolls(t *testing.T) {
	s := pgtest.Store(t)
	ctx := t.Context()
	o, cluster := agentCluster(t, s, "a")
	if err := s.RecordAgentCertificate(ctx, o, cluster, "sha256:device-a", 1_000); err != nil {
		t.Fatal(err)
	}
	features := []string{"applicationRuntime"}
	if linked, err := s.RecordAgentLink(ctx, cluster, "sha256:device-a", 1, features, "1.1.2"); err != nil || !linked {
		t.Fatal(linked, err)
	}
	if linked, err := s.RecordAgentLink(ctx, cluster, "sha256:device-x", 1, features, "1.1.2"); err != nil || linked {
		t.Fatal("only the current device links", linked, err)
	}
	agent := must[store.ClusterAgent](t, "read")(readAgent(t, s, cluster))
	if !agent.Accepts("sha256:device-a") || agent.Accepts("sha256:device-x") {
		t.Fatalf("%+v", agent)
	}
	if v, _ := agent.ProtocolVersion.Get(); v != 1 || !slices.Equal(agent.Features, features) {
		t.Fatalf("%+v", agent)
	}

	otherOrg, _ := agentCluster(t, s, "b")
	tn := tenant(t, s, otherOrg)
	if revoked := must[bool](t, "revoke")(tn.RevokeClusterAgent(ctx, cluster)); revoked {
		t.Fatal("another organization")
	}
	tn = tenant(t, s, o)
	if revoked := must[bool](t, "revoke")(tn.RevokeClusterAgent(ctx, cluster)); !revoked {
		t.Fatal("not revoked")
	}
	commit(t, tn)
	if a := must[store.ClusterAgent](t, "read")(readAgent(t, s, cluster)); a.Accepts("sha256:device-a") {
		t.Fatal("revoked")
	}

	// A renewal of the same device keeps the revocation.
	if err := s.RecordAgentCertificate(ctx, o, cluster, "sha256:device-a", 2_000); err != nil {
		t.Fatal(err)
	}
	if a := must[store.ClusterAgent](t, "read")(readAgent(t, s, cluster)); a.RevokedAt.IsNone() {
		t.Fatal("still revoked")
	}
	if _, err := s.TestExec(ctx, "UPDATE cluster_agents SET revoked_at = NULL"); err == nil {
		t.Fatal("the same device stays revoked")
	}
	// A fresh enrollment with another device replaces it.
	if err := s.RecordAgentCertificate(ctx, o, cluster, "sha256:device-b", 3_000); err != nil {
		t.Fatal(err)
	}
	replaced := must[store.ClusterAgent](t, "read")(readAgent(t, s, cluster))
	if !replaced.Accepts("sha256:device-b") || replaced.Accepts("sha256:device-a") {
		t.Fatalf("%+v", replaced)
	}
}

func readAgent(t *testing.T, s *store.Store, cluster ids.ClusterID) (store.ClusterAgent, error) {
	t.Helper()
	a, found, err := s.ClusterAgent(t.Context(), cluster)
	if err == nil && !found {
		err = errors.New("no agent")
	}
	return a, err
}
