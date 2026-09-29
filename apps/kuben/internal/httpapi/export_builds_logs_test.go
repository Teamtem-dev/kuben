package httpapi

import (
	"context"
	"io"
	"time"

	"golang.org/x/sync/errgroup"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/ids"
)

// BuildLogOf is where a test's build log is read from: pods (nil without a
// cluster), whether the attempt settled, and its kept tail.
type BuildLogOf struct {
	Pods    corev1client.PodInterface
	Attempt ids.BuildAttemptID
	Job     string
	Settled bool
	Tail    string
}

func (b BuildLogOf) source() buildLogSource {
	return buildLogSource{
		pods: b.Pods, attempt: b.Attempt, job: b.Job, poll: time.Millisecond,
		settled: func(context.Context) (bool, error) { return b.Settled, nil },
		tail:    func(context.Context) (string, error) { return b.Tail, nil },
	}
}

// ReadBuildLog is the one-shot read of b.
func ReadBuildLog(ctx context.Context, b BuildLogOf) (string, error) { return b.source().once(ctx) }

// FollowBuildLog writes the followed log of b to w until it ends.
func FollowBuildLog(ctx context.Context, w io.Writer, b BuildLogOf) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	g, readCtx := errgroup.WithContext(ctx)
	pieces := make(chan logPiece)
	g.Go(func() error {
		b.source().stream(readCtx, pieces)
		return nil
	})
	err := writeBuildEvents(ctx, w, func() {}, pieces, followTiming{limit: time.Minute, rescan: time.Millisecond, keepAlive: time.Minute})
	cancel()
	if waitErr := g.Wait(); waitErr != nil {
		return waitErr
	}
	return err
}
