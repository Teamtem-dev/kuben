package apiclient

// An app's builds (2.1): `kuben builds`.

import (
	"context"
	"iter"
	"net/http"
	"strconv"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/opt"
)

// BuildStageDto is one stage of a build (BuildStageDto).
type BuildStageDto struct {
	Name       string          `json:"name"`
	Status     string          `json:"status"`
	StartedAt  opt.Val[int64]  `json:"startedAt"`
	FinishedAt opt.Val[int64]  `json:"finishedAt"`
	Detail     opt.Val[string] `json:"detail"`
}

// BuildDto is one build of an app, as GET …/builds answers it; printed
// again by `kuben builds --json`.
type BuildDto struct {
	ID              string          `json:"id"`
	Attempt         int32           `json:"attempt"`
	Repository      string          `json:"repository"`
	Branch          string          `json:"branch"`
	Commit          string          `json:"commit"`
	Strategy        string          `json:"strategy"`
	Phase           string          `json:"phase"`
	BlockedReason   opt.Val[string] `json:"blockedReason"`
	Failure         opt.Val[string] `json:"failure"`
	FailureDetail   opt.Val[string] `json:"failureDetail"`
	Image           opt.Val[string] `json:"image"`
	Release         opt.Val[string] `json:"release"`
	Deployment      opt.Val[string] `json:"deployment"`
	DeployDecision  opt.Val[string] `json:"deployDecision"`
	CancelRequested bool            `json:"cancelRequested"`
	CreatedAt       int64           `json:"createdAt"`
	StartedAt       opt.Val[int64]  `json:"startedAt"`
	FinishedAt      opt.Val[int64]  `json:"finishedAt"`
	Stages          []BuildStageDto `json:"stages,omitempty"`
}

// TriggeredBuildDto is the answer to a build request.
type TriggeredBuildDto struct {
	SyncOperation string            `json:"syncOperation"`
	Build         opt.Val[BuildDto] `json:"build"`
}

// Builds is the app's newest builds, at most limit.
func (c Client) Builds(ctx context.Context, app AppPath, limit int64) ([]BuildDto, error) {
	return get[[]BuildDto](ctx, c, app.api()+"/builds?limit="+strconv.FormatInt(limit, 10))
}

// TriggerBuild asks for a build of the app's source head now.
func (c Client) TriggerBuild(ctx context.Context, app AppPath) (TriggeredBuildDto, error) {
	return call[TriggeredBuildDto](ctx, c, http.MethodPost, app.api()+"/builds", nil, nil)
}

// CancelBuild stops build.
func (c Client) CancelBuild(ctx context.Context, app AppPath, build string) (BuildDto, error) {
	return call[BuildDto](ctx, c, http.MethodPost, app.api()+"/builds/"+build+"/cancel", nil, nil)
}

// BuildLog is the build's log as text.
func (c Client) BuildLog(ctx context.Context, app AppPath, build string) (string, error) {
	response, err := c.send(ctx, http.MethodGet, app.api()+"/builds/"+build+"/logs", nil, nil)
	if err != nil {
		return "", err
	}
	body, err := read(response)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

// FollowBuildLog is the build's log lines as they are written, until the
// build's log ends; read like FollowLogs.
func (c Client) FollowBuildLog(ctx context.Context, app AppPath, build string) (iter.Seq2[FollowEvent, error], error) {
	return c.follow(ctx, app.api()+"/builds/"+build+"/logs?follow=true")
}
