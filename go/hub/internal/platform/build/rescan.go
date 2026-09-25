package build

// Rescans of the images apps run (M4.6; plan §18.1; rescan.rs): an image
// scanned yesterday may have a new finding today. Every image a live target
// runs whose newest scan is older than the interval is scanned again with a
// fresh feed, by a scan-only Job in the build namespace with the same
// budgets and isolation as a build (no service-account token, no source).
// A rescan that cannot run is recorded as unavailable, which also waits for
// the next interval. New findings gate future deployments; running apps
// are never stopped by them.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/Teamtem-dev/kuben/go/hub/internal/core/clock"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/ids"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/scan"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/health"
	"github.com/Teamtem-dev/kuben/go/hub/internal/store"
	"github.com/Teamtem-dev/kuben/go/kubenapi/v1alpha1"
)

const (
	// RescanSubsystem is the health entry of the rescan loop.
	RescanSubsystem = "rescans"
	// RescanLabel labels a rescan Job and its pod.
	RescanLabel = "kuben.dev/rescan"
	// rescanBatch is the images rescanned per organization per pass.
	rescanBatch = 10
	// passEvery is the pause between passes.
	passEvery = 30 * time.Minute
	// rescanPoll is the pause between reads of a rescan Job.
	rescanPoll = 5 * time.Second
	// rescanDeadline is the longest one rescan may take.
	rescanDeadline = 15 * time.Minute
	// rescanTTL is how long Kubernetes keeps a finished rescan Job.
	rescanTTL = 3600
)

// ErrScanningOff says that no scanner image is configured.
var ErrScanningOff = errors.New("scanning is not configured")

// Rescanner rescans the running images of every organization.
type Rescanner struct {
	Store    *store.Store
	Client   kubernetes.Interface
	Settings Settings
	// Interval is how old the newest scan of an image may be.
	Interval time.Duration
	Clock    clock.Clock
	Logger   *slog.Logger
}

// RescanJobName is the deterministic Job name of a rescan of digest.
func RescanJobName(digest string) string {
	sum := sha256.Sum256([]byte(digest))
	return "kscan-" + hex.EncodeToString(sum[:8])
}

// RescanJob is the Job that scans repository@digest; [ErrScanningOff]
// without a scanner.
func RescanJob(s Settings, repository, digest string) (*batchv1.Job, error) {
	q := &quantities{}
	scanner, ok := scanContainer(q, s, repository, opt.Some(digest)).Get()
	if !ok {
		return nil, ErrScanningOff
	}
	name := RescanJobName(digest)
	jobLabels := map[string]string{RescanLabel: name, v1alpha1.LabelManagedBy: v1alpha1.LabelManagerValue}
	job := &batchv1.Job{
		TypeMeta:   metav1.TypeMeta{APIVersion: "batch/v1", Kind: "Job"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: s.Namespace, Labels: jobLabels},
		Spec: batchv1.JobSpec{
			BackoffLimit:            new(int32(0)),
			ActiveDeadlineSeconds:   new(int64(rescanDeadline / time.Second)),
			TTLSecondsAfterFinished: new(int32(rescanTTL)),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: jobLabels},
				Spec: corev1.PodSpec{
					RestartPolicy:                corev1.RestartPolicyNever,
					AutomountServiceAccountToken: new(false),
					EnableServiceLinks:           new(false),
					SecurityContext:              podSecurity(),
					Containers:                   []corev1.Container{scanner},
					Volumes:                      workspaceVolumes(q, s),
				},
			},
		},
	}
	if q.err != nil {
		return nil, q.err
	}
	return job, nil
}

// RunRescans runs r until ctx ends.
func RunRescans(ctx context.Context, r *Rescanner, h *health.Health) error {
	h.OK(RescanSubsystem)
	for ctx.Err() == nil {
		if _, err := r.Pass(ctx); err != nil {
			r.Logger.Warn("a rescan pass failed", "error", err)
			h.Degrade(RescanSubsystem, err.Error())
		} else {
			h.OK(RescanSubsystem)
		}
		if !sleep(ctx, passEvery) {
			return nil
		}
	}
	return nil
}

// Pass rescans what is due in every organization, one image at a time,
// and says how many images it rescanned.
func (r *Rescanner) Pass(ctx context.Context) (int, error) {
	before := clock.SaturatingSub(r.Clock.NowMs(), r.Interval.Milliseconds())
	orgs, err := r.Store.OrgIDs(ctx)
	if err != nil {
		return 0, err //nolint:wrapcheck // a store error naming its operation
	}
	done := 0
	for _, org := range orgs {
		due, err := r.due(ctx, org, before)
		if err != nil {
			return done, err
		}
		for _, image := range due {
			select {
			case <-ctx.Done():
				return done, nil
			default:
			}
			if err := r.rescan(ctx, org, image); err != nil {
				return done, err
			}
			done++
		}
	}
	if done > 0 {
		r.Logger.Info("running images rescanned", "images", done)
	}
	return done, nil
}

func (r *Rescanner) due(ctx context.Context, org ids.OrgID, before int64) ([]store.ImageRef, error) {
	t, err := r.Store.Tenant(ctx, org)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error naming its operation
	}
	defer t.Rollback(ctx)                        //nolint:errcheck // read only
	return t.RescanDue(ctx, before, rescanBatch) //nolint:wrapcheck // a store error naming its operation
}

// rescan scans one image and records the scan.
func (r *Rescanner) rescan(ctx context.Context, org ids.OrgID, image store.ImageRef) error {
	jobs := r.Client.BatchV1().Jobs(r.Settings.Namespace)
	name := RescanJobName(image.Digest)
	job, err := RescanJob(r.Settings, image.Repository, image.Digest)
	if err != nil {
		return err
	}
	// A 409: another replica or a previous pass started it; follow it.
	if _, err := jobs.Create(ctx, job, metav1.CreateOptions{}); err != nil && !isConflict(err) {
		return fmt.Errorf("creating the rescan Job %s: %w", name, err)
	}
	report, sbom, finished, err := r.follow(ctx, name)
	if err != nil || !finished {
		return err
	}
	summary := SummaryOf(report, r.Clock.NowMs())
	scanned := store.NewScan{
		Repository: image.Repository, Digest: image.Digest, Summary: summary, Detail: detailOf(report),
	}
	if err := keepScan(ctx, r.Store, org, scanned, sbom); err != nil {
		return err
	}
	if err := deleteNamed(ctx, jobs.Delete, name); err != nil {
		return err
	}
	r.Logger.Info("image rescanned", "org", org.String(), "digest", image.Digest, "status", string(summary.Status),
		"critical", summary.Counts.Critical, "high", summary.Counts.High)
	return nil
}

// follow waits until the rescan Job name finished (or is gone) and
// collects what it left; false when ctx ended first.
func (r *Rescanner) follow(ctx context.Context, name string) (scan.Report, opt.Val[[]byte], bool, error) {
	none := opt.None[[]byte]()
	jobs := r.Client.BatchV1().Jobs(r.Settings.Namespace)
	started := r.Clock.NowMs()
	for {
		live, err := jobs.Get(ctx, name, metav1.GetOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return scan.Report{}, none, false, fmt.Errorf("reading the rescan Job %s: %w", name, err)
		}
		if err != nil || jobEnded(live) {
			pods := r.Client.CoreV1().Pods(r.Settings.Namespace)
			report, sbom, err := Collect(ctx, pods, RescanLabel+"="+name, r.Logger)
			return report, sbom, err == nil, err
		}
		if time.Duration(r.Clock.NowMs()-started)*time.Millisecond > rescanDeadline+rescanPoll {
			return scan.Unavailable("the rescan did not finish"), none, true, nil
		}
		if !sleep(ctx, rescanPoll) {
			return scan.Report{}, none, false, nil
		}
	}
}

// jobEnded reports a Complete or Failed condition.
func jobEnded(job *batchv1.Job) bool {
	for _, c := range job.Status.Conditions {
		if (c.Type == batchv1.JobComplete || c.Type == batchv1.JobFailed) && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// keepScan records scanned and its SBOM in org.
func keepScan(ctx context.Context, st *store.Store, org ids.OrgID, scanned store.NewScan, sbom opt.Val[[]byte]) error {
	t, err := st.Tenant(ctx, org)
	if err != nil {
		return err //nolint:wrapcheck // a store error naming its operation
	}
	defer t.Rollback(ctx) //nolint:errcheck // a no-op after the commit
	if _, err := t.RecordScan(ctx, scanned); err != nil {
		return err //nolint:wrapcheck // a store error naming its operation
	}
	if content, ok := sbom.Get(); ok {
		if _, err := t.PutSbom(ctx, scanned.Digest, content); err != nil {
			return err //nolint:wrapcheck // a store error naming its operation
		}
	}
	return t.Commit(ctx) //nolint:wrapcheck // a store error naming its operation
}
