package httpapi

import (
	"context"
	"io"
	"net/http"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/Teamtem-dev/kuben/internal/core/clock"
	"github.com/Teamtem-dev/kuben/internal/core/ids"
	"github.com/Teamtem-dev/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/internal/kube/projection"
)

// Log internals under test (routes/apps/logs.rs).
var (
	TextLines     = textLines
	UTF8ErrorText = utf8ErrorText
	AppEventFrom  = appEventFrom
	SortEvents    = sortEvents
	PodsOf        = podsOf
	SSEEvent      = sseEvent
	ReadLines     = readLines
)

type (
	LogLine  = logLine
	LogEnd   = logEnd
	LogPiece = logPiece
)

const (
	MaxFollowsPerUser = maxFollowsPerUser
	MaxLine           = maxLine
	SSEPing           = ssePing
	LimitMessage      = limitMessage
)

// OpenFor is how many followed logs user has open, and whether the user
// has an entry at all.
func (s *logStreams) OpenFor(user ids.UserID) (int, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, ok := s.open[user]
	return n, ok
}

// LinePiece is a line read from a pod.
func LinePiece(l LogLine) LogPiece { return pieceLine{line: l} }

// EndPiece is the end of a pod's read.
func EndPiece(pod string, err opt.Val[string]) LogPiece { return pieceEnd{pod: pod, err: err} }

// FollowsLogs is whether r bypasses the request timeout.
func FollowsLogs(s *Server, r *http.Request) bool { return s.followsLogs(r) }

// RunFollow runs the event loop of a followed log with the given waits.
func RunFollow(
	ctx context.Context, w io.Writer, c clock.Clock, limit, rescan, keepAlive time.Duration, tail int64,
	pods func() []*projection.PodView, read func(*projection.PodView, *corev1.PodLogOptions),
	pieces <-chan LogPiece,
) error {
	f := &follower{
		w: w, flush: func() {}, clock: c,
		timing: followTiming{limit: limit, rescan: rescan, keepAlive: keepAlive},
		pods:   pods, read: read, pieces: pieces, tail: tail,
	}
	return f.run(ctx)
}
