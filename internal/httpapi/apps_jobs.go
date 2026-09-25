package api

// Scheduled runs: "run now" for a scheduled process (scenario 7,
// routes/apps/jobs.rs).

import (
	"context"
	"fmt"
	"slices"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/Teamtem-dev/kuben/internal/core/kerrors"
	"github.com/Teamtem-dev/kuben/internal/core/perm"
	"github.com/Teamtem-dev/kuben/internal/httpapi/gen"
	"github.com/Teamtem-dev/kuben/internal/kube/render"
	"github.com/Teamtem-dev/kuben/internal/store"
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

// unixSeconds is nowMs in whole seconds; a time before the epoch is 0, as
// Rust's `duration_since(UNIX_EPOCH).map_or(0, ..)` made it.
func unixSeconds(nowMs int64) uint64 {
	if nowMs < 0 {
		return 0
	}
	return uint64(nowMs / 1000) //nolint:gosec // G115: not negative, checked above
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
		return nil, err //nolint:wrapcheck // a kerrors already
	}
	process, err := processToRun(req, a.app)
	if err != nil {
		return nil, err
	}
	cluster, err := s.cluster()
	if err != nil {
		return nil, err
	}
	name, err := startManualJob(ctx, cluster.Typed, a.app.Namespace,
		render.WorkloadName(a.app.Slug, process), s.deps.Clock.NowMs())
	if err != nil {
		return nil, err
	}
	return &gen.JobStarted{Job: name}, nil
}

// processToRun is the process req names, used as given even when empty (the
// CronJob lookup then answers 404, as in Rust); without one, the first
// scheduled process of app in name order.
func processToRun(req *gen.RunJob, app store.AppRecord) (string, error) {
	if req != nil {
		if p, ok := req.Process.Get(); ok {
			return p, nil
		}
	}
	noProcess := kerrors.New(kerrors.Validation, "this app has no scheduled process")
	spec, ok := desiredSpec(app)
	if !ok {
		return "", noProcess
	}
	names := make([]string, 0, len(spec.Runtime.Processes))
	for name := range spec.Runtime.Processes {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		if spec.Runtime.Processes[name].Schedule != nil {
			return name, nil
		}
	}
	return "", noProcess
}

// startManualJob creates a Job from the CronJob cronName in namespace,
// named after the time nowMs, and returns its name.
func startManualJob(ctx context.Context, typed kubernetes.Interface, namespace, cronName string, nowMs int64) (string, error) {
	cron, err := typed.BatchV1().CronJobs(namespace).Get(ctx, cronName, metav1.GetOptions{})
	if err != nil {
		return "", kubeError(err, cronName)
	}
	name := manualJobName(cronName, unixSeconds(nowMs))
	job := render.JobFromCron(cron, name)
	if _, err := typed.BatchV1().Jobs(namespace).Create(ctx, &job, metav1.CreateOptions{}); err != nil {
		return "", kubeError(err, name)
	}
	return name, nil
}
