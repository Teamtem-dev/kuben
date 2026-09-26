package httpapi

// What an app says about itself (M2.12): its log lines, once or followed
// live, and the Kubernetes events of its own objects (routes/apps/logs.rs).
//
// A followed log is a Server-Sent Events stream: the one stream besides the
// tab's (ADR-014), since log lines are too many for the shared one. Each is
// bounded: a few per user and a hundred on a replica, an hour long, a line
// at most 16 KiB. The client sets the pace (a slow reader slows the read
// from the cluster), and closing the connection ends the reads at once.

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/sync/errgroup"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"

	"github.com/Teamtem-dev/kuben/internal/core/clock"
	"github.com/Teamtem-dev/kuben/internal/core/ids"
	"github.com/Teamtem-dev/kuben/internal/core/kerrors"
	"github.com/Teamtem-dev/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/internal/core/perm"
	"github.com/Teamtem-dev/kuben/internal/httpapi/gen"
	"github.com/Teamtem-dev/kuben/internal/httpapi/httpx"
	"github.com/Teamtem-dev/kuben/internal/kube/projection"
	"github.com/Teamtem-dev/kuben/internal/kube/render"
)

// errStreamHandled tells writeError that the handler already answered (a
// followed log wrote its own event stream).
var errStreamHandled = errors.New("stream handled")

const (
	// maxLogPods is how many pods are read at once, once or followed.
	maxLogPods = 10
	// maxFollowsPerUser and maxFollows bound the followed logs of one user
	// and of one replica.
	maxFollowsPerUser = 4
	maxFollows        = 100
	// followLimit ends a followed log; the client reconnects if it still
	// wants it.
	followLimit = time.Hour
	// rescanInterval is how often a followed log looks for new pods (a
	// rollout, a restart).
	rescanInterval = 5 * time.Second
	// keepAliveInterval is how long a followed log stays silent before it
	// sends a ping comment (axum's KeepAlive, reset by every event).
	keepAliveInterval = 15 * time.Second
	// maxLine cuts longer lines.
	maxLine = 16 * 1024
	// maxEvents is how many events are returned, newest first.
	maxEvents = 100
	// logBytesLimit bounds a one-shot read of one pod.
	logBytesLimit = int64(1 << 20)
	// followRetryAfterSecs is the Retry-After of a refused followed log.
	followRetryAfterSecs = 5
)

// limitMessage is the end of a stream that reached followLimit.
const limitMessage = "the stream reached its limit; reconnect to go on"

// invalidUTF8 is the error futures' Lines gives on a line that is not
// UTF-8: the followed read of that pod ends with it.
const invalidUTF8 = "stream did not contain valid UTF-8"

// logStreams tracks followed logs open on this replica, per user.
type logStreams struct {
	mu   sync.Mutex
	open map[ids.UserID]int
}

// logStreamPermit is one open followed log; releasing it frees its place.
type logStreamPermit struct {
	streams *logStreams
	user    ids.UserID
}

func newLogStreams() *logStreams {
	return &logStreams{
		open: make(map[ids.UserID]int),
	}
}

// Acquire reserves a place for one more followed log of user.
func (s *logStreams) Acquire(user ids.UserID) (*logStreamPermit, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	total := 0
	for _, c := range s.open {
		total += c
	}
	mine := s.open[user]
	if mine >= maxFollowsPerUser || total >= maxFollows {
		return nil, kerrors.TooMany(followRetryAfterSecs)
	}
	s.open[user] = mine + 1
	return &logStreamPermit{
		streams: s,
		user:    user,
	}, nil
}

// Release frees the permit's place; a user with none left leaves the map.
func (p *logStreamPermit) Release() {
	if p == nil || p.streams == nil {
		return
	}
	p.streams.mu.Lock()
	defer p.streams.mu.Unlock()
	if c, ok := p.streams.open[p.user]; ok {
		if c <= 1 {
			delete(p.streams.open, p.user)
		} else {
			p.streams.open[p.user] = c - 1
		}
	}
	p.streams = nil
}

// splitLine separates `2026-09-16T10:00:00.123Z message` into its time and
// message, the message cut to maxLine bytes on a character boundary.
func splitLine(raw string) (opt.Val[string], string) {
	t := opt.None[string]()
	line := raw
	if prefix, rest, ok := strings.Cut(raw, " "); ok && len(prefix) >= 20 && prefix[4] == '-' {
		t, line = opt.Some(prefix), rest
	}
	if len(line) > maxLine {
		end := maxLine
		for end > 0 && !utf8.RuneStart(line[end]) {
			end--
		}
		line = line[:end]
	}
	return t, line
}

// textLines splits text as Rust's str::lines: on "\n", a "\r" before it
// dropped, no empty line after a final "\n".
func textLines(text string) []string {
	out := []string{}
	for text != "" {
		line, rest, found := strings.Cut(text, "\n")
		if found {
			line = strings.TrimSuffix(line, "\r")
		}
		out = append(out, line)
		text = rest
	}
	return out
}

// utf8ErrorText is the Display of Rust's Utf8Error for b, which is not
// valid UTF-8: where the valid prefix ends and how long the invalid
// sequence is (absent when b ends in the middle of a character).
func utf8ErrorText(b []byte) string {
	at := 0
	for at < len(b) {
		r, n := utf8.DecodeRune(b[at:])
		if r == utf8.RuneError && n == 1 {
			break
		}
		at += n
	}
	if n, ok := invalidSequenceLen(b[at:]).Get(); ok {
		return fmt.Sprintf("invalid utf-8 sequence of %d bytes from index %d", n, at)
	}
	return fmt.Sprintf("incomplete utf-8 byte sequence from index %d", at)
}

// invalidSequenceLen is Rust's Utf8Error::error_len for the bytes b that
// start at the first invalid position: absent when they are a character
// cut short by the end.
func invalidSequenceLen(b []byte) opt.Val[int] {
	if len(b) == 0 {
		return opt.None[int]()
	}
	width, lo, hi := leadByte(b[0])
	if width == 0 {
		return opt.Some(1)
	}
	for i := 1; i < width; i++ {
		if i >= len(b) {
			return opt.None[int]()
		}
		if i > 1 {
			lo, hi = 0x80, 0xBF
		}
		if b[i] < lo || b[i] > hi {
			return opt.Some(i)
		}
	}
	return opt.Some(width) // unreachable for invalid input
}

// leadByte is the width of the character a byte starts and the range its
// second byte must be in (Rust's run_utf8_validation); width 0 for a byte
// that starts none.
func leadByte(first byte) (width int, lo, hi byte) {
	switch {
	case first >= 0xC2 && first <= 0xDF:
		return 2, 0x80, 0xBF
	case first == 0xE0:
		return 3, 0xA0, 0xBF
	case first == 0xED:
		return 3, 0x80, 0x9F
	case first >= 0xE1 && first <= 0xEF:
		return 3, 0x80, 0xBF
	case first == 0xF0:
		return 4, 0x90, 0xBF
	case first >= 0xF1 && first <= 0xF3:
		return 4, 0x80, 0xBF
	case first == 0xF4:
		return 4, 0x80, 0x8F
	default:
		return 0, 0, 0
	}
}

// partsAfter counts how many hyphen-separated segments follow prefix in name.
func partsAfter(name, prefix string) (int, bool) {
	rest, ok := strings.CutPrefix(name, prefix)
	if !ok {
		return 0, false
	}
	rest, ok = strings.CutPrefix(rest, "-")
	if !ok || rest == "" {
		return 0, false
	}
	return strings.Count(rest, "-") + 1, true
}

// belongs checks if an object named name of kind belongs to the app: the
// app's own name (App, Service, HTTPRoute, …), a workload of one of its
// processes, or what a workload made (ReplicaSets, Jobs, Pods). A pod the
// projection knows is the app's by its label; one already gone is judged by
// its name.
func belongs(kind, name, app string, workloads []string, pods map[string]struct{}) bool {
	made := func(maxParts int) bool {
		for _, w := range workloads {
			if n, ok := partsAfter(name, w); ok && n <= maxParts {
				return true
			}
		}
		return false
	}
	switch kind {
	case "Deployment", "CronJob", "HorizontalPodAutoscaler", "PodDisruptionBudget":
		return slices.Contains(workloads, name)
	case "ReplicaSet", "Job":
		// `<deployment>-<hash>`; `<cron>-<time>` or `<cron>-run-<time>`.
		return made(2)
	case "Pod":
		// `<replicaset>-<hash>`, `<job>-<hash>`.
		if _, ok := pods[name]; ok {
			return true
		}
		return made(3)
	default:
		return name == app
	}
}

type appObjectsResult struct {
	workloads []string
	pods      map[string]struct{}
	// certNamespace and certNames are the Gateway's namespace and the
	// certificates of the app's hosts (empty when there are none).
	certNamespace string
	certNames     map[string]struct{}
}

func appObjects(s *Server, a appScope) appObjectsResult {
	namespace := a.app.Namespace
	name := a.app.Slug
	var workloads []string
	if v, ok := a.view.Get(); ok {
		for _, p := range v.Processes {
			workloads = append(workloads, render.WorkloadName(name, p.Name))
		}
	}
	podViews := s.deps.Projections.PodsOfApp(namespace, name)
	pods := make(map[string]struct{}, len(podViews))
	for _, p := range podViews {
		pods[p.Name] = struct{}{}
	}
	var certNamespace string
	var certNames map[string]struct{}
	if route, ok := s.deps.Projections.Route(namespace, name); ok && len(route.Gateways) > 0 {
		if gwNS, _, ok := strings.Cut(route.Gateways[0], "/"); ok {
			if exp, ok := s.deps.Projections.Exposure(namespace, name); ok {
				names := make(map[string]struct{})
				for _, h := range exp.Hosts {
					if h.CertificateReady.IsSome() {
						names[render.HostSecretName(h.Host)] = struct{}{}
					}
				}
				if len(names) > 0 {
					certNamespace = gwNS
					certNames = names
				}
			}
		}
	}
	return appObjectsResult{
		workloads:     workloads,
		pods:          pods,
		certNamespace: certNamespace,
		certNames:     certNames,
	}
}

// textOf is absent for "": the Go API types use "" (omitted on the wire)
// where k8s-openapi had None.
func textOf(s string) opt.Val[string] {
	if s == "" {
		return opt.None[string]()
	}
	return opt.Some(s)
}

// timestampText is a time as jiff's Timestamp Display writes it: UTC, with
// the fractional seconds only when there are some, trailing zeros dropped.
func timestampText(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

// appEventFrom is the AppEvent of a Kubernetes event. Kubernetes omits zero
// counts and empty strings, so those read as Rust's None.
func appEventFrom(e *corev1.Event) gen.AppEvent {
	lastSeen := opt.None[time.Time]()
	switch {
	case e.Series != nil && !e.Series.LastObservedTime.IsZero():
		lastSeen = opt.Some(e.Series.LastObservedTime.Time)
	case !e.LastTimestamp.IsZero():
		lastSeen = opt.Some(e.LastTimestamp.Time)
	case !e.EventTime.IsZero():
		lastSeen = opt.Some(e.EventTime.Time)
	case !e.CreationTimestamp.IsZero():
		lastSeen = opt.Some(e.CreationTimestamp.Time)
	}
	firstSeen := lastSeen
	if !e.FirstTimestamp.IsZero() {
		firstSeen = opt.Some(e.FirstTimestamp.Time)
	}
	count := int32(1)
	switch {
	case e.Series != nil && e.Series.Count != 0:
		count = e.Series.Count
	case e.Count != 0:
		count = e.Count
	}
	eventType := e.Type
	if eventType == "" {
		eventType = "Normal"
	}
	// ReportingController is the Go name of `reportingComponent`.
	source := textOf(e.ReportingController)
	if source.IsNone() {
		source = textOf(e.Source.Component)
	}
	return gen.AppEvent{
		Count:     max(count, 1),
		FirstSeen: optNilString(optMap(firstSeen, timestampText)),
		Kind:      e.InvolvedObject.Kind,
		LastSeen:  optNilString(optMap(lastSeen, timestampText)),
		Message:   optNilString(textOf(e.Message)),
		Name:      e.InvolvedObject.Name,
		Reason:    optNilString(textOf(e.Reason)),
		Source:    optNilString(source),
		Type:      eventType,
	}
}

// optMap applies f to a present value.
func optMap[T, U any](v opt.Val[T], f func(T) U) opt.Val[U] {
	if x, ok := v.Get(); ok {
		return opt.Some(f(x))
	}
	return opt.None[U]()
}

// sortEvents orders events newest first, those never seen last, keeping
// the order of ties (Rust's stable sort_by on Option<String>).
func sortEvents(events []gen.AppEvent) {
	slices.SortStableFunc(events, func(a, b gen.AppEvent) int {
		aSeen, aOk := a.LastSeen.Get()
		bSeen, bOk := b.LastSeen.Get()
		switch {
		case aOk && bOk:
			return cmp.Compare(bSeen, aSeen)
		case aOk:
			return -1
		case bOk:
			return 1
		default:
			return 0
		}
	})
}

// podsOf is the app's pods, of one process when asked (an empty process is
// asked too and matches only pods of that process), at most maxLogPods.
func podsOf(projections *projection.Projections, namespace, app string, process opt.Val[string]) []*projection.PodView {
	pods := projections.PodsOfApp(namespace, app)
	out := make([]*projection.PodView, 0, min(len(pods), maxLogPods))
	for _, p := range pods {
		if want, ok := process.Get(); ok {
			if proc, has := p.Process.Get(); !has || proc != want {
				continue
			}
		}
		out = append(out, p)
		if len(out) >= maxLogPods {
			break
		}
	}
	return out
}

func kubeReadError(err error) string {
	var status apierrors.APIStatus
	if errors.As(err, &status) {
		return status.Status().Message
	}
	return err.Error()
}

// logLine is one line of a followed log.
type logLine struct {
	Pod     string          `json:"pod"`
	Process opt.Val[string] `json:"process"`
	// Time is when the container wrote it (RFC 3339).
	Time opt.Val[string] `json:"time"`
	Line string          `json:"line"`
}

// logEnd tells a followed log stopped: one pod's, or all of them (Pod is
// absent: the stream reached its limit).
type logEnd struct {
	Pod   opt.Val[string] `json:"pod"`
	Error opt.Val[string] `json:"error"`
}

// serdeJSON is v as serde_json writes it: Go's encoding with HTML escaping
// off and U+2028/U+2029 written as themselves (Go escapes them).
func serdeJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, fmt.Errorf("encoding an event: %w", err)
	}
	text := bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
	out := make([]byte, 0, len(text))
	for i := 0; i < len(text); i++ {
		if text[i] != '\\' || i+1 >= len(text) {
			out = append(out, text[i])
			continue
		}
		switch string(text[i+1 : min(i+6, len(text))]) {
		case "u2028":
			out = append(out, "\u2028"...)
			i += 5
			continue
		case "u2029":
			out = append(out, "\u2029"...)
			i += 5
			continue
		}
		out = append(out, text[i], text[i+1]) // an escape pair, kept whole
		i++
	}
	return out, nil
}

// sseEvent is one named event as axum writes it: `event: <name>`, one
// `data: ` line of JSON, a blank line. An event that cannot be encoded is
// an `error` event, as Rust's json_event made it.
func sseEvent(name string, data any) []byte {
	payload, err := serdeJSON(data)
	if err != nil {
		return []byte("event: error\ndata: serialize\n\n")
	}
	out := make([]byte, 0, len(payload)+len(name)+17)
	out = append(out, "event: "...)
	out = append(out, name...)
	out = append(out, "\ndata: "...)
	out = append(out, payload...)
	return append(out, "\n\n"...)
}

// ssePing is axum's keep-alive comment with the text "ping".
const ssePing = ": ping\n\n"

// followsLogs reports whether r asks for a followed log: the getAppLogs
// route with `follow` true as the generated decoder reads it (one value,
// strconv.ParseBool). Such a stream outlives the request timeout, which
// Rust's TimeoutLayer never applied to a body either.
func (s *Server) followsLogs(r *http.Request) bool {
	route, ok := s.routes.FindPath(r.Method, r.URL)
	if !ok || route.Name() != gen.GetAppLogsOperation {
		return false
	}
	values := r.URL.Query()["follow"]
	if len(values) != 1 {
		return false
	}
	follow, err := strconv.ParseBool(values[0])
	return err == nil && follow
}

// GetAppEvents lists the app's Kubernetes events (pods, workloads, route,
// certificates), newest first, at most 100.
func (s *Server) GetAppEvents(ctx context.Context, params gen.GetAppEventsParams) (gen.GetAppEventsRes, error) {
	acc, err := s.access(ctx)
	if err != nil {
		return nil, err
	}
	a, err := s.findApp(ctx, acc, params.Project, params.Environment, params.App)
	if err != nil {
		return nil, err
	}
	if _, err := acc.Require(perm.AppRead, a.chain()); err != nil {
		return nil, err //nolint:wrapcheck // a kerrors already
	}
	cluster, err := s.cluster()
	if err != nil {
		return nil, err
	}
	own := appObjects(s, a)
	eventsList, err := cluster.Typed.CoreV1().Events(a.app.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, kubeError(err, a.app.Slug)
	}
	out := []gen.AppEvent{}
	for i := range eventsList.Items {
		e := &eventsList.Items[i]
		if belongs(e.InvolvedObject.Kind, e.InvolvedObject.Name, a.app.Slug, own.workloads, own.pods) {
			out = append(out, appEventFrom(e))
		}
	}
	// The certificates of the app's own listeners live beside the Gateway.
	if own.certNamespace != "" && len(own.certNames) > 0 {
		certList, err := cluster.Typed.CoreV1().Events(own.certNamespace).List(ctx, metav1.ListOptions{
			FieldSelector: "involvedObject.kind=Certificate",
		})
		if err != nil {
			s.deps.Logger.Debug("cannot read certificate events", "error", err, "namespace", own.certNamespace)
		} else {
			for i := range certList.Items {
				e := &certList.Items[i]
				if _, ok := own.certNames[e.InvolvedObject.Name]; ok {
					out = append(out, appEventFrom(e))
				}
			}
		}
	}
	sortEvents(out)
	if len(out) > maxEvents {
		out = out[:maxEvents]
	}
	res := gen.GetAppEventsOKApplicationJSON(out)
	return &res, nil
}

// GetAppLogs returns recent log lines per pod (at most 10 pods), or with
// follow=true streams new ones as Server-Sent Events.
func (s *Server) GetAppLogs(ctx context.Context, params gen.GetAppLogsParams) (gen.GetAppLogsRes, error) {
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
	cluster, err := s.cluster()
	if err != nil {
		return nil, err
	}
	pods := cluster.Typed.CoreV1().Pods(a.app.Namespace)
	process := opt.None[string]()
	if p, ok := params.Process.Get(); ok {
		process = opt.Some(p)
	}
	tail := max(int64(1), min(int64(2000), params.Tail.Or(200)))

	if params.Follow.Or(false) {
		permit, err := s.logStreams.Acquire(acc.Current.User.ID)
		if err != nil {
			return nil, err
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
		s.followLogs(ctx, w, flusher.Flush, pods, podSource{
			projections: s.deps.Projections, namespace: a.app.Namespace, app: a.app.Slug, process: process,
		}, tail)
		return nil, errStreamHandled
	}

	targets := podsOf(s.deps.Projections, a.app.Namespace, a.app.Slug, process)
	out := make([]gen.PodLogs, len(targets))
	opts := corev1.PodLogOptions{
		Timestamps: true,
		Previous:   params.Previous.Or(false),
		TailLines:  &tail,
		LimitBytes: new(logBytesLimit),
	}
	var g errgroup.Group
	for i, pod := range targets {
		g.Go(func() error {
			out[i] = readLogsOnce(ctx, pods, pod, opts)
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, fmt.Errorf("reading logs: %w", err)
	}
	res := gen.GetAppLogsOKApplicationJSON(out)
	return &res, nil
}

// readLogsOnce is the recent lines of one pod, or why they could not be
// read. A body that is not UTF-8 is an error, as kube's logs() made it.
func readLogsOnce(ctx context.Context, pods corev1client.PodInterface, pod *projection.PodView, opts corev1.PodLogOptions) gen.PodLogs {
	opts.Container = pod.Process.Or("")
	item := gen.PodLogs{Pod: pod.Name, Process: optNilString(pod.Process), Lines: []string{}}
	item.Error.SetToNull()
	raw, err := pods.GetLogs(pod.Name, &opts).Do(ctx).Raw()
	switch {
	case err != nil:
		item.Error = gen.NewOptNilString(kubeReadError(err))
	case !utf8.Valid(raw):
		item.Error = gen.NewOptNilString("UTF-8 Error: " + utf8ErrorText(raw))
	default:
		item.Lines = textLines(string(raw))
	}
	return item
}

// logPiece is what a pod's read yields: a line, then its end.
//
//sumtype:decl
type logPiece interface{ isLogPiece() }

// pieceLine is one line of a pod.
type pieceLine struct{ line logLine }

// pieceEnd is the end of a pod's read, and why.
type pieceEnd struct {
	pod string
	err opt.Val[string]
}

func (pieceLine) isLogPiece() {}
func (pieceEnd) isLogPiece()  {}

// podSource is which pods a followed log reads.
type podSource struct {
	projections *projection.Projections
	namespace   string
	app         string
	process     opt.Val[string]
}

func (p podSource) pods() []*projection.PodView {
	return podsOf(p.projections, p.namespace, p.app, p.process)
}

// followTiming is how long a followed log waits for what. They are
// durations measured by the runtime's monotonic timers, as tokio's were,
// not points in time, so they do not go through the clock; tests shorten
// them.
type followTiming struct {
	limit     time.Duration
	rescan    time.Duration
	keepAlive time.Duration
}

// followLogs streams the lines of the app's pods to w until the client
// leaves or the stream reaches its limit. Every pod read runs in one
// errgroup that has ended when followLogs returns.
func (s *Server) followLogs(
	ctx context.Context, w io.Writer, flush func(),
	pods corev1client.PodInterface, source podSource, tail int64,
) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	g, readCtx := errgroup.WithContext(ctx)
	pieces := make(chan logPiece)
	f := &follower{
		w: w, flush: flush, clock: s.deps.Clock,
		timing: followTiming{limit: followLimit, rescan: rescanInterval, keepAlive: keepAliveInterval},
		pods:   source.pods,
		read: func(pod *projection.PodView, opts *corev1.PodLogOptions) {
			g.Go(func() error {
				readPodLog(readCtx, s.deps.Logger, pods, pod, opts, pieces)
				return nil
			})
		},
		pieces: pieces,
		tail:   tail,
	}
	if err := f.run(ctx); err != nil {
		s.deps.Logger.Debug("followed log ended", "error", err)
	}
	cancel()
	if err := g.Wait(); err != nil {
		s.deps.Logger.Debug("followed log reads", "error", err)
	}
}

// readPodLog sends the lines of one pod, then its end, to out. Cancelling
// ctx aborts the read of the body.
func readPodLog(
	ctx context.Context, logger *slog.Logger, pods corev1client.PodInterface,
	pod *projection.PodView, opts *corev1.PodLogOptions, out chan<- logPiece,
) {
	body, err := pods.GetLogs(pod.Name, opts).Stream(ctx)
	if err != nil {
		sendPiece(ctx, out, pieceEnd{pod: pod.Name, err: opt.Some(kubeReadError(err))})
		return
	}
	end, ok := readLines(ctx, body, pod.Name, pod.Process, out)
	if err := body.Close(); err != nil {
		logger.Debug("closing a pod log", "pod", pod.Name, "error", err)
	}
	if ok {
		sendPiece(ctx, out, pieceEnd{pod: pod.Name, err: end})
	}
}

// sendPiece hands p to the stream unless the stream is gone.
func sendPiece(ctx context.Context, out chan<- logPiece, p logPiece) bool {
	select {
	case out <- p:
		return true
	case <-ctx.Done():
		return false
	}
}

// readLines sends each line of body as futures' Lines reads it (a "\n",
// then a "\r" before it, stripped) and returns why the read ended: absent
// at the end of the body, the error otherwise (a line that is not UTF-8
// ends it). ok is false when the stream went away meanwhile.
func readLines(ctx context.Context, body io.Reader, pod string, process opt.Val[string], out chan<- logPiece) (end opt.Val[string], ok bool) {
	r := bufio.NewReader(body)
	for {
		raw, err := r.ReadString('\n')
		if ctx.Err() != nil {
			return opt.None[string](), false
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return opt.Some(err.Error()), true
		}
		if !utf8.ValidString(raw) {
			return opt.Some(invalidUTF8), true
		}
		if raw != "" {
			if line, cut := strings.CutSuffix(raw, "\n"); cut {
				raw = strings.TrimSuffix(line, "\r")
			}
			t, line := splitLine(raw)
			if !sendPiece(ctx, out, pieceLine{line: logLine{Pod: pod, Process: process, Time: t, Line: line}}) {
				return opt.None[string](), false
			}
		}
		if err != nil {
			return opt.None[string](), true
		}
	}
}

// podEnded is when a pod's read ended (clock milliseconds) and why.
type podEnded struct {
	atMs int64
	err  opt.Val[string]
}

// follower is the event loop of one followed log. Its maps are touched by
// run alone; the reads it starts talk to it through pieces.
type follower struct {
	w      io.Writer
	flush  func()
	clock  clock.Clock
	timing followTiming
	// pods is the pods to read now; read starts reading one.
	pods   func() []*projection.PodView
	read   func(pod *projection.PodView, opts *corev1.PodLogOptions)
	pieces <-chan logPiece
	tail   int64

	following map[string]struct{}
	// ended is when each pod's read ended, and why: a pod that is still
	// there is read again from then on, and the same error is told once.
	ended map[string]podEnded
	first bool
}

// run writes events until ctx ends, a write fails (the client left) or the
// stream reaches its limit.
func (f *follower) run(ctx context.Context) error {
	f.following, f.ended, f.first = map[string]struct{}{}, map[string]podEnded{}, true
	limit := time.NewTimer(f.timing.limit)
	defer limit.Stop()
	rescan := time.NewTicker(f.timing.rescan)
	defer rescan.Stop()
	keepAlive := time.NewTimer(f.timing.keepAlive)
	defer keepAlive.Stop()
	write := func(b []byte) error {
		if _, err := f.w.Write(b); err != nil {
			return fmt.Errorf("writing a followed log: %w", err)
		}
		f.flush()
		keepAlive.Reset(f.timing.keepAlive)
		return nil
	}
	f.scan()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-limit.C:
			return write(sseEvent("end", logEnd{Pod: opt.None[string](), Error: opt.Some(limitMessage)}))
		case <-rescan.C:
			f.scan()
		case <-keepAlive.C:
			if err := write([]byte(ssePing)); err != nil {
				return err
			}
		case p := <-f.pieces:
			if event, ok := f.event(p).Get(); ok {
				if err := write(event); err != nil {
					return err
				}
			}
		}
	}
}

// scan starts reading the pods not read yet: on the first scan the last
// lines, then a pod read before only from when its read ended, a new pod
// from its start.
func (f *follower) scan() {
	pods := f.pods()
	for name := range f.ended {
		if !slices.ContainsFunc(pods, func(p *projection.PodView) bool { return p.Name == name }) {
			delete(f.ended, name)
		}
	}
	for _, pod := range pods {
		if _, ok := f.following[pod.Name]; ok {
			continue
		}
		opts := &corev1.PodLogOptions{Follow: true, Timestamps: true, Container: pod.Process.Or("")}
		if prev, ok := f.ended[pod.Name]; f.first {
			opts.TailLines = new(f.tail)
		} else if ok {
			opts.SinceSeconds = new(max(clock.SaturatingSub(f.clock.NowMs(), prev.atMs)/1000, 1))
		}
		f.following[pod.Name] = struct{}{}
		f.read(pod, opts)
	}
	f.first = false
}

// event is what a piece writes: a line, or a pod's end unless the same
// end was told already.
func (f *follower) event(p logPiece) opt.Val[[]byte] {
	switch p := p.(type) {
	case pieceLine:
		return opt.Some(sseEvent("line", p.line))
	case pieceEnd:
		delete(f.following, p.pod)
		prev, had := f.ended[p.pod]
		told := had && prev.err == p.err
		f.ended[p.pod] = podEnded{atMs: f.clock.NowMs(), err: p.err}
		if told {
			return opt.None[[]byte]()
		}
		return opt.Some(sseEvent("end", logEnd{Pod: opt.Some(p.pod), Error: p.err}))
	}
	return opt.None[[]byte]()
}
