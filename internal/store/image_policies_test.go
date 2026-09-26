package store_test

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Teamtem-dev/kuben/internal/core/artifact"
	"github.com/Teamtem-dev/kuben/internal/core/ids"
	"github.com/Teamtem-dev/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/internal/store"
	"github.com/Teamtem-dev/kuben/internal/store/pgtest"
)

// repo/image_policies.rs policies_come_due_and_record_checks.
func TestPoliciesComeDueAndRecordChecks(t *testing.T) {
	s := pgtest.Store(t)
	ctx := t.Context()
	f := newControlsFixture(t, s)
	now := func() int64 { return time.Now().UnixMilli() }
	tn := tenant(t, s, f.org)
	err := tn.SetImagePolicy(ctx, store.NewImagePolicy{
		Project: f.project, Target: f.target, Repository: "docker.io/library/nginx", Pattern: "semver:^1",
		Enabled: true, IntervalSecs: 300, By: "u",
	})
	if err != nil {
		t.Fatal(err)
	}
	commit(t, tn)
	if orgs, err := s.ImagePolicyOrgs(ctx); err != nil || !slices.Equal(orgs, []ids.OrgID{f.org}) {
		t.Fatalf("orgs: %v %v", orgs, err)
	}

	tn = tenant(t, s, f.org)
	due, err := tn.DueImagePolicies(ctx, now()+1, 10)
	if err != nil || len(due) != 1 || due[0].Environment != f.environment {
		t.Fatalf("due: %+v %v", due, err)
	}
	digest := "sha256:" + strings.Repeat("a", 64)
	err = tn.ImagePolicyChecked(ctx, f.target, store.PolicyCheck{
		NextCheckAt: now() + 60_000, Tag: opt.Some("1.2.3"), Digest: opt.Some(digest),
	})
	if err != nil {
		t.Fatal(err)
	}
	if due, err := tn.DueImagePolicies(ctx, now(), 10); err != nil || len(due) != 0 {
		t.Fatalf("not due again yet: %+v %v", due, err)
	}
	err = tn.ImagePolicyChecked(ctx, f.target, store.PolicyCheck{
		NextCheckAt: now(), Failures: 2, Error: opt.Some("unreachable"),
	})
	if err != nil {
		t.Fatal(err)
	}
	policy, ok, err := tn.ImagePolicy(ctx, f.target)
	if err != nil || !ok {
		t.Fatalf("policy: %v %v", ok, err)
	}
	if policy.LastTag != opt.Some("1.2.3") || policy.Failures != 2 || policy.LastError != opt.Some("unreachable") {
		t.Fatalf("a failure keeps what was found before: %+v", policy)
	}
	if d, ok, err := tn.CurrentDigest(ctx, f.target); err != nil || ok {
		t.Fatalf("never deployed: %q %v %v", d, ok, err)
	}
	parsed, err := artifact.ParseDigest(digest)
	if err != nil {
		t.Fatal(err)
	}
	started, err := tn.DeployFollowedImage(ctx, policy, parsed, "nginx:1.2.3")
	if err != nil || started != (store.StartedNotFound{}) {
		t.Fatalf("an app without configuration is not deployed: %#v %v", started, err)
	}
	if deleted, err := tn.DeleteImagePolicy(ctx, f.target); err != nil || !deleted {
		t.Fatalf("delete: %v %v", deleted, err)
	}
	if deleted, err := tn.DeleteImagePolicy(ctx, f.target); err != nil || deleted {
		t.Fatalf("delete twice: %v %v", deleted, err)
	}
}
