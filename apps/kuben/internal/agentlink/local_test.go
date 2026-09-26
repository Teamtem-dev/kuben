package agentlink_test

import (
	"encoding/json"
	"testing"

	"github.com/google/go-cmp/cmp"
	corev1 "k8s.io/api/core/v1"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/agentlink"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/opt"
)

// Ported from crates/kuben-platform/src/local_agent.rs.

func TestATokenIsPublishedOnlyUntilTheAgentEnrolled(t *testing.T) {
	const now, hour = int64(1_000_000_000), int64(3_600_000)
	issue := func() (agentlink.PublishedToken, error) {
		return agentlink.PublishedToken{Token: "new", ExpiresAt: now + hour}, nil
	}
	token := func(value string, expires int64) opt.Val[agentlink.PublishedToken] {
		return opt.Some(agentlink.PublishedToken{Token: value, ExpiresAt: expires})
	}
	cases := []struct {
		name     string
		enrolled bool
		live     opt.Val[agentlink.PublishedToken]
		want     opt.Val[agentlink.PublishedToken]
	}{
		{"enrolled", true, token("old", now+hour), opt.None[agentlink.PublishedToken]()},
		{"a fresh token stays", false, token("old", now+hour), token("old", now+hour)},
		{"one close to its expiry is replaced", false, token("old", now+60_000), token("new", now+hour)},
		{"none yet", false, opt.None[agentlink.PublishedToken](), token("new", now+hour)},
	}
	for _, c := range cases {
		got, err := agentlink.TokenToPublish(c.enrolled, c.live, now, issue)
		if err != nil {
			t.Fatal(err)
		}
		if diff := cmp.Diff(c.want.Ptr(), got.Ptr()); diff != "" {
			t.Errorf("%s: %s", c.name, diff)
		}
	}
}

// secret reads the applied body back as the Secret the apiserver keeps.
func secret(t *testing.T, p agentlink.Published) *corev1.Secret {
	t.Helper()
	body, err := json.Marshal(agentlink.SecretBody(p))
	if err != nil {
		t.Fatal(err)
	}
	var s corev1.Secret
	if err := json.Unmarshal(body, &s); err != nil {
		t.Fatal(err)
	}
	if s.Name != agentlink.EnrollmentSecret || s.Kind != "Secret" || s.APIVersion != "v1" || s.Type != corev1.SecretTypeOpaque ||
		s.Labels["app.kubernetes.io/managed-by"] != "kuben" {
		t.Fatalf("%+v", s)
	}
	return &s
}

func TestTheSecretCarriesTheEnrollmentAndReadsBack(t *testing.T) {
	withToken := agentlink.Published{
		Hub: "kuben.kuben-system.svc:7443", CA: "-----BEGIN CERTIFICATE-----", Cluster: "0190",
		Token: opt.Some(agentlink.PublishedToken{Token: "kbt_x", ExpiresAt: 42}),
	}
	s := secret(t, withToken)
	if s.Annotations["kuben.dev/token-expires-at"] != "42" {
		t.Fatal(s.Annotations)
	}
	got, ok := agentlink.LivePublished(s)
	if !ok || got != withToken {
		t.Fatalf("%+v", got)
	}
	enrolled := agentlink.Published{Hub: "h:1", CA: "c", Cluster: "0190"}
	s = secret(t, enrolled)
	if _, has := s.Data["token"]; has {
		t.Fatal("no token once enrolled")
	}
	got, ok = agentlink.LivePublished(s)
	if !ok || got != enrolled {
		t.Fatalf("%+v", got)
	}
}
