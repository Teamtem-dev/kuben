package api

// What an app says about itself (M2.12): its log lines, once or followed
// live, and the Kubernetes events of its own objects (routes/apps/logs.rs).

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Teamtem-dev/kuben/go/hub/internal/api/gen"
	"github.com/Teamtem-dev/kuben/go/hub/internal/api/httpx"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/ids"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/kerr"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/perm"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/projection"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/registry"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/render"
)

var errStreamHandled = errors.New("stream handled")

const (
	maxLogPods        = 10
	maxFollowsPerUser = 4
	maxFollows        = 100
	followLimit       = time.Hour
	rescanInterval    = 5 * time.Second
	maxLine           = 16 * 1024
	maxEvents         = 100
)

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
		return nil, kerr.TooMany(5)
	}
	s.open[user] = mine + 1
	return &logStreamPermit{
		streams: s,
		user:    user,
	}, nil
}

// Release frees the permit's place.
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

// Count returns the total number of open followed logs.
func (s *logStreams) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	total := 0
	for _, c := range s.open {
		total += c
	}
	return total
}

// splitLine separates RFC 3339 timestamp and message, cutting message to maxLine bytes
// on a valid UTF-8 character boundary.
func splitLine(raw string) (*string, string) {
	var timeStr *string
	line := raw
	if idx := strings.IndexByte(raw, ' '); idx != -1 {
		prefix := raw[:idx]
		if len(prefix) >= 20 && prefix[4] == '-' {
			timeStr = &prefix
			line = raw[idx+1:]
		}
	}
	if len(line) > maxLine {
		end := maxLine
		for end > 0 && !utf8.RuneStart(line[end]) {
			end--
		}
		line = line[:end]
	}
	return timeStr, line
}

// partsAfter counts how many hyphen-separated segments follow prefix in name.
func partsAfter(name, prefix string) (int, bool) {
	if !strings.HasPrefix(name, prefix) {
		return 0, false
	}
	rest := name[len(prefix):]
	if !strings.HasPrefix(rest, "-") {
		return 0, false
	}
	rest = rest[1:]
	if rest == "" {
		return 0, false
	}
	return strings.Count(rest, "-") + 1, true
}

// belongs checks if an object named name of kind belongs to the app.
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
		return made(2)
	case "Pod":
		if _, ok := pods[name]; ok {
			return true
		}
		return made(3)
	default:
		return name == app
	}
}

type appObjectsResult struct {
	workloads     []string
	pods          map[string]struct{}
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

func optNilStr(s *string) gen.OptNilString {
	if s != nil {
		return gen.NewOptNilString(*s)
	}
	var o gen.OptNilString
	o.SetToNull()
	return o
}

func optNilStrVal(s string, ok bool) gen.OptNilString {
	if ok {
		return gen.NewOptNilString(s)
	}
	var o gen.OptNilString
	o.SetToNull()
	return o
}

func appEventFrom(e corev1.Event) gen.AppEvent {
	var lastSeen *time.Time
	if e.Series != nil && !e.Series.LastObservedTime.IsZero() {
		t := e.Series.LastObservedTime.Time
		lastSeen = &t
	} else if !e.LastTimestamp.IsZero() {
		t := e.LastTimestamp.Time
		lastSeen = &t
	} else if !e.EventTime.IsZero() {
		t := e.EventTime.Time
		lastSeen = &t
	} else if !e.CreationTimestamp.IsZero() {
		t := e.CreationTimestamp.Time
		lastSeen = &t
	}

	count := int32(1)
	if e.Series != nil && e.Series.Count > 0 {
		count = e.Series.Count
	} else if e.Count > 0 {
		count = e.Count
	}

	var firstSeen *time.Time
	if !e.FirstTimestamp.IsZero() {
		t := e.FirstTimestamp.Time
		firstSeen = &t
	} else {
		firstSeen = lastSeen
	}

	eventType := e.Type
	if eventType == "" {
		eventType = "Normal"
	}

	var source *string
	if e.ReportingController != "" {
		source = &e.ReportingController
	} else if e.Source.Component != "" {
		source = &e.Source.Component
	}

	var firstSeenStr gen.OptNilString
	if firstSeen != nil {
		firstSeenStr = gen.NewOptNilString(firstSeen.UTC().Format(time.RFC3339))
	} else {
		firstSeenStr.SetToNull()
	}

	var lastSeenStr gen.OptNilString
	if lastSeen != nil {
		lastSeenStr = gen.NewOptNilString(lastSeen.UTC().Format(time.RFC3339))
	} else {
		lastSeenStr.SetToNull()
	}

	return gen.AppEvent{
		Count:     count,
		FirstSeen: firstSeenStr,
		Kind:      e.InvolvedObject.Kind,
		LastSeen:  lastSeenStr,
		Message:   optNilStrVal(e.Message, e.Message != ""),
		Name:      e.InvolvedObject.Name,
		Reason:    optNilStrVal(e.Reason, e.Reason != ""),
		Source:    optNilStr(source),
		Type:      eventType,
	}
}

func podsOf(projections *projection.Projections, namespace, app, process string) []*projection.PodView {
	pods := projections.PodsOfApp(namespace, app)
	out := make([]*projection.PodView, 0, min(len(pods), maxLogPods))
	for _, p := range pods {
		if process != "" {
			proc, ok := p.Process.Get()
			if !ok || proc != process {
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

type logLine struct {
	Pod     string  `json:"pod"`
	Process *string `json:"process"`
	Time    *string `json:"time"`
	Line    string  `json:"line"`
}

type logEnd struct {
	Pod   *string `json:"pod"`
	Error *string `json:"error"`
}

func equalStringPtr(a, b *string) bool {
	if a == nil && b == nil {
		return true
	}
	if a != nil && b != nil {
		return *a == *b
	}
	return false
}

func writeSSE(w io.Writer, event string, data any) error {
	b, err := json.Marshal(data)
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	buf.WriteString("event: ")
	buf.WriteString(event)
	buf.WriteByte('\n')
	for _, line := range bytes.Split(b, []byte("\n")) {
		buf.WriteString("data: ")
		buf.Write(line)
		buf.WriteByte('\n')
	}
	buf.WriteByte('\n')
	_, err = w.Write(buf.Bytes())
	return err
}

// GetAppEvents lists recent Kubernetes events about the app's objects.
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
		return nil, err //nolint:wrapcheck // a kerr already
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
	var out []gen.AppEvent
	for _, e := range eventsList.Items {
		if belongs(e.InvolvedObject.Kind, e.InvolvedObject.Name, a.app.Slug, own.workloads, own.pods) {
			out = append(out, appEventFrom(e))
		}
	}
	if own.certNamespace != "" && len(own.certNames) > 0 {
		certList, err := cluster.Typed.CoreV1().Events(own.certNamespace).List(ctx, metav1.ListOptions{
			FieldSelector: "involvedObject.kind=Certificate",
		})
		if err != nil {
			s.deps.Logger.Debug("cannot read certificate events", "error", err, "namespace", own.certNamespace)
		} else {
			for _, e := range certList.Items {
				if _, ok := own.certNames[e.InvolvedObject.Name]; ok {
					out = append(out, appEventFrom(e))
				}
			}
		}
	}
	slices.SortFunc(out, func(a, b gen.AppEvent) int {
		aSeen, aOk := a.LastSeen.Get()
		bSeen, bOk := b.LastSeen.Get()
		if aOk && bOk {
			return cmp.Compare(bSeen, aSeen) // newest first
		}
		if bOk {
			return 1
		}
		if aOk {
			return -1
		}
		return 0
	})
	if len(out) > maxEvents {
		out = out[:maxEvents]
	}
	if out == nil {
		out = []gen.AppEvent{}
	}
	res := gen.GetAppEventsOKApplicationJSON(out)
	return &res, nil
}

// GetAppLogs returns recent log lines per pod, or with follow=true streams live lines as SSE.
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
		return nil, err //nolint:wrapcheck // a kerr already
	}
	cluster, err := s.cluster()
	if err != nil {
		return nil, err
	}

	if params.Follow.Or(false) {
		permit, err := s.logStreams.Acquire(acc.Current.User.ID)
		if err != nil {
			return nil, err
		}
		defer permit.Release()

		w, ok := httpx.ResponseWriterFrom(ctx)
		if !ok {
			return nil, kerr.New(kerr.Internal, "response writer not available")
		}
		flusher, ok := w.(http.Flusher)
		if !ok {
			return nil, kerr.New(kerr.Internal, "streaming not supported")
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()

		s.streamLogs(ctx, w, flusher, a, cluster, params)
		return nil, errStreamHandled
	}

	processFilter := params.Process.Or("")
	pods := podsOf(s.deps.Projections, a.app.Namespace, a.app.Slug, processFilter)
	out := make([]gen.PodLogs, len(pods))
	var wg sync.WaitGroup
	tailLines := max(int64(1), min(int64(2000), params.Tail.Or(200)))
	limitBytes := int64(1 << 20)

	for i, pod := range pods {
		wg.Add(1)
		go func(idx int, p *projection.PodView) {
			defer wg.Done()
			container := p.Process.Or("")
			opts := &corev1.PodLogOptions{
				Container:  container,
				Timestamps: true,
				Previous:   params.Previous.Or(false),
				TailLines:  &tailLines,
				LimitBytes: &limitBytes,
			}
			raw, err := cluster.Typed.CoreV1().Pods(a.app.Namespace).GetLogs(p.Name, opts).Do(ctx).Raw()
			var lines []string
			var readErr string
			if err != nil {
				readErr = kubeReadError(err)
				lines = []string{}
			} else {
				scanner := bufio.NewScanner(bytes.NewReader(raw))
				for scanner.Scan() {
					lines = append(lines, scanner.Text())
				}
				if lines == nil {
					lines = []string{}
				}
			}
			item := gen.PodLogs{
				Pod:   p.Name,
				Lines: lines,
			}
			if proc, ok := p.Process.Get(); ok && proc != "" {
				item.Process = gen.NewOptNilString(proc)
			} else {
				item.Process.SetToNull()
			}
			if readErr != "" {
				item.Error = gen.NewOptNilString(readErr)
			} else {
				item.Error.SetToNull()
			}
			out[idx] = item
		}(i, pod)
	}
	wg.Wait()

	res := gen.GetAppLogsOKApplicationJSON(out)
	return &res, nil
}

func (s *Server) streamLogs(
	ctx context.Context,
	w http.ResponseWriter,
	flusher http.Flusher,
	a appScope,
	cluster registry.Cluster,
	params gen.GetAppLogsParams,
) {
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	limitTimer := time.NewTimer(followLimit)
	defer limitTimer.Stop()

	pingTicker := time.NewTicker(15 * time.Second)
	defer pingTicker.Stop()

	rescanTicker := time.NewTicker(rescanInterval)
	defer rescanTicker.Stop()

	type piece struct {
		line *logLine
		end  *logEnd
	}

	pieces := make(chan piece, 64)
	following := make(map[string]context.CancelFunc)
	defer func() {
		for _, c := range following {
			c()
		}
	}()

	type endedInfo struct {
		at  time.Time
		err *string
	}
	ended := make(map[string]endedInfo)
	first := true

	scanPods := func() {
		processFilter := params.Process.Or("")
		currentPods := podsOf(s.deps.Projections, a.app.Namespace, a.app.Slug, processFilter)
		currentPodNames := make(map[string]struct{}, len(currentPods))
		for _, p := range currentPods {
			currentPodNames[p.Name] = struct{}{}
		}
		for name := range ended {
			if _, ok := currentPodNames[name]; !ok {
				delete(ended, name)
			}
		}
		for _, pod := range currentPods {
			if _, ok := following[pod.Name]; ok {
				continue
			}
			podOpts := &corev1.PodLogOptions{
				Follow:     true,
				Timestamps: true,
			}
			if proc, ok := pod.Process.Get(); ok && proc != "" {
				podOpts.Container = proc
			}
			if first {
				t := max(int64(1), min(int64(2000), params.Tail.Or(200)))
				podOpts.TailLines = &t
			} else if prev, ok := ended[pod.Name]; ok {
				elapsed := int64(time.Since(prev.at).Seconds())
				if elapsed < 1 {
					elapsed = 1
				}
				podOpts.SinceSeconds = &elapsed
			}

			podCtx, podCancel := context.WithCancel(streamCtx)
			following[pod.Name] = podCancel

			podName := pod.Name
			var podProc *string
			if proc, ok := pod.Process.Get(); ok && proc != "" {
				podProc = &proc
			}

			go func(name string, proc *string, opts *corev1.PodLogOptions) {
				req := cluster.Typed.CoreV1().Pods(a.app.Namespace).GetLogs(name, opts)
				reader, err := req.Stream(podCtx)
				if err != nil {
					msg := kubeReadError(err)
					select {
					case pieces <- piece{end: &logEnd{Pod: &name, Error: &msg}}:
					case <-podCtx.Done():
					}
					return
				}
				defer reader.Close()

				go func() {
					<-podCtx.Done()
					_ = reader.Close()
				}()

				r := bufio.NewReader(reader)
				var readErr *string
				for {
					raw, err := r.ReadString('\n')
					if len(raw) > 0 {
						raw = strings.TrimRight(raw, "\r\n")
						timeStr, cut := splitLine(raw)
						l := &logLine{
							Pod:     name,
							Process: proc,
							Time:    timeStr,
							Line:    cut,
						}
						select {
						case pieces <- piece{line: l}:
						case <-podCtx.Done():
							return
						}
					}
					if err != nil {
						if !errors.Is(err, io.EOF) && podCtx.Err() == nil {
							s := err.Error()
							readErr = &s
						}
						break
					}
				}
				select {
				case pieces <- piece{end: &logEnd{Pod: &name, Error: readErr}}:
				case <-podCtx.Done():
				}
			}(podName, podProc, podOpts)
		}
		first = false
	}

	scanPods()

	for {
		select {
		case <-ctx.Done():
			return
		case <-limitTimer.C:
			limitMsg := "the stream reached its limit; reconnect to go on"
			_ = writeSSE(w, "end", &logEnd{Pod: nil, Error: &limitMsg})
			flusher.Flush()
			return
		case <-rescanTicker.C:
			scanPods()
		case p := <-pieces:
			if p.line != nil {
				if err := writeSSE(w, "line", p.line); err != nil {
					return
				}
				flusher.Flush()
			} else if p.end != nil {
				name := ""
				if p.end.Pod != nil {
					name = *p.end.Pod
				}
				if cancelPod, ok := following[name]; ok {
					cancelPod()
					delete(following, name)
				}
				prev, hadPrev := ended[name]
				sameErr := hadPrev && equalStringPtr(prev.err, p.end.Error)
				ended[name] = endedInfo{at: time.Now(), err: p.end.Error}
				if !sameErr {
					if err := writeSSE(w, "end", p.end); err != nil {
						return
					}
					flusher.Flush()
				}
			}
		case <-pingTicker.C:
			if _, err := w.Write([]byte(":ping\n\n")); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
