package api

// Resource admission of app changes (routes/apps/admission.rs, M4.5; plan
// §11, §16).
//
// Before a change is accepted, the app's peak requests (every process at
// its maximum replicas, plus the surge pod of a rolling update) are added to
// those of the other live apps and must fit the environment's quota and the
// installation's quota for the organization; a pod larger than the largest
// schedulable node is refused as well. Nodes that are not known yet only
// give a warning, never a false "fits". Nothing here can be overridden from
// the API: quotas are raised by whoever owns them.

import (
	"context"
	"encoding/json"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Teamtem-dev/kuben/go/hub/internal/core/capacity"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/ids"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/kerr"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/scan"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/controller"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/discovery"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/registry"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/render"
	"github.com/Teamtem-dev/kuben/go/hub/internal/store"
	"github.com/Teamtem-dev/kuben/go/hub/internal/wire"
	"github.com/Teamtem-dev/kuben/go/kubenapi/v1alpha1"
)

// anyImage stands in for the image when an app's resources are computed:
// only its processes matter.
const anyImage = "admission.invalid/any:latest"

// admissionPlacement is where an app change lands.
type admissionPlacement struct {
	environment ids.EnvironmentID
	// quota is the environment's quota (v1alpha1.Quota as JSON).
	quota  opt.Val[any]
	target ids.TargetID
}

// platform is the size presets of the cluster's KubenConfig, or the
// defaults without a cluster.
func (s *Server) platform(ctx context.Context) (render.Platform, error) {
	c, err := s.cluster()
	if err != nil {
		return render.DefaultPlatform(), nil //nolint:nilerr // no cluster: the defaults, as Rust
	}
	gvr := v1alpha1.SchemeGroupVersion.WithResource(v1alpha1.KubenConfigResource)
	list, err := c.Dynamic.Resource(gvr).List(ctx, metav1.ListOptions{})
	if err != nil {
		return render.Platform{}, kubeError(err, "kubenconfigs")
	}
	if list == nil {
		return render.DefaultPlatform(), nil
	}
	configs := make([]v1alpha1.KubenConfig, 0, len(list.Items))
	for i := range list.Items {
		data, err := list.Items[i].MarshalJSON()
		if err != nil {
			return render.Platform{}, kerr.Wrap(err, "reading a KubenConfig")
		}
		var c v1alpha1.KubenConfig
		if err := json.Unmarshal(data, &c); err != nil {
			return render.Platform{}, kerr.Wrap(err, "reading a KubenConfig")
		}
		configs = append(configs, c)
	}
	return controller.PlatformOf(configs), nil
}

// environmentLimits is the limits of an environment quota; an unreadable
// quota limits nothing.
func environmentLimits(quota opt.Val[any]) capacity.Limits {
	q, ok := quota.Get()
	if !ok || q == nil {
		return capacity.Limits{}
	}
	data, err := json.Marshal(q)
	if err != nil {
		return capacity.Limits{}
	}
	var parsed v1alpha1.Quota
	if err := json.Unmarshal(data, &parsed); err != nil {
		return capacity.Limits{}
	}
	var l capacity.Limits
	if parsed.CPU != nil {
		if m, ok := capacity.CPUMillis(*parsed.CPU); ok {
			l.CPUMillis = opt.Some(m)
		}
	}
	if parsed.Memory != nil {
		if b, ok := capacity.Bytes(*parsed.Memory); ok {
			l.MemoryBytes = opt.Some(b)
		}
	}
	if parsed.Pods != nil {
		l.Pods = opt.Some(uint64(*parsed.Pods))
	}
	return l
}

// peak is the peak requests of an app with spec.
func peak(spec *v1alpha1.AppSpec, p render.Platform) (render.AppDemand, error) {
	app := v1alpha1.App{Spec: *spec}
	app.Name = "admission"
	return render.Demand(&app, p) //nolint:wrapcheck // a BuildError, said as a validation failure
}

// admit admits spec at `at`, in t's transaction, and returns the warnings
// to show.
func (s *Server) admit(ctx context.Context, t *store.Tenant, at admissionPlacement, spec *v1alpha1.AppSpec) ([]string, error) {
	platform, err := s.platform(ctx)
	if err != nil {
		return nil, err
	}
	quota := s.deps.Config.Quota
	orgLimits, err := quota.OrgLimits()
	if err != nil {
		return nil, kerr.Wrap(err, "quota")
	}
	own, err := peak(spec, platform)
	if err != nil {
		return nil, kerr.New(kerr.Validation, "%s", err.Error())
	}
	live, err := t.LiveConfigs(ctx)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	var others []store.LiveConfig
	for _, l := range live {
		if l.Target != at.target {
			others = append(others, l)
		}
	}
	if limit, ok := quota.OrgApps.Get(); ok && uint64(len(others)) >= limit {
		return nil, kerr.New(kerr.Conflict, "the organization's quota allows %d apps", limit)
	}
	inEnvironment, inOrg := own.Peak, own.Peak
	for _, other := range others {
		otherSpec, ok := specOf(other.Config, opt.Some(anyImage))
		if !ok {
			continue
		}
		// A configuration that no longer renders cannot run either.
		demand, err := peak(&otherSpec, platform)
		if err != nil {
			continue
		}
		inOrg = inOrg.Plus(demand.Peak)
		if other.Environment == at.environment {
			inEnvironment = inEnvironment.Plus(demand.Peak)
		}
	}
	if err := environmentLimits(at.quota).Admit(inEnvironment); err != nil {
		return nil, kerr.New(kerr.Conflict, "the environment's quota is exceeded: %s", err.Error())
	}
	if err := orgLimits.Admit(inOrg); err != nil {
		return nil, kerr.New(kerr.Conflict, "the organization's quota is exceeded: %s", err.Error())
	}
	return s.schedulable(ctx, t, own)
}

// schedulable estimates whether the app's largest pod fits the largest
// node the cluster reported.
func (s *Server) schedulable(ctx context.Context, t *store.Tenant, own render.AppDemand) ([]string, error) {
	record, found, err := t.ClusterCapabilities(ctx, registry.Primary)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	largest := opt.None[capacity.NodeCapacity]()
	if found {
		if data, err := json.Marshal(record.Facts); err == nil {
			var facts discovery.ClusterFacts
			if json.Unmarshal(data, &facts) == nil {
				largest = facts.NodeCapacity()
			}
		}
	}
	switch e := capacity.EstimateFit(largest, own.LargestPod.CPUMillis, own.LargestPod.MemoryBytes).(type) {
	case capacity.FitsEstimate:
		return nil, nil
	case capacity.UnlikelyToSchedule:
		return nil, kerr.New(kerr.Conflict, "the app would not be scheduled: %s", e.Why)
	case capacity.UnknownConstraints:
		return []string{"scheduling is not estimated: " + e.Why}, nil
	}
	return nil, nil
}

// scanGate is the scan gate of target's environment on release (M4.6):
// its warnings, or a refusal naming the findings.
func (s *Server) scanGate(ctx context.Context, t *store.Tenant, target ids.TargetID, release ids.ReleaseID) ([]string, error) {
	verdict, found, err := t.ScanVerdict(ctx, target, release, s.deps.Clock.NowMs())
	if err != nil || !found {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	switch v := verdict.(type) {
	case scan.Pass:
		return nil, nil
	case scan.Warn:
		return v.Reasons, nil
	case scan.Block:
		return nil, kerr.New(kerr.Conflict, "the environment's vulnerability gate refuses this release: %s",
			strings.Join(v.Reasons, "; "))
	}
	return nil, nil
}

// specOf is an App spec from a configuration revision and the image it
// runs (apps/mod.rs spec_of); false when the configuration is not one.
func specOf(config opt.Val[any], image opt.Val[string]) (v1alpha1.AppSpec, bool) {
	c, ok := config.Get()
	if !ok {
		return v1alpha1.AppSpec{}, false
	}
	if object, isObject := c.(map[string]any); isObject {
		if img, has := image.Get(); has {
			withImage := make(map[string]any, len(object)+1)
			for k, v := range object {
				withImage[k] = v
			}
			withImage["source"] = map[string]any{"image": img}
			c = withImage
		}
	}
	text, err := wire.CanonicalValue(c)
	if err != nil {
		return v1alpha1.AppSpec{}, false
	}
	spec, err := v1alpha1.DecodeAppSpec([]byte(text))
	return spec, err == nil
}
