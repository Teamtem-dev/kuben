package build_test

// Failure injection for builds (M3 exit criteria; scenarios.rs): the
// worker's decisions (outcome.Classify, build.NextFor, build.PlanFor)
// driven through scripted cluster and registry behaviour, without a
// cluster.
//
// sim acts like the worker: it creates the Job on admission, deletes it on
// a stop (later reads see it gone), verifies reported digests against a
// fake registry, and queues a new attempt after a lost worker, up to
// store.MaxBuildAttempts. Every event it records must be legal for the
// phase it is in, as the database would enforce.

import (
	"math/rand"
	"reflect"
	"slices"
	"testing"
	"testing/quick"

	"github.com/Teamtem-dev/kuben/internal/build"
	"github.com/Teamtem-dev/kuben/internal/core/artifact"
	opbuild "github.com/Teamtem-dev/kuben/internal/core/ops/build"
	"github.com/Teamtem-dev/kuben/internal/core/ops/outcome"
	"github.com/Teamtem-dev/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/internal/store"
)

const (
	good   = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	forged = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
)

// step is one scripted thing that happens between two worker claims.
//
//sumtype:decl
type step interface{ step() }

type (
	// cluster: the Job shows this (while it exists).
	cluster struct{ seen outcome.JobObservation }
	// cancel: someone asks the build to stop.
	cancel struct{}
	// registryUp: the registry becomes (un)reachable.
	registryUp struct{ up bool }
)

func (cluster) step()    {}
func (cancel) step()     {}
func (registryUp) step() {}

type sim struct {
	t          *testing.T
	phase      opbuild.Phase
	cancel     bool
	attempt    uint32
	jobCreated bool
	jobDeleted bool
	reported   opt.Val[artifact.Digest]
	verified   opt.Val[artifact.Digest]
	failure    opt.Val[outcome.BuildFailure]
	registry   map[string]bool
	registryUp bool
	// trail is every phase the attempts went through, for assertions.
	trail []opbuild.Phase
	// illegal is the first illegal event, which fails the property.
	illegal error
}

func newSim(t *testing.T) *sim {
	return &sim{
		t: t, phase: opbuild.Queued, attempt: 1, registry: map[string]bool{good: true}, registryUp: true,
		trail: []opbuild.Phase{opbuild.Queued},
	}
}

func (s *sim) apply(events []opbuild.Event) {
	for _, e := range events {
		next, err := s.phase.Apply(e)
		if err != nil {
			if s.illegal == nil {
				s.illegal = err
			}
			return
		}
		s.phase = next
		s.trail = append(s.trail, next)
	}
}

// claim is one claim of the build operation; true when it settled for good.
func (s *sim) claim(seen outcome.JobObservation) bool {
	switch build.NextFor(s.phase, s.cancel) {
	case build.NextSettle:
		return true
	case build.NextCancelQueued:
		s.apply([]opbuild.Event{opbuild.EventCancelRequested})
		return true
	case build.NextAdmit:
		s.jobCreated = true
		s.apply([]opbuild.Event{opbuild.EventStarted})
		return false
	case build.NextObserve:
		if s.jobDeleted || !s.jobCreated {
			seen = outcome.JobObservation{}
		}
		return s.carry(build.PlanFor(s.phase, s.cancel, outcome.Classify(seen), s.reported))
	}
	return true
}

func (s *sim) carry(plan build.Plan) bool {
	switch p := plan.(type) {
	case build.PlanWait:
		s.apply(p.Events)
		return false
	case build.PlanStop:
		s.jobDeleted = true
		s.apply(p.Events)
		return false
	case build.PlanCancelled:
		s.apply(p.Events)
		return true
	case build.PlanVerify:
		s.apply(p.Events)
		if s.reported.IsNone() {
			s.reported = opt.Some(p.Digest)
		}
		if !s.registryUp {
			return false
		}
		if s.registry[p.Digest.String()] {
			s.apply([]opbuild.Event{opbuild.EventVerified})
			s.verified = opt.Some(p.Digest)
			return true
		}
		return s.fail(nil, outcome.OutputRejected)
	case build.PlanFail:
		return s.fail(p.Events, p.Failure)
	}
	return true
}

func (s *sim) fail(events []opbuild.Event, failure outcome.BuildFailure) bool {
	s.apply(events)
	s.apply([]opbuild.Event{opbuild.EventFailed})
	s.failure = opt.Some(failure)
	if failure.Retryable() && s.attempt < store.MaxBuildAttempts {
		// The worker queues the next attempt with the same inputs.
		next := newSim(s.t)
		next.attempt, next.registry, next.registryUp = s.attempt+1, s.registry, s.registryUp
		next.trail = append(s.trail, opbuild.Queued) //nolint:gocritic // the trail moves to the next attempt
		next.cancel, next.illegal = s.cancel, s.illegal
		*s = *next
		return false
	}
	return true
}

// run runs script; the last cluster state repeats until the build settles
// or 200 claims pass.
func (s *sim) run(script ...step) *sim {
	seen := pending()
	for i := range 200 {
		if i < len(script) {
			switch st := script[i].(type) {
			case cluster:
				seen = st.seen
			case cancel:
				s.cancel = true
			case registryUp:
				s.registryUp = st.up
			}
		}
		if s.claim(seen) {
			break
		}
	}
	if s.illegal != nil {
		s.t.Errorf("attempt %d: %v", s.attempt, s.illegal)
	}
	return s
}

func job(pod outcome.PodObservation) outcome.JobObservation {
	return outcome.JobObservation{Exists: true, Pod: opt.Some(pod)}
}

func exit(name string, code int32, reason string, message opt.Val[string]) outcome.ContainerExit {
	return outcome.ContainerExit{Name: name, ExitCode: code, Reason: opt.Some(reason), Message: message}
}

func pending() outcome.JobObservation { return job(outcome.PodObservation{Phase: "Pending"}) }

func fetching() outcome.JobObservation {
	return job(outcome.PodObservation{Phase: "Pending", Running: []string{"fetch"}})
}

func building() outcome.JobObservation {
	return job(outcome.PodObservation{
		Phase: "Running",
		Exits: []outcome.ContainerExit{
			exit("fetch", 0, "Completed", opt.None[string]()), exit("plan", 0, "Completed", opt.None[string]()),
		},
		Running: []string{"build"},
	})
}

func finished(digest string) outcome.JobObservation {
	report := `{"digest":"` + digest + `","strategy":"dockerfile"}`
	j := job(outcome.PodObservation{
		Phase: "Succeeded",
		Exits: []outcome.ContainerExit{
			exit("fetch", 0, "Completed", opt.None[string]()), exit("build", 0, "Completed", opt.Some(report)),
		},
	})
	j.Succeeded = true
	return j
}

func failedPod(pod outcome.PodObservation) outcome.JobObservation {
	j := job(pod)
	j.Failed, j.FailedReason = true, opt.Some("BackoffLimitExceeded")
	return j
}

func oom() outcome.JobObservation {
	return failedPod(outcome.PodObservation{
		Phase: "Failed", Exits: []outcome.ContainerExit{exit("build", 137, "OOMKilled", opt.None[string]())},
	})
}

func deadline() outcome.JobObservation {
	return outcome.JobObservation{Exists: true, Failed: true, FailedReason: opt.Some("DeadlineExceeded")}
}

func diskEviction() outcome.JobObservation {
	return failedPod(outcome.PodObservation{
		Phase: "Failed", Reason: opt.Some("Evicted"),
		Message: opt.Some("Pod ephemeral local storage usage exceeds the total limit of containers 10Gi."),
	})
}

func enospc() outcome.JobObservation {
	return failedPod(outcome.PodObservation{
		Phase: "Failed",
		Exits: []outcome.ContainerExit{exit("build", 1, "Error",
			opt.Some("error: failed to copy: write /home/user/.local/share/buildkit/x: no space left on device"))},
	})
}

func nodeLost() outcome.JobObservation {
	return job(outcome.PodObservation{Phase: "Unknown", Reason: opt.Some("NodeLost")})
}

func unschedulable() outcome.JobObservation {
	return job(outcome.PodObservation{
		Phase: "Pending", UnschedulableSecs: opt.Some(outcome.UnschedulableLimitSecs + 1),
		Message: opt.Some("0/1 nodes are available: 1 Insufficient memory."),
	})
}

func sourceMissing() outcome.JobObservation {
	return failedPod(outcome.PodObservation{
		Phase: "Failed",
		Exits: []outcome.ContainerExit{exit("fetch", 128, "Error", opt.Some("fatal: remote error: upload-pack: not our ref"))},
	})
}

func (s *sim) failed(t *testing.T, want outcome.BuildFailure) {
	t.Helper()
	if s.phase != opbuild.Failed || s.failure != opt.Some(want) {
		t.Errorf("%s %v, want failed %s", s.phase, s.failure, want)
	}
}

func TestABuildGoesFromQueuedToAVerifiedImage(t *testing.T) {
	s := newSim(t).run(cluster{pending()}, cluster{fetching()}, cluster{building()}, cluster{finished(good)})
	if s.phase != opbuild.Succeeded || s.verified.IsNone() || s.verified.Or(artifact.Digest{}).String() != good {
		t.Errorf("%s %v", s.phase, s.verified)
	}
	for _, phase := range []opbuild.Phase{opbuild.Preparing, opbuild.Running, opbuild.Publishing, opbuild.VerifyingOutput} {
		if !slices.Contains(s.trail, phase) {
			t.Errorf("%s not in %v", phase, s.trail)
		}
	}
}

func TestOutOfMemoryFailsTheBuildWithoutARetry(t *testing.T) {
	s := newSim(t).run(cluster{building()}, cluster{oom()})
	s.failed(t, outcome.OutOfMemory)
	if s.attempt != 1 {
		t.Errorf("attempt %d", s.attempt)
	}
}

func TestTheDeadlineFailsTheBuild(t *testing.T) {
	newSim(t).run(cluster{building()}, cluster{deadline()}).failed(t, outcome.DeadlineExceeded)
}

func TestAFullDiskFailsTheBuildByEvictionOrEnospc(t *testing.T) {
	for _, full := range []outcome.JobObservation{diskEviction(), enospc()} {
		newSim(t).run(cluster{building()}, cluster{full}).failed(t, outcome.DiskFull)
	}
}

func TestAnUnreadableCommitFailsAtFetch(t *testing.T) {
	newSim(t).run(cluster{fetching()}, cluster{sourceMissing()}).failed(t, outcome.SourceUnavailable)
}

func TestABuildNoNodeCanTakeFailsAsUnschedulable(t *testing.T) {
	newSim(t).run(cluster{pending()}, cluster{unschedulable()}).failed(t, outcome.Unschedulable)
}

func TestCancellingAQueuedBuildNeedsNoJob(t *testing.T) {
	s := newSim(t).run(cancel{})
	if s.phase != opbuild.Cancelled || s.jobCreated {
		t.Errorf("%s, job created %t: nothing ran", s.phase, s.jobCreated)
	}
}

func TestCancellingARunningBuildDeletesItsJobFirst(t *testing.T) {
	s := newSim(t).run(cluster{building()}, cluster{building()}, cancel{}, cluster{building()})
	if s.phase != opbuild.Cancelled || !s.jobDeleted || !slices.Contains(s.trail, opbuild.Cancelling) {
		t.Errorf("%s, deleted %t, trail %v", s.phase, s.jobDeleted, s.trail)
	}
	if s.failure.IsSome() {
		t.Error("a stop is not a failure")
	}
}

func TestAJobThatDiesWhileBeingStoppedIsCancelled(t *testing.T) {
	// The kill of the stop shows up as an OOM-looking exit before the Job is gone.
	s := newSim(t).run(cluster{building()}, cluster{building()})
	s.cancel = true
	if !s.carry(build.PlanFor(s.phase, true, outcome.Classify(oom()), opt.None[artifact.Digest]())) {
		t.Error("not settled")
	}
	if s.phase != opbuild.Cancelled || s.failure.IsSome() {
		t.Errorf("%s %v", s.phase, s.failure)
	}
}

func TestOutputPushedBeforeTheStopIsKept(t *testing.T) {
	s := newSim(t).run(cluster{building()}, cluster{building()}, cluster{finished(good)}, cancel{})
	// The worker saw the finished Job on the same claim the stop arrived.
	if s.phase != opbuild.Succeeded && s.phase != opbuild.Cancelled {
		t.Errorf("%s", s.phase)
	}
	racing := newSim(t).run(cluster{building()}, cluster{building()})
	racing.cancel = true
	if !racing.carry(build.PlanFor(racing.phase, true, outcome.Classify(finished(good)), opt.None[artifact.Digest]())) {
		t.Error("not settled")
	}
	if racing.phase != opbuild.Succeeded {
		t.Errorf("%s: the artifact exists; the epoch decides the deploy", racing.phase)
	}
}

func TestALostNodeIsRetriedAsANewAttempt(t *testing.T) {
	s := newSim(t).run(cluster{building()}, cluster{nodeLost()}, cluster{pending()}, cluster{building()},
		cluster{finished(good)})
	if s.phase != opbuild.Succeeded || s.attempt != 2 || !slices.Contains(s.trail, opbuild.Failed) {
		t.Errorf("%s, attempt %d, trail %v", s.phase, s.attempt, s.trail)
	}
}

func TestLostWorkersAreRetriedABoundedNumberOfTimes(t *testing.T) {
	s := newSim(t).run(cluster{nodeLost()})
	s.failed(t, outcome.LostWorker)
	if s.attempt != store.MaxBuildAttempts {
		t.Errorf("attempt %d", s.attempt)
	}
}

func TestAJobThatVanishesMidBuildIsALostWorker(t *testing.T) {
	s := newSim(t).run(cluster{building()}, cluster{building()})
	s.jobDeleted = true // someone deleted it by hand
	if s.carry(build.PlanFor(s.phase, false, outcome.Classify(outcome.JobObservation{}), opt.None[artifact.Digest]())) {
		t.Error("no retry was queued")
	}
	if s.phase != opbuild.Queued || s.attempt != 2 {
		t.Errorf("%s, attempt %d", s.phase, s.attempt)
	}
}

func TestADigestTheRegistryDoesNotHoldIsRejected(t *testing.T) {
	s := newSim(t).run(cluster{building()}, cluster{finished(forged)})
	s.failed(t, outcome.OutputRejected)
	if s.verified.IsSome() {
		t.Errorf("verified %v", s.verified)
	}
}

func TestAnUnreachableRegistryDelaysVerificationOnly(t *testing.T) {
	s := newSim(t).run(
		registryUp{false},
		cluster{building()},
		cluster{finished(good)},
		cluster{outcome.JobObservation{}}, // the Job was cleaned up meanwhile
		cluster{outcome.JobObservation{}},
		registryUp{true},
	)
	if s.phase != opbuild.Succeeded {
		t.Errorf("%s: %v", s.phase, s.trail)
	}
	if d, ok := s.reported.Get(); !ok || d.String() != good {
		t.Errorf("reported %v", s.reported)
	}
}

// script is a random script with the weights of the Rust proptest
// strategy, 1 to 59 steps.
type script []step

// Generate implements quick.Generator.
func (script) Generate(r *rand.Rand, _ int) reflect.Value {
	weighted := []struct {
		weight int
		step   step
	}{
		{4, cluster{pending()}},
		{4, cluster{fetching()}},
		{6, cluster{building()}},
		{3, cluster{finished(good)}},
		{2, cluster{finished(forged)}},
		{1, cluster{oom()}},
		{1, cluster{deadline()}},
		{1, cluster{diskEviction()}},
		{1, cluster{nodeLost()}},
		{1, cluster{outcome.JobObservation{}}},
		{1, cancel{}},
		{1, registryUp{false}},
		{2, registryUp{true}},
	}
	total := 0
	for _, w := range weighted {
		total += w.weight
	}
	out := make(script, 1+r.Intn(59))
	for i := range out {
		n := r.Intn(total)
		for _, w := range weighted {
			if n < w.weight {
				out[i] = w.step
				break
			}
			n -= w.weight
		}
	}
	return reflect.ValueOf(out)
}

// Whatever the cluster and registry do, every recorded event is legal, a
// build succeeds only with a digest the registry holds, is cancelled only
// when asked, and retries stay bounded.
func TestBuildsStayConsistentUnderAnyFailure(t *testing.T) {
	property := func(sc script) bool {
		asked := slices.ContainsFunc(sc, func(s step) bool { _, ok := s.(cancel); return ok })
		s := newSim(t).run(sc...)
		ok := s.illegal == nil && s.attempt <= store.MaxBuildAttempts
		if s.phase == opbuild.Succeeded {
			d, verified := s.verified.Get()
			ok = ok && verified && d.String() == good
		} else {
			ok = ok && s.verified.IsNone()
		}
		if s.phase == opbuild.Cancelled {
			ok = ok && asked
		}
		if s.phase == opbuild.Failed {
			ok = ok && s.failure.IsSome()
		}
		return ok
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 512}); err != nil {
		t.Error(err)
	}
}
