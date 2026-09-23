package api

// Scheduled runs: "run now" for a scheduled process (scenario 7,
// routes/apps/jobs.rs).

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Teamtem-dev/kuben/go/hub/internal/api/gen"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/kerr"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/perm"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/render"
)

// manualJobName is apps/jobs.rs manual_job_name: name for a manual run,
// within the 63-character limit of Job names.
func manualJobName(cron string, unixSecs uint64) string {
	suffix := fmt.Sprintf("-run-%d", unixSecs)
	keep := max(0, 63-len(suffix))
	var base string
	if len(cron) > keep {
		base = cron[:keep]
	} else {
		base = cron
	}
	return strings.TrimRight(base, "-") + suffix
}

// RunApp runs a scheduled process now (a Job from its CronJob template).
func (s *Server) RunApp(ctx context.Context, req *gen.RunJob, params gen.RunAppParams) (gen.RunAppRes, error) {
	acc, err := s.access(ctx)
	if err != nil {
		return nil, err
	}
	a, err := s.findApp(ctx, acc, params.Project, params.Environment, params.App)
	if err != nil {
		return nil, err
	}
	if _, err := acc.Require(perm.AppDeploy, a.chain()); err != nil {
		return nil, err //nolint:wrapcheck // a kerr already
	}
	var process string
	if req != nil {
		if p, ok := req.Process.Get(); ok && p != "" {
			process = p
		}
	}
	if process == "" {
		spec, ok := desiredSpec(a.app)
		if !ok {
			return nil, kerr.New(kerr.Validation, "this app has no scheduled process")
		}
		names := make([]string, 0, len(spec.Runtime.Processes))
		for name := range spec.Runtime.Processes {
			names = append(names, name)
		}
		slices.Sort(names)
		for _, name := range names {
			if spec.Runtime.Processes[name].Schedule != nil {
				process = name
				break
			}
		}
		if process == "" {
			return nil, kerr.New(kerr.Validation, "this app has no scheduled process")
		}
	}
	cluster, err := s.cluster()
	if err != nil {
		return nil, err
	}
	cronName := render.WorkloadName(a.app.Slug, process)
	cron, err := cluster.Typed.BatchV1().CronJobs(a.app.Namespace).Get(ctx, cronName, metav1.GetOptions{})
	if err != nil {
		return nil, kubeError(err, cronName)
	}
	now := uint64(time.Now().Unix())
	name := manualJobName(cronName, now)
	job := render.JobFromCron(cron, name)
	if _, err := cluster.Typed.BatchV1().Jobs(a.app.Namespace).Create(ctx, &job, metav1.CreateOptions{}); err != nil {
		return nil, kubeError(err, name)
	}
	return &gen.JobStarted{Job: name}, nil
}
