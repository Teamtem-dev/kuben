package store

// What a build leaves besides its image (2.1, migration 0035): the tail of
// its pod's log, kept when the attempt settles so it outlives the pod
// sweep, and the timings of its stages.

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/ids"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/opt"
)

// MaxBuildLogTail is the most of a build's log the store keeps, in bytes;
// the server keeps less (build.logTailKiB).
const MaxBuildLogTail = 1 << 20

// BuildStageName is one stage of a build.
type BuildStageName string

// The stages of a build, in order.
const (
	StageClone  BuildStageName = "clone"
	StagePlan   BuildStageName = "plan"
	StageBuild  BuildStageName = "build"
	StageScan   BuildStageName = "scan"
	StagePush   BuildStageName = "push"
	StageDeploy BuildStageName = "deploy"
)

// BuildStageStatus is how far a stage got.
type BuildStageStatus string

// The statuses of a stage.
const (
	StagePending   BuildStageStatus = "pending"
	StageRunning   BuildStageStatus = "running"
	StageSucceeded BuildStageStatus = "succeeded"
	StageFailed    BuildStageStatus = "failed"
	StageSkipped   BuildStageStatus = "skipped"
)

var (
	stageNames    = []BuildStageName{StageClone, StagePlan, StageBuild, StageScan, StagePush, StageDeploy}
	stageStatuses = []BuildStageStatus{StagePending, StageRunning, StageSucceeded, StageFailed, StageSkipped}
)

// BuildStage is one stage of a build as stored: its JSON is the API's
// BuildStageDto.
type BuildStage struct {
	Name       BuildStageName   `json:"name"`
	Status     BuildStageStatus `json:"status"`
	StartedAt  opt.Val[int64]   `json:"startedAt,omitzero"`
	FinishedAt opt.Val[int64]   `json:"finishedAt,omitzero"`
	// Detail is at most 2048 bytes when stored.
	Detail opt.Val[string] `json:"detail,omitzero"`
}

// valid reports whether the stage's name and status are known ones.
func (s BuildStage) valid() bool {
	return slices.Contains(stageNames, s.Name) && slices.Contains(stageStatuses, s.Status)
}

// stagesOf reads a stored stages column; nil when none were recorded.
func stagesOf(op string, text *string) ([]BuildStage, error) {
	if text == nil {
		return nil, nil
	}
	var stages []BuildStage
	if err := json.Unmarshal([]byte(*text), &stages); err != nil {
		return nil, decodeErr(op, "%v", err)
	}
	for _, s := range stages {
		if !s.valid() {
			return nil, decodeErr(op, "unknown build stage %s/%s", rustQuote(string(s.Name)), rustQuote(string(s.Status)))
		}
	}
	return stages, nil
}

// SetBuildStages records the stages of build, replacing those recorded.
func (t *Tenant) SetBuildStages(ctx context.Context, build ids.BuildAttemptID, stages []BuildStage) (bool, error) {
	const op = "record a build's stages"
	stored := make([]BuildStage, 0, len(stages))
	for _, s := range stages {
		if !s.valid() {
			return false, DatabaseError{Op: op, Err: fmt.Errorf("unknown build stage %q/%q", s.Name, s.Status)}
		}
		if d, ok := s.Detail.Get(); ok {
			s.Detail = opt.Some(bounded(d))
		}
		stored = append(stored, s)
	}
	text, err := json.Marshal(stored)
	if err != nil {
		return false, dbErr(op, err)
	}
	n, err := exec(ctx, t.tx, op, setStages, build, t.org.String(), string(text), t.store.now())
	return n == 1, err
}

// SetBuildLogTail keeps the end of build's log: at most [MaxBuildLogTail]
// bytes, cut on a character boundary. False when the build is unknown.
func (t *Tenant) SetBuildLogTail(ctx context.Context, build ids.BuildAttemptID, log string) (bool, error) {
	n, err := exec(ctx, t.tx, "keep a build's log", setLogTail, build, t.org.String(), logTail(log, MaxBuildLogTail),
		t.store.now())
	return n == 1, err
}

// BuildLogTail is the kept end of build's log; none when the build is
// unknown or kept none (yet).
func (t *Tenant) BuildLogTail(ctx context.Context, build ids.BuildAttemptID) (opt.Val[string], error) {
	tail, found, err := queryOpt(ctx, t.tx, "read a build's log", logTailOf, func(row pgx.CollectableRow) (*string, error) {
		var s *string
		err := row.Scan(&s)
		return s, err
	}, build, t.org.String())
	if err != nil || !found {
		return opt.None[string](), err
	}
	return opt.FromPtr(tail), nil
}

// logTail is the last max bytes of log at most, starting on a character.
func logTail(log string, maxBytes int) string {
	if len(log) <= maxBytes {
		return log
	}
	start := len(log) - maxBytes
	for start < len(log) && !utf8.RuneStart(log[start]) {
		start++
	}
	return log[start:]
}
