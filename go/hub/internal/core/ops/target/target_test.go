package target_test

import (
	"encoding/json"
	"errors"
	"math"
	"testing"
	"testing/quick"

	"github.com/google/go-cmp/cmp"
	"github.com/google/uuid"

	"github.com/Teamtem-dev/kuben/go/hub/internal/core/kerr"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/ops/target"
)

func uid(n byte) uuid.UUID { return uuid.UUID{15: n} }

// differs compares rejections by value; they are plain comparable structs.
func differs(got, want target.Reject) bool { return !cmp.Equal(got, want) }

func newTarget() target.State { return target.New(uid(7)) }

func autodeploy(t target.State, epoch target.SourceEpoch) target.AutodeployRequest {
	return target.AutodeployRequest{
		LifecycleUID:        t.LifecycleUID,
		SourceEpoch:         epoch,
		BuildConfigRevision: t.BuildConfigRevision,
		ExpectedGeneration:  t.DesiredGeneration,
	}
}

func TestANewTargetIsAutomaticAtGenerationZero(t *testing.T) {
	want := target.State{LifecycleUID: uid(7), Policy: target.Auto}
	if diff := cmp.Diff(want, newTarget()); diff != "" {
		t.Fatal(diff)
	}
}

func TestALateBuildOfAnOlderHeadCannotMoveTheTarget(t *testing.T) {
	tg := newTarget()
	a := tg.ObserveSourceHead() // epoch 1 → build A
	b := tg.ObserveSourceHead() // epoch 2 → build B
	genB, rej := tg.TryAutodeploy(autodeploy(tg, b))
	if rej != nil || genB != 1 {
		t.Fatalf("B deploys: got %d, %v", genB, rej)
	}
	_, lateA := tg.TryAutodeploy(autodeploy(tg, a))
	if differs(lateA, target.StaleSource{Current: 2, Requested: 1}) {
		t.Errorf("got %v", lateA)
	}
	if tg.DesiredGeneration != genB {
		t.Errorf("the target moved to %d", tg.DesiredGeneration)
	}
}

func TestAStaleExpectedGenerationIsRejected(t *testing.T) {
	tg := newTarget()
	head := tg.ObserveSourceHead()
	stale := autodeploy(tg, head)
	if _, rej := tg.DeployExplicit(tg.LifecycleUID, tg.DesiredGeneration); rej != nil {
		t.Fatalf("manual deploy: %v", rej)
	}
	_, rej := tg.TryAutodeploy(stale)
	if differs(rej, target.GenerationMoved{Current: 1, Expected: 0}) {
		t.Errorf("got %v", rej)
	}
}

func TestRollbackPinsUntilResumed(t *testing.T) {
	tg := newTarget()
	head := tg.ObserveSourceHead()
	if _, rej := tg.Rollback(tg.LifecycleUID, tg.DesiredGeneration); rej != nil {
		t.Fatalf("rollback: %v", rej)
	}
	if _, rej := tg.TryAutodeploy(autodeploy(tg, head)); differs(rej, target.NotAutomatic{Policy: target.Pinned}) {
		t.Errorf("got %v", rej)
	}
	// An explicit deploy works under any policy and does not unpin.
	if _, rej := tg.DeployExplicit(tg.LifecycleUID, tg.DesiredGeneration); rej != nil || tg.Policy != target.Pinned {
		t.Errorf("explicit deploy: %v, policy %s", rej, tg.Policy)
	}
	if rej := tg.ResumeAuto(uid(8)); differs(rej, target.LifecycleMismatch{}) || tg.Policy != target.Pinned {
		t.Errorf("another target resumed this one: %v", rej)
	}
	if rej := tg.ResumeAuto(tg.LifecycleUID); rej != nil {
		t.Fatalf("resume: %v", rej)
	}
	if _, rej := tg.TryAutodeploy(autodeploy(tg, head)); rej != nil {
		t.Errorf("got %v", rej)
	}
}

func TestAManualOrUnsetPolicyDoesNotAutodeploy(t *testing.T) {
	for _, policy := range []target.DeployPolicy{target.Manual, ""} {
		tg := newTarget()
		tg.Policy = policy
		head := tg.ObserveSourceHead()
		if _, rej := tg.TryAutodeploy(autodeploy(tg, head)); differs(rej, target.NotAutomatic{Policy: policy}) {
			t.Errorf("%q: got %v", policy, rej)
		}
	}
}

func TestARecreatedTargetIgnoresWorkForTheOldOne(t *testing.T) {
	old := newTarget()
	head := old.ObserveSourceHead()
	request := autodeploy(old, head)
	recreated := target.New(uid(8))
	recreated.ObserveSourceHead()
	if _, rej := recreated.TryAutodeploy(request); differs(rej, target.LifecycleMismatch{}) {
		t.Errorf("got %v", rej)
	}
}

func TestADeletingTargetAcceptsNothing(t *testing.T) {
	tg := newTarget()
	head := tg.ObserveSourceHead()
	tg.BeginDeletion()
	if _, rej := tg.TryAutodeploy(autodeploy(tg, head)); differs(rej, target.Deleting{}) {
		t.Errorf("autodeploy: %v", rej)
	}
	if _, rej := tg.Rollback(tg.LifecycleUID, tg.DesiredGeneration); differs(rej, target.Deleting{}) {
		t.Errorf("rollback: %v", rej)
	}
	if _, rej := tg.DeployExplicit(tg.LifecycleUID, tg.DesiredGeneration); differs(rej, target.Deleting{}) {
		t.Errorf("explicit: %v", rej)
	}
}

func TestABuildOfTheOldConfigurationDoesNotAutodeploy(t *testing.T) {
	tg := newTarget()
	head := tg.ObserveSourceHead()
	request := autodeploy(tg, head)
	tg.ChangeBuildConfig()
	if _, rej := tg.TryAutodeploy(request); differs(rej, target.BuildConfigChanged{}) {
		t.Errorf("got %v", rej)
	}
}

func TestCountersSaturateAndTheGenerationIsExhausted(t *testing.T) {
	tg := newTarget()
	tg.SourceEpoch, tg.BuildConfigRevision, tg.DesiredGeneration = math.MaxUint64, math.MaxUint64, math.MaxUint64
	tg.ChangeBuildConfig()
	if got := tg.ObserveSourceHead(); got != math.MaxUint64 || tg.BuildConfigRevision != math.MaxUint64 {
		t.Errorf("counters wrapped: %d, %d", got, tg.BuildConfigRevision)
	}
	if _, rej := tg.DeployExplicit(tg.LifecycleUID, tg.DesiredGeneration); differs(rej, target.Exhausted{}) {
		t.Errorf("got %v", rej)
	}
	if _, rej := tg.Rollback(tg.LifecycleUID, tg.DesiredGeneration); differs(rej, target.Exhausted{}) || tg.Policy != target.Auto {
		t.Errorf("a refused rollback pinned the target: %v, %s", rej, tg.Policy)
	}
}

func TestRejectsSayWhy(t *testing.T) {
	for _, tc := range []struct {
		rej  target.Reject
		want string
	}{
		{target.LifecycleMismatch{}, "the target was deleted and recreated (lifecycle mismatch)"},
		{target.Deleting{}, "the target is being deleted"},
		{target.StaleSource{Current: 42, Requested: 41}, "a newer source head exists (epoch 42, request 41)"},
		{target.BuildConfigChanged{}, "the build configuration changed since the build started"},
		{target.GenerationMoved{Current: 90, Expected: 89}, "the target moved on (generation 90, expected 89)"},
		{target.NotAutomatic{Policy: target.Pinned}, "automatic deploys are off for this target (Pinned)"},
		{target.NotAutomatic{Policy: target.Manual}, "automatic deploys are off for this target (Manual)"},
		{target.Exhausted{}, "the generation counter is exhausted"},
	} {
		if got := tc.rej.Error(); got != tc.want {
			t.Errorf("got %s", got)
		}
	}
	var err error = target.StaleSource{Current: 2, Requested: 1}
	var rej target.Reject
	if !errors.As(err, &rej) || differs(rej, target.StaleSource{Current: 2, Requested: 1}) {
		t.Errorf("got %v", rej)
	}
}

func TestStateKeepsItsJSON(t *testing.T) {
	tg := newTarget()
	tg.ObserveSourceHead()
	if _, rej := tg.Rollback(tg.LifecycleUID, 0); rej != nil {
		t.Fatal(rej)
	}
	raw, err := json.Marshal(tg)
	want := `{"lifecycleUid":"00000000-0000-0000-0000-000000000007","deleting":false,"desiredGeneration":1,` +
		`"sourceEpoch":1,"buildConfigRevision":0,"policy":"pinned"}`
	if err != nil || string(raw) != want {
		t.Fatalf("got %s, %v", raw, err)
	}
	var back target.State
	if err = json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(tg, back); diff != "" {
		t.Error(diff)
	}
	big, err := json.Marshal(struct {
		G target.Generation  `json:"g"`
		E target.SourceEpoch `json:"e"`
	}{math.MaxUint64, 3})
	if err != nil || string(big) != `{"g":18446744073709551615,"e":3}` {
		t.Errorf("got %s, %v", big, err)
	}
}

func TestPoliciesParseAndPinTheirStrings(t *testing.T) {
	for policy, s := range map[target.DeployPolicy]string{
		target.Auto: "auto", target.Manual: "manual", target.Pinned: "pinned",
	} {
		raw, err := json.Marshal(policy)
		if err != nil || string(raw) != `"`+s+`"` || policy.String() != s {
			t.Errorf("%s: %s, %v", s, raw, err)
		}
		if got, perr := target.ParseDeployPolicy(s); perr != nil || got != policy {
			t.Errorf("%s: got %s, %v", s, got, perr)
		}
	}
	if _, err := target.ParseDeployPolicy("Auto"); !errors.Is(err, kerr.ErrValidation) {
		t.Errorf("got %v", err)
	}
	var state target.State
	if err := json.Unmarshal([]byte(`{"policy":"frozen"}`), &state); err == nil {
		t.Error("frozen is not a policy")
	}
}

// step is one operation of the property below, drawn from a seed with the
// weights the Rust strategy had: 3 observe, 1 config, 5 builds (lagging 0 to
// 2 heads), 2 explicit, 1 rollback, 1 resume, 1 delete.
type step struct {
	kind  string
	lag   uint64
	stale bool
}

func stepOf(seed uint16) step {
	extra := uint64(seed >> 8)
	switch n := seed % 14; {
	case n < 3:
		return step{kind: "observe"}
	case n < 4:
		return step{kind: "config"}
	case n < 9:
		return step{kind: "build", lag: extra % 3}
	case n < 11:
		return step{kind: "explicit", stale: extra%2 == 1}
	case n < 12:
		return step{kind: "rollback", stale: extra%2 == 1}
	case n < 13:
		return step{kind: "resume"}
	default:
		return step{kind: "delete"}
	}
}

// run performs one step and reports the rejection and, for a finished build,
// the epoch it carried (lag saturates at the first head).
func run(tg *target.State, s step) (rej target.Reject, buildEpoch target.SourceEpoch, isBuild bool) {
	expected := tg.DesiredGeneration
	if s.stale {
		expected-- // wraps at zero, as the Rust test did
	}
	switch s.kind {
	case "observe":
		tg.ObserveSourceHead()
	case "config":
		tg.ChangeBuildConfig()
	case "build":
		epoch := tg.SourceEpoch - target.SourceEpoch(min(s.lag, uint64(tg.SourceEpoch)))
		_, rej = tg.TryAutodeploy(autodeploy(*tg, epoch))
		return rej, epoch, true
	case "explicit":
		_, rej = tg.DeployExplicit(tg.LifecycleUID, expected)
	case "rollback":
		_, rej = tg.Rollback(tg.LifecycleUID, expected)
	case "resume":
		rej = tg.ResumeAuto(tg.LifecycleUID)
	case "delete":
		tg.BeginDeletion()
	}
	return rej, 0, false
}

// moved says what is wrong with how the generation changed, if anything.
func moved(before, after target.State, rej target.Reject) string {
	switch {
	case after.DesiredGeneration < before.DesiredGeneration:
		return "generation decreased"
	case after.DesiredGeneration == before.DesiredGeneration:
		return ""
	case rej != nil:
		return "a rejected change moved the target"
	case after.DesiredGeneration != before.DesiredGeneration+1:
		return "more than one step per change"
	case before.Deleting:
		return "a deleting target moved"
	}
	return ""
}

// deployed says what is wrong with an accepted automatic deploy, if anything.
func deployed(before, after target.State, epoch target.SourceEpoch) string {
	switch {
	case epoch != before.SourceEpoch:
		return "a stale build deployed"
	case before.Policy != target.Auto:
		return "a pinned or manual target auto-deployed"
	case after.DesiredGeneration == before.DesiredGeneration:
		return "an accepted build did not move the target"
	}
	return ""
}

// Generations never decrease; an automatic deploy is accepted only for the
// current head, configuration and expected generation of a live, unpinned
// target.
func TestOnlyCurrentWorkMovesTheTarget(t *testing.T) {
	property := func(seeds []uint16) bool {
		tg := newTarget()
		for _, seed := range seeds {
			before := tg
			rej, epoch, isBuild := run(&tg, stepOf(seed))
			wrong := moved(before, tg, rej)
			if wrong == "" && isBuild && rej == nil {
				wrong = deployed(before, tg, epoch)
			}
			if wrong != "" {
				t.Logf("%s: %+v → %+v", wrong, before, tg)
				return false
			}
		}
		return true
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 2000}); err != nil {
		t.Fatal(err)
	}
}
