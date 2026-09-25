package materializer

import (
	"context"
	"errors"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Teamtem-dev/kuben/api/v1alpha1"
	"github.com/Teamtem-dev/kuben/internal/core/opt"
	wire "github.com/Teamtem-dev/kuben/internal/jsonx"
	"github.com/Teamtem-dev/kuben/internal/kube/controller"
	"github.com/Teamtem-dev/kuben/internal/kube/render"
	"github.com/Teamtem-dev/kuben/internal/store"
)

// planRefusal is the failure code of a run whose plan cannot be rendered.
func planRefusal(err error) string {
	var build *render.BuildError
	var tooLarge *render.TooLargeError
	switch {
	case errors.As(err, &build) && build != nil:
		return string(build.Reason)
	case errors.As(err, &tooLarge):
		return "PlanTooLarge"
	}
	return "RenderFailed"
}

// freeze freezes the run's RenderPlan before its first write (ADR-026):
// the App rendered against the cluster's capabilities now. A retry finds
// the plan frozen and never renders it again.
func (w *Worker) freeze(ctx context.Context, claim store.Claim, m *store.Materialization, app *v1alpha1.App) stop {
	facts := opt.None[render.IssuerFacts]()
	if watch, ok := w.d.Facts.Get(); ok && watch != nil {
		f, known := watch.Get()
		if !known {
			// A plan is frozen for good: never render it before the
			// cluster's capabilities are known.
			return stopWait{after: poll, code: "CapabilitiesPending"}
		}
		facts = opt.Some[render.IssuerFacts](f)
	}
	list, err := w.resource(configsGVR, "").List(ctx, metav1.ListOptions{})
	if err != nil {
		return retryKube(err)
	}
	if list == nil {
		return retryKube(errors.New("listing KubenConfigs: no list returned"))
	}
	configs := make([]v1alpha1.KubenConfig, 0, len(list.Items))
	for i := range list.Items {
		c, err := decode[v1alpha1.KubenConfig](&list.Items[i])
		if err != nil {
			return retryKube(err)
		}
		configs = append(configs, c)
	}
	capabilities := render.CapabilitiesOf(controller.PlatformOf(configs).Gated(facts))
	plan, err := render.Render(app, capabilities)
	if err != nil {
		return refused(planRefusal(err))
	}
	snapshotJSON, err := plan.CapabilitiesJSON()
	if err != nil {
		return refused("RenderFailed")
	}
	snapshot, err := wire.DecodeAny(snapshotJSON)
	if err != nil {
		return refused("RenderFailed")
	}
	resources, err := plan.Resources()
	if err != nil {
		return refused("RenderFailed")
	}
	t, err := w.d.Store.Tenant(ctx, m.Org)
	if err != nil {
		return retryStore(err)
	}
	defer t.Rollback(ctx) //nolint:errcheck // a no-op after the commit
	id, frozen, err := t.FreezeRunPlan(ctx, claim, m.Run, render.RendererVersion, snapshot, resources)
	switch {
	case err != nil:
		return retryStore(err)
	case !frozen:
		return stopFenced{}
	}
	if err := t.Commit(ctx); err != nil {
		return retryStore(err)
	}
	w.d.Logger.Info("render plan frozen", "run", m.Run, "plan", id, "digest", plan.Digest)
	return nil
}
