package policy_test

import (
	"encoding/json"
	"errors"
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"testing/quick"

	"github.com/google/go-cmp/cmp"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/kerrors"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/perm"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/policy"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/scan"
)

var hash = []byte{1, 2, 3}

func pending(required, approved uint8) policy.Pending {
	return policy.Pending{
		RequestedBy: "user:alice",
		Required:    required,
		Approved:    approved,
		ExpiresAt:   1_000,
		PlanHash:    hash,
	}
}

func TestProductionNeedsOneAdminApproval(t *testing.T) {
	p := policy.Initial(true)
	if p.RequiredApprovals != 1 || p.ApproveRole != perm.Admin {
		t.Errorf("got %+v", p)
	}
	if got := policy.Initial(false).RequiredApprovals; got != 0 {
		t.Errorf("got %d approvals", got)
	}
	if err := p.Validate(); err != nil {
		t.Error(err)
	}
	if err := policy.Open().Validate(); err != nil {
		t.Error(err)
	}
	if policy.Initial(false) != policy.Open() || policy.Initial(true) != policy.Production() {
		t.Error("the initial policy is the open or the production one")
	}
}

func TestInvalidPoliciesAreRefused(t *testing.T) {
	tests := []struct {
		name    string
		change  func(*policy.EnvironmentPolicy)
		want    policy.Error
		message string
	}{
		{
			"too many approvals", func(p *policy.EnvironmentPolicy) { p.RequiredApprovals = 6 },
			policy.Error{Kind: policy.KindTooManyApprovals},
			"at most 5 approvals can be required",
		},
		{
			"viewers cannot deploy", func(p *policy.EnvironmentPolicy) { p.DeployRole = perm.Viewer },
			policy.Error{Kind: policy.KindDeployRole, Role: perm.Viewer},
			"the deploy role `viewer` cannot deploy",
		},
		{
			"developers cannot approve", func(p *policy.EnvironmentPolicy) { p.ApproveRole = perm.Developer },
			policy.Error{Kind: policy.KindApproveRole, Role: perm.Developer},
			"the approve role `developer` cannot approve releases",
		},
		{
			"too short a wait", func(p *policy.EnvironmentPolicy) { p.ApprovalTTLSecs = 10 },
			policy.Error{Kind: policy.KindApprovalTTL},
			"approvals must wait between 300 and 2592000 seconds",
		},
		{
			"too long a wait", func(p *policy.EnvironmentPolicy) { p.ApprovalTTLSecs = policy.MaxApprovalTTLSecs + 1 },
			policy.Error{Kind: policy.KindApprovalTTL},
			"approvals must wait between 300 and 2592000 seconds",
		},
		{
			"a gate on low findings", func(p *policy.EnvironmentPolicy) { p.Scan.Severity = scan.SeverityLow },
			policy.Error{Kind: policy.KindScanGate},
			"the scan gate counts `high` or `critical` findings, with a maximum age of an hour to 90 days",
		},
		{
			"an unknown role", func(p *policy.EnvironmentPolicy) { p.DeployRole = "root" },
			policy.Error{Kind: policy.KindDeployRole, Role: "root"},
			"the deploy role `root` cannot deploy",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := policy.Production()
			tt.change(&p)
			err := p.Validate()
			var got policy.Error
			if !errors.As(err, &got) || got != tt.want {
				t.Fatalf("got %v, want %v", err, tt.want)
			}
			if got.Error() != tt.message {
				t.Errorf("got %q, want %q", got.Error(), tt.message)
			}
			if !errors.Is(err, kerrors.ErrValidation) || errors.Is(err, kerrors.ErrConflict) {
				t.Error("a refused policy is a validation failure")
			}
		})
	}
	if err := (policy.EnvironmentPolicy{}).Validate(); err == nil {
		t.Error("the zero policy is not valid")
	}
	for _, ttl := range []uint32{policy.MinApprovalTTLSecs, policy.MaxApprovalTTLSecs} {
		p := policy.Production()
		p.ApprovalTTLSecs = ttl
		if err := p.Validate(); err != nil {
			t.Errorf("a wait of %d seconds: %v", ttl, err)
		}
	}
}

func TestRestartsAndHandoversNeedNoApproval(t *testing.T) {
	p := policy.Production()
	p.RequiredApprovals = 2
	tests := []struct {
		kind policy.ChangeKind
		want uint8
	}{
		{policy.Deploy, 2},
		{policy.Rollback, 2},
		{policy.Promotion, 2},
		{policy.Build, 2},
		{policy.Rotation, 2},
		{policy.Restart, 0},
		{policy.Handover, 0},
		{policy.Emergency, 0},
		{policy.ChangeKind("bogus"), 2},
	}
	for _, tt := range tests {
		if got := p.ApprovalsFor(tt.kind); got != tt.want {
			t.Errorf("%s: got %d, want %d", tt.kind, got, tt.want)
		}
	}
	if got := policy.Open().ApprovalsFor(policy.Deploy); got != 0 {
		t.Errorf("got %d", got)
	}
}

func TestRolesAreCheckedAgainstThePolicy(t *testing.T) {
	p := policy.Production()
	p.DeployRole = perm.Admin
	tests := []struct {
		name string
		got  bool
		want bool
	}{
		{"nobody deploys", p.MayDeploy(opt.None[perm.Role]()), false},
		{"developer deploys", p.MayDeploy(opt.Some(perm.Developer)), false},
		{"admin deploys", p.MayDeploy(opt.Some(perm.Admin)), true},
		{"owner approves", p.MayApprove(opt.Some(perm.Owner)), true},
		{"developer approves", p.MayApprove(opt.Some(perm.Developer)), false},
		{"viewer approves", p.MayApprove(opt.Some(perm.Viewer)), false},
		{"nobody approves", p.MayApprove(opt.None[perm.Role]()), false},
		{"unknown role deploys", p.MayDeploy(opt.Some(perm.Role("root"))), false},
	}
	for _, tt := range tests {
		if tt.got != tt.want {
			t.Errorf("%s: got %v", tt.name, tt.got)
		}
	}
}

func TestWeakeningIsDetectedFieldByField(t *testing.T) {
	p := policy.Production()
	with := func(change func(*policy.EnvironmentPolicy)) policy.EnvironmentPolicy {
		c := p
		change(&c)
		return c
	}
	ownerApproves := with(func(c *policy.EnvironmentPolicy) { c.ApproveRole = perm.Owner })
	tests := []struct {
		name    string
		next    policy.EnvironmentPolicy
		current policy.EnvironmentPolicy
		want    bool
	}{
		{"itself", p, p, false},
		{"open after production", policy.Open(), p, true},
		{"more approvals", with(func(c *policy.EnvironmentPolicy) { c.RequiredApprovals = 2 }), p, false},
		{"a weaker approve role", with(func(c *policy.EnvironmentPolicy) { c.ApproveRole = perm.Admin }), ownerApproves, true},
		{"a longer wait", with(func(c *policy.EnvironmentPolicy) { c.ApprovalTTLSecs++ }), p, true},
		{"a stronger deploy role", with(func(c *policy.EnvironmentPolicy) { c.DeployRole = perm.Owner }), p, false},
		{"a weaker deploy role", p, with(func(c *policy.EnvironmentPolicy) { c.DeployRole = perm.Owner }), true},
		{"a weaker gate", with(func(c *policy.EnvironmentPolicy) { c.Scan.Mode = scan.ModeWarn }), p, true},
	}
	for _, tt := range tests {
		if got := tt.next.Weakens(tt.current); got != tt.want {
			t.Errorf("%s: got %v", tt.name, got)
		}
	}
}

func TestRequestersNeverApproveTheirOwnChange(t *testing.T) {
	byToken := pending(1, 0)
	byToken.RequestedBy = "token:alice"
	tests := []struct {
		name     string
		run      policy.Pending
		decision policy.Decision
	}{
		{"approve", pending(1, 0), policy.Approve},
		{"nor reject it: withdrawing is cancellation", pending(1, 0), policy.Reject},
		{"a token acts for its owner", byToken, policy.Approve},
	}
	for _, tt := range tests {
		tally, err := policy.Decide(tt.run, "alice", false, tt.decision, hash, 0)
		if !errors.Is(err, policy.ErrSelfApproval) || tally != nil {
			t.Errorf("%s: got %v, %v", tt.name, tally, err)
		}
	}
}

func TestDecisionsNeedTheShownPlanAndAnOpenWindow(t *testing.T) {
	run := pending(1, 0)
	tests := []struct {
		name          string
		decidedBefore bool
		planHash      []byte
		now           int64
		want          policy.Tally
		wantErr       error
	}{
		{"another plan", false, []byte{9}, 0, nil, policy.ErrStalePlan},
		{"the window closed", false, hash, 1_000, nil, policy.ErrExpired},
		{"decided before", true, hash, 0, nil, policy.ErrAlreadyDecided},
		{"in time", false, hash, 999, policy.Approved{}, nil},
		{"expiry is checked first", true, []byte{9}, 1_000, nil, policy.ErrExpired},
	}
	for _, tt := range tests {
		got, err := policy.Decide(run, "bob", tt.decidedBefore, policy.Approve, tt.planHash, tt.now)
		if got != tt.want || !errors.Is(err, tt.wantErr) {
			t.Errorf("%s: got %v, %v", tt.name, got, err)
		}
	}
	if _, err := policy.Decide(run, "bob", false, policy.Decision("maybe"), hash, 0); !errors.Is(err, kerrors.ErrValidation) {
		t.Errorf("an unknown decision: got %v", err)
	}
}

func TestApprovalsAreCountedAndOneRejectionCancels(t *testing.T) {
	tests := []struct {
		name     string
		run      policy.Pending
		decision policy.Decision
		want     policy.Tally
	}{
		{"second of three", pending(3, 1), policy.Approve, policy.Waiting{Remaining: 1}},
		{"third of three", pending(3, 2), policy.Approve, policy.Approved{}},
		{"a rejection", pending(3, 2), policy.Reject, policy.Rejected{}},
		{"the count saturates", pending(255, 255), policy.Approve, policy.Approved{}},
		{"none required", pending(0, 0), policy.Approve, policy.Approved{}},
	}
	for _, tt := range tests {
		got, err := policy.Decide(tt.run, "carol", false, tt.decision, hash, 0)
		if err != nil || got != tt.want {
			t.Errorf("%s: got %v, %v", tt.name, got, err)
		}
	}
}

func TestPlanHashesRoundTripAsHex(t *testing.T) {
	if got := policy.Hex([]byte{0, 171, 255}); got != "00abff" {
		t.Errorf("got %q", got)
	}
	tests := []struct {
		text string
		want []byte
		ok   bool
	}{
		{"00abff", []byte{0, 171, 255}, true},
		{"00ABFF", []byte{0, 171, 255}, true},
		{"", []byte{}, true},
		{strings.Repeat("a", 128), []byte(strings.Repeat("\xaa", 64)), true},
		{"abc", nil, false},
		{"zz", nil, false},
		{strings.Repeat("a", 130), nil, false},
		{"éé", nil, false},
	}
	for _, tt := range tests {
		got, ok := policy.Unhex(tt.text)
		if ok != tt.ok || !cmp.Equal(got, tt.want) {
			t.Errorf("%q: got %v, %v", tt.text, got, ok)
		}
	}
	if got := policy.Principal("user:u1"); got != "u1" {
		t.Errorf("got %q", got)
	}
	if got := policy.Principal("plain"); got != "plain" {
		t.Errorf("got %q", got)
	}
	if got := policy.Principal("token:a:b"); got != "a:b" {
		t.Errorf("got %q", got)
	}
}

// votes is a generated sequence of (who, approve) votes on one run.
type votes struct {
	required uint8
	cast     []vote
}

type vote struct {
	who     int
	approve bool
}

// Generate draws 1..=MaxApprovals required approvals and up to 19 votes of
// six people, as the Rust property did.
func (votes) Generate(r *rand.Rand, _ int) reflect.Value {
	v := votes{required: uint8(1 + r.Intn(int(policy.MaxApprovals)))}
	for range r.Intn(20) {
		v.cast = append(v.cast, vote{who: r.Intn(6), approve: r.Intn(2) == 0})
	}
	return reflect.ValueOf(v)
}

// However many approvers vote, a run is approved exactly when the required
// number of distinct non-requesters approved and nobody rejected first.
func TestARunIsApprovedOnlyByEnoughOtherPeople(t *testing.T) {
	people := []string{"alice", "bob", "carol", "dave", "erin", "frank"}
	property := func(v votes) bool {
		decided := map[int]bool{}
		var approved uint8
		var outcome policy.Tally
		for _, cast := range v.cast {
			if outcome != nil {
				break
			}
			run := pending(v.required, approved)
			decision := policy.Reject
			if cast.approve {
				decision = policy.Approve
			}
			tally, err := policy.Decide(run, people[cast.who], decided[cast.who], decision, hash, 0)
			if err != nil {
				if !errors.Is(err, policy.ErrSelfApproval) && !errors.Is(err, policy.ErrAlreadyDecided) {
					t.Logf("unexpected refusal %v", err)
					return false
				}
				continue
			}
			decided[cast.who] = true
			switch done := tally.(type) {
			case policy.Waiting:
				approved++
				if done.Remaining != v.required-approved {
					t.Logf("remaining %d after %d of %d", done.Remaining, approved, v.required)
					return false
				}
			case policy.Approved:
				approved++
				outcome = done
			case policy.Rejected:
				outcome = done
			}
		}
		if decided[0] {
			t.Log("alice asked for the change")
			return false
		}
		if outcome == (policy.Approved{}) && approved != v.required {
			return false
		}
		return approved <= v.required
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 2000}); err != nil {
		t.Error(err)
	}
}

func TestWireFormat(t *testing.T) {
	tests := []struct {
		name  string
		value any
		want  string
	}{
		{
			"open policy", policy.Open(),
			`{"requiredApprovals":0,"deployRole":"developer","approveRole":"admin","approvalTtlSecs":604800,` +
				`"scan":{"mode":"off","severity":"critical","requireScan":false,"maxAgeSecs":604800}}`,
		},
		{
			"production policy", policy.Production(),
			`{"requiredApprovals":1,"deployRole":"developer","approveRole":"admin","approvalTtlSecs":604800,` +
				`"scan":{"mode":"block","severity":"critical","requireScan":false,"maxAgeSecs":604800}}`,
		},
		{"approve", policy.Approve, `"approve"`},
		{"reject", policy.Reject, `"reject"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := json.Marshal(tt.value)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tt.want {
				t.Errorf("got  %s\nwant %s", got, tt.want)
			}
			back := reflect.New(reflect.TypeOf(tt.value))
			if err := json.Unmarshal(got, back.Interface()); err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff(tt.value, back.Elem().Interface()); diff != "" {
				t.Errorf("round trip (-want +got):\n%s", diff)
			}
		})
	}
	if policy.Approve.Stored() != "approved" || policy.Reject.Stored() != "rejected" || policy.Decision("x").Stored() != "" {
		t.Error("the stored names are approved and rejected")
	}
}

func TestPoliciesStoredWithoutAGateHaveItOff(t *testing.T) {
	var p policy.EnvironmentPolicy
	input := `{"requiredApprovals":2,"deployRole":"admin","approveRole":"owner","approvalTtlSecs":600}`
	if err := json.Unmarshal([]byte(input), &p); err != nil {
		t.Fatal(err)
	}
	want := policy.EnvironmentPolicy{
		RequiredApprovals: 2, DeployRole: perm.Admin, ApproveRole: perm.Owner, ApprovalTTLSecs: 600, Scan: scan.Off(),
	}
	if p != want {
		t.Errorf("got %+v, want %+v", p, want)
	}
}

func TestDecodingRefusesWhatSerdeRefuses(t *testing.T) {
	tests := []struct {
		name   string
		target any
		input  string
	}{
		{"no approvals field", new(policy.EnvironmentPolicy), `{"deployRole":"admin","approveRole":"owner","approvalTtlSecs":600}`},
		{"no deploy role", new(policy.EnvironmentPolicy), `{"requiredApprovals":1,"approveRole":"owner","approvalTtlSecs":600}`},
		{"no approve role", new(policy.EnvironmentPolicy), `{"requiredApprovals":1,"deployRole":"admin","approvalTtlSecs":600}`},
		{"no wait", new(policy.EnvironmentPolicy), `{"requiredApprovals":1,"deployRole":"admin","approveRole":"owner"}`},
		{"an unknown role", new(policy.EnvironmentPolicy), `{"requiredApprovals":1,"deployRole":"root","approveRole":"owner","approvalTtlSecs":600}`},
		{"approvals beyond a byte", new(policy.EnvironmentPolicy), `{"requiredApprovals":256,"deployRole":"admin","approveRole":"owner","approvalTtlSecs":600}`},
		{"a partial gate", new(policy.EnvironmentPolicy), `{"requiredApprovals":1,"deployRole":"admin","approveRole":"owner","approvalTtlSecs":600,"scan":{"mode":"off"}}`},
		{"the stored decision name", new(policy.Decision), `"approved"`},
		{"an upper-case decision", new(policy.Decision), `"Approve"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := json.Unmarshal([]byte(tt.input), tt.target); err == nil {
				t.Errorf("decoded %s into %+v", tt.input, tt.target)
			}
		})
	}
}

func TestParsing(t *testing.T) {
	kinds := []policy.ChangeKind{
		policy.Deploy, policy.Rollback, policy.Promotion, policy.Build,
		policy.Restart, policy.Handover, policy.Rotation, policy.Emergency,
	}
	for _, k := range kinds {
		if got, err := policy.ParseChangeKind(string(k)); err != nil || got != k {
			t.Errorf("%s: got %q, %v", k, got, err)
		}
	}
	for _, d := range []policy.Decision{policy.Approve, policy.Reject} {
		if got, err := policy.ParseDecision(string(d)); err != nil || got != d {
			t.Errorf("%s: got %q, %v", d, got, err)
		}
	}
	_, errKind := policy.ParseChangeKind("redeploy")
	_, errDecision := policy.ParseDecision("approved")
	for _, err := range []error{errKind, errDecision} {
		if !errors.Is(err, kerrors.ErrValidation) {
			t.Errorf("got %v, want a validation error", err)
		}
	}
}

func TestPreviewPoliciesKeepTheGateButNotTheApprovals(t *testing.T) {
	production := policy.Production()
	preview := policy.ForPreview(production)
	if preview.RequiredApprovals != 0 {
		t.Errorf("got %d approvals", preview.RequiredApprovals)
	}
	if preview.Scan != scan.Production() || preview.DeployRole != production.DeployRole {
		t.Errorf("got %+v", preview)
	}
	if err := preview.Validate(); err != nil {
		t.Error(err)
	}
	if production.RequiredApprovals != 1 {
		t.Error("the source policy is not changed")
	}
}
