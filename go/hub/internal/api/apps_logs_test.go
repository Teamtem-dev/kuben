package api_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Teamtem-dev/kuben/go/hub/internal/api"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/ids"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/kerr"
)

func TestAnAppsObjectsAreToldApartFromItsNeighbours(t *testing.T) {
	workloads := []string{"web-web", "web-nightly"}
	pods := map[string]struct{}{"web-web-7d9c-x2x9q": {}}
	own := func(kind, name string) bool {
		return api.Belongs(kind, name, "web", workloads, pods)
	}
	assert.True(t, own("App", "web"))
	assert.True(t, own("HTTPRoute", "web"))
	assert.False(t, own("HTTPRoute", "web-api"), "another app's route")
	assert.True(t, own("Deployment", "web-web"))
	assert.False(t, own("Deployment", "web-web-2"))
	assert.True(t, own("ReplicaSet", "web-web-7d9c5f"))
	assert.True(t, own("Pod", "web-web-7d9c-x2x9q"))
	assert.True(t, own("Pod", "web-web-7d9c5f-abcde"), "a pod already gone")
	assert.True(t, own("CronJob", "web-nightly"))
	assert.True(t, own("Job", "web-nightly-29310240"))
	assert.True(t, own("Job", "web-nightly-run-1757548800"))
	assert.True(t, own("Pod", "web-nightly-run-1757548800-k2j4d"))
	assert.False(t, own("Pod", "api-web-7d9c5f-abcde"))
	assert.False(t, own("Pod", "web-web-a-b-c-d"))
	assert.False(t, own("Service", "api"))
}

func TestAFollowedLogLineKeepsItsTimeAndIsCutOnACharacter(t *testing.T) {
	timeStr, line := api.SplitLine("2026-09-16T10:00:00.123456789Z hello world")
	require.NotNil(t, timeStr)
	assert.Equal(t, "2026-09-16T10:00:00.123456789Z", *timeStr)
	assert.Equal(t, "hello world", line)

	timeStr, line = api.SplitLine("no time here")
	assert.Nil(t, timeStr)
	assert.Equal(t, "no time here", line)

	const maxLine = 16 * 1024
	long := fmt.Sprintf("2026-09-16T10:00:00Z %s", strings.Repeat("é", maxLine))
	_, cut := api.SplitLine(long)
	assert.LessOrEqual(t, len(cut), maxLine)
	for _, r := range cut {
		assert.Equal(t, 'é', r)
	}
}

func TestFollowedLogsAreCappedPerUserAndFreedWhenClosed(t *testing.T) {
	streams := api.NewLogStreams()
	alice, bob := ids.New[ids.User](), ids.New[ids.User]()
	held := make([]*api.LogStreamPermit, 0, 4)
	for i := 0; i < 4; i++ {
		permit, err := streams.Acquire(alice)
		require.NoError(t, err, "a place")
		held = append(held, permit)
	}
	_, err := streams.Acquire(alice)
	require.Error(t, err)
	var kerrErr *kerr.Error
	require.True(t, errors.As(err, &kerrErr))
	assert.Equal(t, kerr.RateLimited, kerrErr.Code)

	other, err := streams.Acquire(bob)
	require.NoError(t, err, "others are not affected")

	for _, p := range held {
		p.Release()
	}
	other.Release()

	assert.Zero(t, streams.Count())

	permit, err := streams.Acquire(alice)
	require.NoError(t, err, "free again")
	permit.Release()
}
