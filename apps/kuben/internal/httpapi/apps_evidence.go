package httpapi

// The observations behind an app's evidence graph (M5.5,
// routes/apps/evidence.rs): SQL for builds and runs, the cluster for the
// Deployment, Service and EndpointSlices, the projections for pods, and
// the Doctor's own checks for the Gateway, route, DNS and TLS.

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"unicode"
	"unicode/utf8"

	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/ops/build"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/ops/run"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/doctor"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/evidence"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/kube/projection"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/kube/registry"
	"github.com/Teamtem-dev/kuben/packages/api/v1alpha1"
)

// fatalReasons are the pod reasons that stop an app by themselves.
var fatalReasons = []string{
	"CrashLoopBackOff",
	"OOMKilled",
	"ImagePullBackOff",
	"ErrImagePull",
	"CreateContainerConfigError",
	"InvalidImageName",
}

// worstOf is the worst status of the checks whose id is one of ids, with
// a fact (`subject: detail`) for each of them that is not ok; false when
// none of them was checked.
func worstOf(checks []doctor.Check, ids []string) (doctor.Status, []string, bool) {
	var (
		status doctor.Status
		facts  = []string{}
		found  bool
	)
	for _, c := range checks {
		if !slices.Contains(ids, c.ID) {
			continue
		}
		if !found || c.Status.Rank() >= status.Rank() {
			status = c.Status
		}
		found = true
		if c.Status != doctor.StatusOK {
			facts = append(facts, c.Subject+": "+c.Detail)
		}
	}
	return status, facts, found
}

// fromChecks folds the checks whose id is one of ids into a node of layer:
// their worst status, a fact for each that is not ok, and the hint of the
// first of those that has one. False when none of them was checked.
func fromChecks(layer evidence.Layer, subject string, checks []doctor.Check, ids []string) (evidence.Node, bool) {
	status, facts, found := worstOf(checks, ids)
	if !found {
		return evidence.Node{}, false
	}
	node := evidence.NewNode(layer, status, subject)
	node.Evidence = facts
	for _, c := range checks {
		if !slices.Contains(ids, c.ID) || c.Status == doctor.StatusOK {
			continue
		}
		if hint, ok := c.Hint.Get(); ok {
			node.Action = opt.Some(hint)
			break
		}
	}
	return node, true
}

// debugName is a phase as Rust's derived Debug printed the enum variant:
// the wire name with its first letter in upper case (`VerifyingOutput`).
func debugName(phase build.Phase) string {
	name := string(phase)
	first, size := utf8.DecodeRuneInString(name)
	if size == 0 {
		return name
	}
	return string(unicode.ToUpper(first)) + name[size:]
}

// buildNode is the node of the newest build of an app built from source,
// or of none yet.
func buildNode(newest opt.Val[buildFacts]) evidence.Node {
	b, ok := newest.Get()
	if !ok {
		return evidence.NewNode(evidence.Build, doctor.StatusUnknown, "no build yet").Fact("the source has not been built")
	}
	phase := debugName(b.phase)
	status := doctor.StatusOK
	//exhaustive:ignore // only failed, cancelled, blocked deviate from OK
	switch b.phase {
	case build.Failed:
		status = doctor.StatusFail
	case build.Cancelled, build.Blocked:
		status = doctor.StatusWarn
	}
	node := evidence.NewNode(evidence.Build, status, "build of "+b.commit).
		Fact("phase " + phase).
		At(b.createdAt)
	if code, ok := b.failure.Get(); ok {
		node = node.Fact("failure " + code).WithAction("Read the build log (app → Builds).")
	}
	return node
}

// buildFacts is what the build node reads of a build attempt.
type buildFacts struct {
	phase     build.Phase
	commit    string // short
	createdAt int64
	failure   opt.Val[string]
}

// runFacts is what the release node reads of a deployment run.
type runFacts struct {
	run        string
	generation uint64
	phase      run.Phase
	createdAt  int64
	image      opt.Val[string]
}

// releaseNode is the node of the newest run, or of none.
func releaseNode(newest opt.Val[runFacts]) evidence.Node {
	r, ok := newest.Get()
	if !ok {
		return evidence.NewNode(evidence.Release, doctor.StatusWarn, "never deployed").WithAction("Deploy the app.")
	}
	status := doctor.StatusOK
	//exhaustive:ignore // only failing/blocking phases deviate from OK
	switch r.phase {
	case run.Failed, run.RecoveryFailed, run.ManualActionRequired:
		status = doctor.StatusFail
	case run.AwaitingApproval, run.Blocked:
		status = doctor.StatusWarn
	}
	node := evidence.NewNode(evidence.Release, status, "revision "+strconv.FormatUint(r.generation, 10)).
		Fact("run " + r.run + " is " + r.phase.String()).
		At(r.createdAt)
	if image, ok := r.image.Get(); ok {
		node = node.Fact("image " + image)
	}
	//exhaustive:ignore // only actions for approval or failure
	switch r.phase {
	case run.AwaitingApproval:
		node = node.WithAction("Ask an approver to approve the run.")
	case run.Failed:
		node = node.WithAction("Read the run's timeline, fix the cause and deploy again, or roll back.")
	}
	return node
}

// sourceNodes is the build node (for an app built from source) and the
// release node of a, from SQL.
func (s *Server) sourceNodes(ctx context.Context, a appScope) ([]evidence.Node, error) {
	t, err := s.deps.Store.Tenant(ctx, a.env.project.org)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	defer t.Rollback(ctx) //nolint:errcheck // read only
	var nodes []evidence.Node
	_, bound, err := t.BindingOfTarget(ctx, a.app.Target)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	if bound {
		builds, err := t.BuildsOfTarget(ctx, a.app.Target, 1)
		if err != nil {
			return nil, err //nolint:wrapcheck // a store error, answered as internal
		}
		newest := opt.None[buildFacts]()
		if len(builds) > 0 {
			b := builds[0]
			newest = opt.Some(buildFacts{
				phase: b.Phase, commit: b.Commit.Short(), createdAt: b.CreatedAt, failure: b.Failure,
			})
		}
		nodes = append(nodes, buildNode(newest))
	}
	runs, err := t.Runs(ctx, a.app.Target, 1)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	newest := opt.None[runFacts]()
	if len(runs) > 0 {
		r := runs[0]
		newest = opt.Some(runFacts{
			run: r.Run.String(), generation: uint64(r.Generation), phase: r.Phase, createdAt: r.CreatedAt, image: r.Image,
		})
	}
	return append(nodes, releaseNode(newest)), nil
}

// deploymentNode is the node of the app's Deployments as listed, or of
// the error that kept them from being read.
func deploymentNode(deployments []appsv1.Deployment, err error) evidence.Node {
	if err != nil {
		return evidence.NewNode(evidence.Deployment, doctor.StatusUnknown, "workloads").Fact("cannot read: " + err.Error())
	}
	if len(deployments) == 0 {
		return evidence.NewNode(evidence.Deployment, doctor.StatusFail, "no Deployment").
			Fact("no Deployment carries the app's label").
			WithAction("Check the run's timeline: the app was not written to the cluster.")
	}
	status := doctor.StatusOK
	node := evidence.NewNode(evidence.Deployment, doctor.StatusOK, fmt.Sprintf("%d Deployment(s)", len(deployments)))
	for _, d := range deployments {
		for _, c := range d.Status.Conditions {
			failed := (c.Type == appsv1.DeploymentAvailable && c.Status == "False") ||
				(c.Type == appsv1.DeploymentProgressing && c.Reason == "ProgressDeadlineExceeded")
			if !failed {
				continue
			}
			status = doctor.StatusFail
			reason := c.Reason
			if reason == "" {
				reason = "-"
			}
			node.Evidence = append(node.Evidence, fmt.Sprintf("%s: %s %s (%s)", d.Name, c.Type, c.Status, reason))
		}
	}
	node.Status = status
	return node
}

// podsNode is the node of the app's pods as the projections know them.
func podsNode(pods []*projection.PodView) evidence.Node {
	if len(pods) == 0 {
		return evidence.NewNode(evidence.Pods, doctor.StatusFail, "no pods").Fact("no pod of the app is known")
	}
	ready := 0
	for _, p := range pods {
		if p.Ready {
			ready++
		}
	}
	node := evidence.NewNode(evidence.Pods, doctor.StatusOK, fmt.Sprintf("%d of %d pods ready", ready, len(pods)))
	for _, p := range pods {
		if reason, ok := p.Reason.Get(); ok && slices.Contains(fatalReasons, reason) {
			node.Status = doctor.StatusFail
			node.Evidence = append(node.Evidence, fmt.Sprintf("%s: %s (%d restarts)", p.Name, reason, p.Restarts))
		} else if p.Phase == projection.PodPending {
			node.Evidence = append(node.Evidence, p.Name+": pending")
		}
	}
	switch {
	case node.Status == doctor.StatusOK && ready == 0:
		node.Status = doctor.StatusFail
	case node.Status == doctor.StatusOK && ready < len(pods):
		node.Status = doctor.StatusWarn
	}
	if node.Status != doctor.StatusOK {
		node.Action = opt.Some("Read the app's logs and events (app → Logs).")
	}
	return node
}

// networkNodes is the node of the app's Service and the node of its ready
// endpoints, read from the cluster.
func networkNodes(ctx context.Context, cluster registry.Cluster, namespace, slug string) []evidence.Node {
	var service evidence.Node
	switch _, err := cluster.Typed.CoreV1().Services(namespace).Get(ctx, slug, metav1.GetOptions{}); {
	case err == nil:
		service = evidence.NewNode(evidence.Service, doctor.StatusOK, "Service "+slug)
	case apierrors.IsNotFound(err):
		service = evidence.NewNode(evidence.Service, doctor.StatusFail, "Service "+slug).Fact("the Service is missing")
	default:
		service = evidence.NewNode(evidence.Service, doctor.StatusUnknown, "Service").Fact("cannot read: " + err.Error())
	}
	var endpoints evidence.Node
	list, err := cluster.Typed.DiscoveryV1().EndpointSlices(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "kubernetes.io/service-name=" + slug,
	})
	if err != nil {
		endpoints = evidence.NewNode(evidence.Endpoints, doctor.StatusUnknown, "endpoints").Fact("cannot read: " + err.Error())
	} else {
		ready := 0
		for _, s := range list.Items {
			for _, e := range s.Endpoints {
				if e.Conditions.Ready != nil && *e.Conditions.Ready {
					ready++
				}
			}
		}
		status := doctor.StatusOK
		if ready == 0 {
			status = doctor.StatusFail
		}
		endpoints = evidence.NewNode(evidence.Endpoints, status, fmt.Sprintf("%d ready endpoint(s)", ready)).
			Fact(fmt.Sprintf("%d EndpointSlice(s)", len(list.Items)))
	}
	return []evidence.Node{service, endpoints}
}

// The checks each layer of the network path folds.
var (
	gatewayChecks = []string{"gateway-class", "gateway", "port-80", "port-443"}
	routeChecks   = []string{"route"}
	dnsLayer      = []string{"dns", "claim", "delegation"}
	tlsChecks     = []string{"issuer", "certificate"}
)

// evidenceGraph is the evidence graph of a, from the Doctor's checks and
// fresh observations of cluster.
func (s *Server) evidenceGraph(ctx context.Context, a appScope, cluster registry.Cluster, checks []doctor.Check) (evidence.Graph, error) {
	nodes, err := s.sourceNodes(ctx, a)
	if err != nil {
		return evidence.Graph{}, err
	}
	list, err := cluster.Typed.AppsV1().Deployments(a.app.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: v1alpha1.LabelApp + "=" + a.app.Slug,
	})
	var deployments []appsv1.Deployment
	if err == nil {
		deployments = list.Items
	}
	nodes = append(nodes,
		deploymentNode(deployments, err),
		podsNode(s.deps.Projections.PodsOfApp(a.app.Namespace, a.app.Slug)),
	)
	serves := slices.ContainsFunc(checks, func(c doctor.Check) bool { return c.ID == "route" || c.ID == "dns" })
	if serves {
		nodes = append(nodes, networkNodes(ctx, cluster, a.app.Namespace, a.app.Slug)...)
		for _, layer := range []struct {
			layer   evidence.Layer
			subject string
			ids     []string
		}{
			{evidence.Gateway, "Gateway", gatewayChecks},
			{evidence.Route, "route", routeChecks},
			{evidence.DNS, "DNS", dnsLayer},
			{evidence.TLS, "certificates", tlsChecks},
		} {
			if node, ok := fromChecks(layer.layer, layer.subject, checks, layer.ids); ok {
				nodes = append(nodes, node)
			}
		}
	}
	return evidence.Of(nodes), nil
}
