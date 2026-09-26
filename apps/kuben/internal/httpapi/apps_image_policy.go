package httpapi

// An app's image update policy (M5.4): routes/apps/image_policy.rs.

import (
	"context"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/imagepolicy"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/kerrors"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/perm"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/httpapi/gen"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/integrations/oci"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/store"
)

// The defaults of an unset `enabled` and `intervalSecs`.
const (
	defaultPolicyEnabled  = true
	defaultPolicyInterval = 300
)

func imagePolicyDto(p store.ImagePolicy) *gen.ImagePolicyDto {
	checked := optNilString(opt.None[string]())
	if at, ok := p.LastCheckedAt.Get(); ok {
		checked = gen.NewOptNilString(Timestamp(at))
	}
	run := nullUUID()
	if r, ok := p.LastRun.Get(); ok {
		run = gen.NewOptNilUUID(r)
	}
	return &gen.ImagePolicyDto{
		Repository: p.Repository, Pattern: p.Pattern, Enabled: p.Enabled, IntervalSecs: p.IntervalSecs,
		NextCheckAt: Timestamp(p.NextCheckAt), LastCheckedAt: checked, LastTag: optNilString(p.LastTag),
		LastDigest: optNilString(p.LastDigest), LastError: optNilString(p.LastError), Failures: p.Failures,
		LastRun: run,
	}
}

// GetImagePolicy is an app's image update policy.
func (s *Server) GetImagePolicy(ctx context.Context, params gen.GetImagePolicyParams) (gen.GetImagePolicyRes, error) {
	_, a, t, err := s.appFor(ctx, perm.AppRead, params.Project, params.Environment, params.App)
	if err != nil {
		return nil, err
	}
	defer t.Rollback(ctx) //nolint:errcheck // read only
	policy, found, err := t.ImagePolicy(ctx, a.app.Target)
	switch {
	case err != nil:
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	case !found:
		return nil, kerrors.New(kerrors.NotFound, "an image policy of `%s`", params.App)
	}
	return imagePolicyDto(policy), nil
}

// PutImagePolicy follows a tag pattern of the app's image repository: a new
// digest is deployed (with approval where the environment asks for it).
func (s *Server) PutImagePolicy(ctx context.Context, req *gen.PutImagePolicy, params gen.PutImagePolicyParams) (gen.PutImagePolicyRes, error) {
	acc, a, t, err := s.appFor(ctx, perm.AppDeploy, params.Project, params.Environment, params.App)
	if err != nil {
		return nil, err
	}
	defer t.Rollback(ctx) //nolint:errcheck // committed on success
	pattern, err := imagepolicy.Parse(req.Pattern)
	if err != nil {
		return nil, kerrors.New(kerrors.Validation, "%s", err)
	}
	interval := req.IntervalSecs.Or(defaultPolicyInterval)
	if interval < int32(imagepolicy.MinIntervalSecs) || interval > int32(imagepolicy.MaxIntervalSecs) {
		return nil, kerrors.New(kerrors.Validation, "intervalSecs must be %d to %d", imagepolicy.MinIntervalSecs, imagepolicy.MaxIntervalSecs)
	}
	given, ok := req.Repository.Get()
	if !ok {
		if given, ok = a.app.Image.Get(); !ok {
			return nil, kerrors.New(kerrors.Validation, "the app has no image yet: name the repository")
		}
	}
	reference, err := oci.Parse(given)
	if err != nil {
		return nil, kerrors.New(kerrors.Validation, "%s", err)
	}
	repository := reference.Repository()
	switch _, bound, err := t.BindingOfTarget(ctx, a.app.Target); {
	case err != nil:
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	case bound:
		return nil, kerrors.New(kerrors.Conflict, "app `%s` builds from Git: its builds decide what it runs", params.App)
	}
	_, actor := acc.Actor()
	err = t.SetImagePolicy(ctx, store.NewImagePolicy{
		Project: a.env.project.project.ID, Target: a.app.Target, Repository: repository, Pattern: pattern.Text(),
		Enabled: req.Enabled.Or(defaultPolicyEnabled), IntervalSecs: uint32(interval), By: actor,
	})
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	record := requestAudit(acc, "image-policy.updated", "app", params.Project+"/"+params.Environment+"/"+params.App)
	record.Data = opt.Some[any](map[string]any{"repository": repository, "pattern": pattern.Text()})
	if err := t.AppendAudit(ctx, record); err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	saved, found, err := t.ImagePolicy(ctx, a.app.Target)
	switch {
	case err != nil:
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	case !found:
		return nil, kerrors.New(kerrors.Internal, "the policy is missing")
	}
	return imagePolicyDto(saved), t.Commit(ctx) //nolint:wrapcheck // a store error, answered as internal
}

// DeleteImagePolicy stops following the image repository.
func (s *Server) DeleteImagePolicy(ctx context.Context, params gen.DeleteImagePolicyParams) (gen.DeleteImagePolicyRes, error) {
	acc, a, t, err := s.appFor(ctx, perm.AppDeploy, params.Project, params.Environment, params.App)
	if err != nil {
		return nil, err
	}
	defer t.Rollback(ctx) //nolint:errcheck // committed on success
	switch deleted, err := t.DeleteImagePolicy(ctx, a.app.Target); {
	case err != nil:
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	case !deleted:
		return nil, kerrors.New(kerrors.NotFound, "an image policy of `%s`", params.App)
	}
	record := requestAudit(acc, "image-policy.deleted", "app", params.Project+"/"+params.Environment+"/"+params.App)
	if err := t.AppendAudit(ctx, record); err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	return &gen.DeleteImagePolicyNoContent{}, t.Commit(ctx) //nolint:wrapcheck // a store error, answered as internal
}
