package httpapi

// A build's log (2.1): read from its build pod while the pod exists, from
// the tail the build worker kept with the attempt once it is gone
// (store.BuildLogTail). Both read as the kept tail does: the fetch, plan
// and build containers in that order, each under a `==> <stage> <==`
// header (build.LogOf); the scan container's output is the SBOM, kept with
// the scan.
//
// `?follow=true` is a Server-Sent Events stream like a followed app log
// (apps_logs.go), under the same bounds (a few per user, an hour, 16 KiB a
// line, a ping every 15 s):
//
//	event: line  data: {"pod":"kbuild-…","process":"clone|plan|build","time":"…"|null,"line":"…"}
//	event: end   data: {"pod":"kbuild-…"|null,"error":"…"|null}
//
// While the attempt waits for its pod the stream waits too; it then
// follows each container as it starts, and sends one `end` when the build
// container's log ends or the pod is gone. A build whose pod is gone sends
// its kept tail as `line` events (`time` null, `pod` the build's Job name),
// then `end` with `pod` null.

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/sync/errgroup"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/build"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/ids"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/kerrors"
	opoutcome "github.com/Teamtem-dev/kuben/apps/kuben/internal/core/ops/outcome"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/perm"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/httpapi/gen"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/httpapi/httpx"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/store"
)

const (
	// defaultBuildNamespace is where build pods run unless `build.namespace`
	// says otherwise (the server's buildNamespace).
	defaultBuildNamespace = "kuben-builds"
	// buildPoll is how often a followed build log looks at its pod.
	buildPoll = 2 * time.Second
	// buildLogLines is how many lines of each container a one-shot read
	// takes.
	buildLogLines = int64(5000)
)

// buildContainers are the containers of a build pod whose logs are the
// build's, in order, with the stage each is.
var buildContainers = []struct {
	container string
	stage     store.BuildStageName
}{
	{opoutcome.FetchContainer, store.StageClone},
	{build.PlanContainer, store.StagePlan},
	{opoutcome.BuildContainer, store.StageBuild},
}

// buildNamespace is the namespace of build pods.
func (s *Server) buildNamespace() string {
	if n := s.deps.Config.Build.Namespace.Or(""); n != "" {
		return n
	}
	return defaultBuildNamespace
}

// newestBuildPod is the newest pod of attempt; none when it has none (any
// more).
func newestBuildPod(ctx context.Context, pods corev1client.PodInterface, attempt ids.BuildAttemptID) (opt.Val[*corev1.Pod], error) {
	listed, err := pods.List(ctx, metav1.ListOptions{LabelSelector: build.AttemptLabel + "=" + attempt.String()})
	if err != nil {
		return opt.None[*corev1.Pod](), fmt.Errorf("listing the build pods: %w", err)
	}
	newest := opt.None[*corev1.Pod]()
	for i := range listed.Items {
		p := &listed.Items[i]
		if n, ok := newest.Get(); ok && p.CreationTimestamp.Before(&n.CreationTimestamp) {
			continue
		}
		newest = opt.Some(p)
	}
	return newest, nil
}

// containerStarted reports whether pod's container name runs or ran.
func containerStarted(pod *corev1.Pod, name string) bool {
	for _, statuses := range [][]corev1.ContainerStatus{pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses} {
		for _, s := range statuses {
			if s.Name == name && (s.State.Running != nil || s.State.Terminated != nil) {
				return true
			}
		}
	}
	return false
}

// podDone reports whether pod will start no more containers.
func podDone(pod *corev1.Pod) bool {
	return pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed
}

// readBuildPod is the log of pod as the kept tail has it: the containers
// that started, each at most 5000 lines and 1 MiB.
func readBuildPod(ctx context.Context, pods corev1client.PodInterface, pod *corev1.Pod) string {
	parts := map[string]string{}
	lines, limit := buildLogLines, logBytesLimit
	for _, c := range buildContainers {
		if !containerStarted(pod, c.container) {
			continue
		}
		raw, err := pods.GetLogs(pod.Name, &corev1.PodLogOptions{
			Container: c.container, TailLines: &lines, LimitBytes: &limit,
		}).Do(ctx).Raw()
		if err != nil {
			parts[c.container] = "(the log could not be read: " + kubeReadError(err) + ")\n"
			continue
		}
		parts[c.container] = string(raw)
	}
	return build.LogOf(parts, store.MaxBuildLogTail)
}

// GetBuildLogs is the build's log as text, or with follow=true a live
// stream of its lines.
func (s *Server) GetBuildLogs(ctx context.Context, params gen.GetBuildLogsParams) (gen.GetBuildLogsRes, error) {
	acc, err := s.access(ctx)
	if err != nil {
		return nil, err
	}
	a, err := s.findApp(ctx, acc, params.Project, params.Environment, params.App)
	if err != nil {
		return nil, err
	}
	if _, err := acc.Require(perm.AppLogsRead, a.chain()); err != nil {
		return nil, err //nolint:wrapcheck // a kerrors already
	}
	id, err := buildID(params.Build)
	if err != nil {
		return nil, err
	}
	org := a.env.project.org
	attempt, err := s.readBuild(ctx, org, a.app.Target, id, params.Build)
	if err != nil {
		return nil, err
	}
	settled := attempt.Phase.IsTerminal()
	var pods corev1client.PodInterface
	cluster, clusterErr := s.cluster()
	switch {
	case clusterErr == nil:
		pods = cluster.Typed.CoreV1().Pods(s.buildNamespace())
	case !settled:
		// A running build's log is in its pod.
		return nil, clusterErr
	}
	source := buildLogSource{
		pods: pods, attempt: attempt.ID, job: build.Name(attempt), poll: buildPoll,
		settled: func(ctx context.Context) (bool, error) {
			now, err := s.readBuild(ctx, org, a.app.Target, id, params.Build)
			return now.Phase.IsTerminal(), err
		},
		tail: func(ctx context.Context) (string, error) { return s.buildLogTail(ctx, org, id) },
	}
	if params.Follow.Or(false) {
		permit, err := s.logStreams.Acquire(acc.Current.User.ID)
		if err != nil {
			return nil, err //nolint:wrapcheck // a kerrors already
		}
		defer permit.Release()
		w, ok := httpx.ResponseWriterFrom(ctx)
		if !ok {
			return nil, kerrors.New(kerrors.Internal, "response writer not available")
		}
		flusher, ok := w.(http.Flusher)
		if !ok {
			return nil, kerrors.New(kerrors.Internal, "streaming not supported")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()
		s.followBuildLog(ctx, w, flusher.Flush, source, followTiming{
			limit: followLimit, rescan: buildPoll, keepAlive: keepAliveInterval,
		})
		return nil, errStreamHandled
	}
	text, err := source.once(ctx)
	if err != nil {
		return nil, err
	}
	return &gen.GetBuildLogsOK{Data: strings.NewReader(text)}, nil
}

// readBuild is build id of tgt, read in a transaction of its own.
func (s *Server) readBuild(ctx context.Context, org ids.OrgID, tgt ids.TargetID, id ids.BuildAttemptID, name string) (store.BuildAttempt, error) {
	t, err := s.deps.Store.Tenant(ctx, org)
	if err != nil {
		return store.BuildAttempt{}, err //nolint:wrapcheck // a store error, answered as internal
	}
	defer t.Rollback(ctx) //nolint:errcheck // read only
	return buildOfTarget(ctx, t, tgt, id, name)
}

// buildLogTail is the log kept with build id; "" when none was.
func (s *Server) buildLogTail(ctx context.Context, org ids.OrgID, id ids.BuildAttemptID) (string, error) {
	t, err := s.deps.Store.Tenant(ctx, org)
	if err != nil {
		return "", err //nolint:wrapcheck // a store error, answered as internal
	}
	defer t.Rollback(ctx) //nolint:errcheck // read only
	tail, err := t.BuildLogTail(ctx, id)
	return tail.Or(""), err //nolint:wrapcheck // a store error, answered as internal
}

// buildLogSource is where the log of one build is read from.
type buildLogSource struct {
	// pods is the build namespace's pods; nil without a cluster.
	pods    corev1client.PodInterface
	attempt ids.BuildAttemptID
	// job is the name of the build's Job, which the lines of a kept tail
	// name as their pod.
	job  string
	poll time.Duration
	// settled reports whether the attempt has settled.
	settled func(context.Context) (bool, error)
	// tail is the log kept with the attempt.
	tail func(context.Context) (string, error)
}

// pod is the attempt's newest pod; none without a cluster.
func (b buildLogSource) pod(ctx context.Context) (opt.Val[*corev1.Pod], error) {
	if b.pods == nil {
		return opt.None[*corev1.Pod](), nil
	}
	return newestBuildPod(ctx, b.pods, b.attempt)
}

// once is the log now: the pod's while it exists, else the kept tail.
func (b buildLogSource) once(ctx context.Context) (string, error) {
	pod, err := b.pod(ctx)
	if err != nil {
		return "", kerrors.New(kerrors.Unavailable, "the build pod cannot be read: %s", kubeReadError(err))
	}
	if p, ok := pod.Get(); ok {
		return readBuildPod(ctx, b.pods, p), nil
	}
	return b.tail(ctx)
}

// wait sleeps for the poll interval; false when ctx ended first.
func (b buildLogSource) wait(ctx context.Context) bool {
	timer := time.NewTimer(b.poll)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// stream sends the log's lines to out, then one end, unless ctx ends
// first.
func (b buildLogSource) stream(ctx context.Context, out chan<- logPiece) {
	for {
		pod, err := b.pod(ctx)
		if err != nil {
			sendPiece(ctx, out, pieceEnd{err: opt.Some(kubeReadError(err))})
			return
		}
		if p, ok := pod.Get(); ok {
			b.streamPod(ctx, p.Name, out)
			return
		}
		settled, err := b.settled(ctx)
		switch {
		case err != nil:
			sendPiece(ctx, out, pieceEnd{err: opt.Some("the build cannot be read")})
			return
		case settled:
			b.streamTail(ctx, out)
			return
		}
		if !b.wait(ctx) {
			return
		}
	}
}

// streamTail sends the kept tail, each line under the stage its header
// names, then an end without a pod.
func (b buildLogSource) streamTail(ctx context.Context, out chan<- logPiece) {
	text, err := b.tail(ctx)
	if err != nil {
		sendPiece(ctx, out, pieceEnd{err: opt.Some("the kept log cannot be read")})
		return
	}
	stage := opt.None[string]()
	for _, line := range textLines(text) {
		if name, ok := strings.CutPrefix(line, "==> "); ok {
			if name, ok = strings.CutSuffix(name, " <=="); ok {
				stage = opt.Some(name)
				continue
			}
		}
		if !sendPiece(ctx, out, pieceLine{line: logLine{Pod: b.job, Process: stage, Time: opt.None[string](), Line: cutLine(line)}}) {
			return
		}
	}
	sendPiece(ctx, out, pieceEnd{})
}

// streamPod follows each build container of pod as it starts, then sends
// the pod's end.
func (b buildLogSource) streamPod(ctx context.Context, pod string, out chan<- logPiece) {
	for _, c := range buildContainers {
		started, alive := b.waitStarted(ctx, pod, c.container)
		if ctx.Err() != nil {
			return
		}
		if !alive {
			break
		}
		if !started {
			continue
		}
		body, err := b.pods.GetLogs(pod, &corev1.PodLogOptions{Container: c.container, Follow: true, Timestamps: true}).Stream(ctx)
		if err != nil {
			sendPiece(ctx, out, pieceEnd{pod: pod, err: opt.Some(kubeReadError(err))})
			return
		}
		end, ok := readLines(ctx, body, pod, opt.Some(string(c.stage)), out)
		_ = body.Close() //nolint:errcheck // the read is over either way
		if !ok {
			return
		}
		if end.IsSome() {
			sendPiece(ctx, out, pieceEnd{pod: pod, err: end})
			return
		}
	}
	sendPiece(ctx, out, pieceEnd{pod: pod})
}

// waitStarted waits until pod's container runs or ran (started), or the
// pod will not start it; alive is false when the pod is gone.
func (b buildLogSource) waitStarted(ctx context.Context, name, container string) (started, alive bool) {
	for {
		pod, err := b.pods.Get(ctx, name, metav1.GetOptions{})
		switch {
		case err != nil:
			return false, false
		case containerStarted(pod, container):
			return true, true
		case podDone(pod):
			return false, true
		}
		if !b.wait(ctx) {
			return false, false
		}
	}
}

// cutLine is line cut to maxLine bytes on a character boundary.
func cutLine(line string) string {
	if len(line) <= maxLine {
		return line
	}
	end := maxLine
	for end > 0 && !utf8.RuneStart(line[end]) {
		end--
	}
	return line[:end]
}

// followBuildLog streams source's log to w until it ends, the client
// leaves or the stream reaches its limit. The read runs in an errgroup
// that has ended when followBuildLog returns.
func (s *Server) followBuildLog(ctx context.Context, w io.Writer, flush func(), source buildLogSource, timing followTiming) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	g, readCtx := errgroup.WithContext(ctx)
	pieces := make(chan logPiece)
	g.Go(func() error {
		source.stream(readCtx, pieces)
		return nil
	})
	if err := writeBuildEvents(ctx, w, flush, pieces, timing); err != nil {
		s.deps.Logger.Debug("followed build log ended", "error", err)
	}
	cancel()
	if err := g.Wait(); err != nil {
		s.deps.Logger.Debug("followed build log read", "error", err)
	}
}

// writeBuildEvents writes the pieces as events until the end piece, the
// limit, or ctx's end.
func writeBuildEvents(ctx context.Context, w io.Writer, flush func(), pieces <-chan logPiece, timing followTiming) error {
	limit := time.NewTimer(timing.limit)
	defer limit.Stop()
	keepAlive := time.NewTimer(timing.keepAlive)
	defer keepAlive.Stop()
	write := func(b []byte) error {
		if _, err := w.Write(b); err != nil {
			return fmt.Errorf("writing a followed build log: %w", err)
		}
		flush()
		keepAlive.Reset(timing.keepAlive)
		return nil
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-limit.C:
			return write(sseEvent("end", logEnd{Pod: opt.None[string](), Error: opt.Some(limitMessage)}))
		case <-keepAlive.C:
			if err := write([]byte(ssePing)); err != nil {
				return err
			}
		case p := <-pieces:
			switch p := p.(type) {
			case pieceLine:
				if err := write(sseEvent("line", p.line)); err != nil {
					return err
				}
			case pieceEnd:
				pod := opt.None[string]()
				if p.pod != "" {
					pod = opt.Some(p.pod)
				}
				return write(sseEvent("end", logEnd{Pod: pod, Error: p.err}))
			}
		}
	}
}
