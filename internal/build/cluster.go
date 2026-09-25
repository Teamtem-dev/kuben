package build

// The worker's objects in the cluster (worker.rs, `// ---- cluster ----`):
// the BuildRun (dynamic client, the api/v1alpha1 type), and the Job, Secret and
// Pods (typed client-go).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"
	"unicode/utf8"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	utiljson "k8s.io/apimachinery/pkg/util/json"
	"k8s.io/client-go/dynamic"

	"github.com/Teamtem-dev/kuben/api/v1alpha1"
	opbuild "github.com/Teamtem-dev/kuben/internal/core/ops/build"
	"github.com/Teamtem-dev/kuben/internal/core/ops/outcome"
	"github.com/Teamtem-dev/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/internal/store"
)

// buildRunsGVR is the BuildRun resource.
var buildRunsGVR = v1alpha1.SchemeGroupVersion.WithResource(v1alpha1.BuildRunResource) //nolint:gochecknoglobals // a constant

// isStatus reports an API error with one of codes.
func isStatus(err error, codes ...int32) bool {
	var status apierrors.APIStatus
	if !errors.As(err, &status) {
		return false
	}
	code := status.Status().Code
	for _, c := range codes {
		if code == c {
			return true
		}
	}
	return false
}

func isNotFound(err error) bool { return isStatus(err, http.StatusNotFound) }

func isConflict(err error) bool { return isStatus(err, http.StatusConflict) }

func isInvalid(err error) bool {
	return isStatus(err, http.StatusUnprocessableEntity, http.StatusBadRequest)
}

// createError is why the build's objects were not created.
//
//sumtype:decl
type createError interface {
	error
	createError()
}

type (
	// providerFailed: the provider handed out no fetch token.
	providerFailed struct{ err ProviderError }
	// kubeFailed: the API server refused a write or a read.
	kubeFailed struct{ err error }
	// renderFailed: the Job could not be rendered.
	renderFailed struct{ reason string }
)

func (e providerFailed) Error() string { return e.err.Error() }
func (e kubeFailed) Error() string     { return e.err.Error() }
func (e renderFailed) Error() string   { return e.reason }

func (providerFailed) createError() {}
func (kubeFailed) createError()     {}
func (renderFailed) createError()   {}

// deleteNamed deletes name through del in the background; a missing object
// is deleted already.
func deleteNamed(ctx context.Context, del func(context.Context, string, metav1.DeleteOptions) error, name string) error {
	background := metav1.DeletePropagationBackground
	if err := del(ctx, name, metav1.DeleteOptions{PropagationPolicy: &background}); err != nil && !isNotFound(err) {
		return fmt.Errorf("deleting %s: %w", name, err)
	}
	return nil
}

func (w *Worker) deleteJob(ctx context.Context, name string) error {
	return deleteNamed(ctx, w.d.Client.BatchV1().Jobs(w.d.Settings.Namespace).Delete, name)
}

func (w *Worker) buildRuns() dynamic.ResourceInterface {
	return w.d.Dynamic.Resource(buildRunsGVR).Namespace(w.d.Settings.Namespace)
}

// decodeRun is u as a BuildRun.
func decodeRun(u *unstructured.Unstructured) (v1alpha1.BuildRun, error) {
	var run v1alpha1.BuildRun
	data, err := u.MarshalJSON()
	if err == nil {
		err = json.Unmarshal(data, &run)
	}
	if err != nil {
		return run, fmt.Errorf("reading BuildRun %s: %w", u.GetName(), err)
	}
	return run, nil
}

// encodeRun is run as an unstructured object, without the
// `creationTimestamp: null` Go writes and Rust did not.
func encodeRun(run v1alpha1.BuildRun) (*unstructured.Unstructured, error) {
	data, err := json.Marshal(run)
	if err != nil {
		return nil, fmt.Errorf("writing BuildRun %s: %w", run.Name, err)
	}
	var object map[string]any
	if err := utiljson.Unmarshal(data, &object); err != nil {
		return nil, fmt.Errorf("writing BuildRun %s: %w", run.Name, err)
	}
	u := &unstructured.Unstructured{Object: object}
	unstructured.RemoveNestedField(u.Object, "metadata", "creationTimestamp")
	return u, nil
}

// buildRun is attempt's BuildRun as the API server has it, created when
// missing.
func (w *Worker) buildRun(ctx context.Context, attempt store.BuildAttempt) (v1alpha1.BuildRun, error) {
	runs := w.buildRuns()
	name := Name(attempt)
	live, err := runs.Get(ctx, name, metav1.GetOptions{})
	if err == nil {
		return decodeRun(live)
	}
	if !isNotFound(err) {
		return v1alpha1.BuildRun{}, err //nolint:wrapcheck // a Kubernetes API error, classified by the caller
	}
	u, err := encodeRun(RunOf(attempt, w.d.Settings))
	if err != nil {
		return v1alpha1.BuildRun{}, err
	}
	created, err := runs.Create(ctx, u, metav1.CreateOptions{})
	if isConflict(err) {
		created, err = runs.Get(ctx, name, metav1.GetOptions{})
	}
	if err != nil {
		return v1alpha1.BuildRun{}, err //nolint:wrapcheck // a Kubernetes API error, classified by the caller
	}
	return decodeRun(created)
}

// createObjects creates attempt's BuildRun, fetch-token Secret and Job,
// whichever are missing, and returns the Job's name.
func (w *Worker) createObjects(ctx context.Context, attempt store.BuildAttempt) (string, createError) {
	name := Name(attempt)
	run, err := w.buildRun(ctx, attempt)
	if err != nil {
		return "", kubeFailed{err}
	}
	jobs := w.d.Client.BatchV1().Jobs(w.d.Settings.Namespace)
	if _, err := jobs.Get(ctx, name, metav1.GetOptions{}); err == nil {
		return name, nil
	} else if !isNotFound(err) {
		return "", kubeFailed{err}
	}
	token, err := w.d.Provider.FetchToken(ctx, attempt.InstallationID, attempt.Repository)
	if err != nil {
		var pe ProviderError
		if !errors.As(err, &pe) {
			pe = Unavailable{Reason: err.Error()}
		}
		return "", providerFailed{pe}
	}
	secrets := w.d.Client.CoreV1().Secrets(w.d.Settings.Namespace)
	secretName := SecretName(attempt)
	if _, err := secrets.Get(ctx, secretName, metav1.GetOptions{}); err == nil {
		// Left by an interrupted attempt to create the Job: replace it.
		w.revoke(ctx, attempt)
		if err := deleteNamed(ctx, secrets.Delete, secretName); err != nil {
			return "", kubeFailed{err}
		}
	} else if !isNotFound(err) {
		return "", kubeFailed{err}
	}
	if _, err := secrets.Create(ctx, SourceSecret(attempt, w.d.Settings, &run, token.Token), metav1.CreateOptions{}); err != nil {
		w.revokeToken(ctx, token)
		return "", kubeFailed{err}
	}
	job, err := Job(attempt, w.d.Settings, &run, w.d.Provider.CloneURL(attempt.Repository))
	if err != nil {
		return "", renderFailed{err.Error()}
	}
	if _, err := jobs.Create(ctx, job, metav1.CreateOptions{}); err != nil && !isConflict(err) {
		return "", kubeFailed{err}
	}
	w.d.Logger.Info("build job created", "build", attempt.ID.String(), "job", name, "commit", attempt.Commit.String())
	return name, nil
}

// observeJob is what attempt's Job and its newest pod show now.
func (w *Worker) observeJob(ctx context.Context, attempt store.BuildAttempt) (outcome.JobObservation, error) {
	found := opt.None[*batchv1.Job]()
	job, err := w.d.Client.BatchV1().Jobs(w.d.Settings.Namespace).Get(ctx, Name(attempt), metav1.GetOptions{})
	switch {
	case err == nil:
		found = opt.Some(job)
	case !isNotFound(err):
		return outcome.JobObservation{}, fmt.Errorf("reading the build Job: %w", err)
	}
	var pods []corev1.Pod
	if found.IsSome() {
		listed, err := w.d.Client.CoreV1().Pods(w.d.Settings.Namespace).List(ctx, metav1.ListOptions{
			LabelSelector: AttemptLabel + "=" + attempt.ID.String(),
		})
		if err != nil {
			return outcome.JobObservation{}, fmt.Errorf("listing the build pods: %w", err)
		}
		pods = listed.Items
	}
	return ObserveJob(found, pods, w.d.Clock.NowMs()), nil
}

// cleanup revokes the fetch token, deletes the Job and the Secret, and
// leaves the final status on the BuildRun, which [Worker.Sweep] removes
// after [Retention].
func (w *Worker) cleanup(ctx context.Context, attempt store.BuildAttempt, phase opbuild.Phase, digest opt.Val[string]) error {
	w.revoke(ctx, attempt)
	if err := w.deleteJob(ctx, Name(attempt)); err != nil {
		return err
	}
	if err := deleteNamed(ctx, w.d.Client.CoreV1().Secrets(w.d.Settings.Namespace).Delete, SecretName(attempt)); err != nil {
		return err
	}
	w.mirror(ctx, attempt, phase, digest)
	return nil
}

// Sweep deletes the BuildRuns that finished more than [Retention] ago and
// says how many.
func (w *Worker) Sweep(ctx context.Context) (int, error) {
	runs := w.buildRuns()
	list, err := runs.List(ctx, metav1.ListOptions{
		LabelSelector: v1alpha1.LabelManagedBy + "=" + v1alpha1.LabelManagerValue,
	})
	if err != nil {
		return 0, fmt.Errorf("listing the BuildRuns: %w", err)
	}
	now := w.d.Clock.NowMs()
	removed := 0
	for i := range list.Items {
		run, err := decodeRun(&list.Items[i])
		if err != nil || !expired(run, now) {
			continue
		}
		del := func(ctx context.Context, name string, o metav1.DeleteOptions) error { return runs.Delete(ctx, name, o) }
		if err := deleteNamed(ctx, del, run.Name); err != nil {
			return removed, err
		}
		removed++
	}
	return removed, nil
}

// revoke revokes the fetch token in attempt's Secret, if it is there.
func (w *Worker) revoke(ctx context.Context, attempt store.BuildAttempt) {
	secret, err := w.d.Client.CoreV1().Secrets(w.d.Settings.Namespace).Get(ctx, SecretName(attempt), metav1.GetOptions{})
	if isNotFound(err) {
		return
	}
	if err != nil {
		w.d.Logger.Warn("cannot read the fetch token to revoke it", "build", attempt.ID.String(), "error", err)
		return
	}
	if token, ok := secret.Data[TokenKey]; ok && utf8.Valid(token) {
		w.revokeToken(ctx, FetchToken{Token: string(token), ExpiresAt: w.d.Clock.NowMs()})
	}
}

func (w *Worker) revokeToken(ctx context.Context, token FetchToken) {
	// An unrevoked token still expires within the hour.
	if err := w.d.Provider.Revoke(ctx, token); err != nil {
		w.d.Logger.Warn("a fetch token could not be revoked; it expires on its own", "error", err)
	}
}

// mirror shows the attempt's progress on its BuildRun; best effort.
func (w *Worker) mirror(ctx context.Context, attempt store.BuildAttempt, phase opbuild.Phase, digest opt.Val[string]) {
	data, err := json.Marshal(statusPatch(attempt, phase, digest, w.d.Clock.NowMs()))
	if err == nil {
		_, err = w.buildRuns().Patch(ctx, Name(attempt), types.MergePatchType, data,
			metav1.PatchOptions{FieldManager: fieldManager}, "status")
	}
	if err != nil && !isNotFound(err) {
		w.d.Logger.Debug("BuildRun status not updated", "build", attempt.ID.String(), "error", err)
	}
}

// crdPhase is the phase BuildRun.status.phase shows.
func crdPhase(phase opbuild.Phase) string {
	switch phase {
	case opbuild.Queued, opbuild.Blocked:
		return "Queued"
	case opbuild.Succeeded:
		return "Succeeded"
	case opbuild.Failed:
		return "Failed"
	case opbuild.Cancelled:
		return "Cancelled"
	case opbuild.Preparing, opbuild.Running, opbuild.Publishing, opbuild.VerifyingOutput, opbuild.CancelRequested,
		opbuild.Cancelling:
	}
	return "Running"
}

// rfc3339 is nowMs as jiff printed a Timestamp: UTC, fractions only when
// there are any.
func rfc3339(nowMs int64) string { return time.UnixMilli(nowMs).UTC().Format(time.RFC3339Nano) }

// statusPatch is the BuildRun status for attempt in phase at nowMs: the
// phase, the verified digest and, once final, when it finished and why.
func statusPatch(attempt store.BuildAttempt, phase opbuild.Phase, digest opt.Val[string], nowMs int64) map[string]any {
	now := rfc3339(nowMs)
	kind, reason, ok := "Progressing", string(phase), true
	switch phase {
	case opbuild.Succeeded:
		kind, reason = "Succeeded", "Verified"
	case opbuild.Failed:
		kind, reason, ok = "Succeeded", attempt.Failure.Or("Failed"), false
	case opbuild.Cancelled:
		kind, reason, ok = "Succeeded", "Cancelled", false
	case opbuild.Queued, opbuild.Blocked, opbuild.Preparing, opbuild.Running, opbuild.Publishing,
		opbuild.VerifyingOutput, opbuild.CancelRequested, opbuild.Cancelling:
	}
	conditionStatus := "False"
	if ok {
		conditionStatus = "True"
	}
	var message any
	if m, ok := attempt.FailureDetail.Get(); ok {
		message = m
	}
	status := map[string]any{
		"phase":   crdPhase(phase),
		"jobName": Name(attempt),
		"conditions": []any{map[string]any{
			"type": kind, "status": conditionStatus, "reason": reason, "message": message, "lastTransitionTime": now,
		}},
	}
	if d, ok := digest.Get(); ok {
		status["imageDigest"] = d
	}
	if phase == opbuild.Preparing {
		status["startedAt"] = now
	}
	if phase.IsTerminal() {
		status["finishedAt"] = now
	}
	return map[string]any{"status": status}
}

// expired reports whether run finished more than [Retention] before nowMs.
func expired(run v1alpha1.BuildRun, nowMs int64) bool {
	if run.Status == nil || run.Status.FinishedAt == nil {
		return false
	}
	finished, err := time.Parse(time.RFC3339Nano, *run.Status.FinishedAt)
	if err != nil {
		return false
	}
	return nowMs/1000-finished.Unix() > retentionSecs
}
