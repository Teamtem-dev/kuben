// Package projection is the read models (crates/kuben-platform/src/
// projection): informers translate Kubernetes objects into small UI-shaped
// views (a PodView is ~200 bytes vs several KiB for a raw Pod), keep them in
// memory, and publish deltas on a bounded fan-out that the event stream
// consumes (ADR-004). It also carries the per-connection tenant filter of
// the event stream (crates/kuben-api/src/stream.rs, Visibility) and the
// stream.Source built on both.
package projection

import (
	"maps"
	"reflect"
	"slices"
	"strings"
	"sync"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/kube/render"
)

// DeltaCapacity bounds each subscriber's backlog. A consumer that falls
// further behind is told to resync (its backlog is dropped) rather than
// growing memory.
const DeltaCapacity = 1024

// Sync bits: one per informer, set when its initial LIST has been swapped
// in.
const (
	syncedPods uint8 = 1 << iota
	syncedProjects
	syncedEnvironments
	syncedApps
	syncedAll = syncedPods | syncedProjects | syncedEnvironments | syncedApps
)

// table is one kind's views by key. Guarded by the Projections mutex.
type table[V any] struct {
	items map[string]*V
	key   func(*V) string
}

func newTable[V any](key func(*V) string) table[V] {
	return table[V]{items: map[string]*V{}, key: key}
}

// upsert stores value and returns it, or nil when it is unchanged.
func (t table[V]) upsert(value V) *V {
	stored := &value
	k := t.key(stored)
	if old, ok := t.items[k]; ok && reflect.DeepEqual(*old, value) {
		return nil
	}
	t.items[k] = stored
	return stored
}

func (t table[V]) remove(key string) bool {
	_, ok := t.items[key]
	delete(t.items, key)
	return ok
}

func (t table[V]) replace(items []V) {
	clear(t.items)
	for i := range items {
		v := items[i]
		t.items[t.key(&v)] = &v
	}
}

// get is the view under key. Stored views are never nil; the check makes
// that visible to NilAway instead of trusting it.
func (t table[V]) get(key string) (*V, bool) {
	v, ok := t.items[key]
	if !ok || v == nil {
		return nil, false
	}
	return v, true
}

// sorted is every view, sorted by key.
func (t table[V]) sorted() []*V {
	keys := slices.Sorted(maps.Keys(t.items))
	out := make([]*V, 0, len(keys))
	for _, k := range keys {
		out = append(out, t.items[k])
	}
	return out
}

// Projections is the in-memory view of the cluster the API and the event
// stream read. Make one with New; it is safe for concurrent use.
type Projections struct {
	mu           sync.Mutex // guards everything below
	pods         table[PodView]
	projects     table[ProjectView]
	environments table[EnvironmentView]
	apps         table[AppView]
	// Optional kinds (Gateway API, cert-manager): never part of readiness.
	routes       table[RouteView]
	certificates table[CertificateView]
	seq          uint64
	subscribers  map[*Subscription]struct{}
	// synced holds the sync bits. Until every informer has listed once,
	// the tables are incomplete and lookups would answer 404 for objects
	// that exist.
	synced uint8
	// allSynced is closed when synced reaches syncedAll.
	allSynced chan struct{}
}

// New is an empty store at sequence 0, not synced.
func New() *Projections {
	return &Projections{
		pods:         newTable(func(v *PodView) string { return v.Key }),
		projects:     newTable(func(v *ProjectView) string { return v.Name }),
		environments: newTable(func(v *EnvironmentView) string { return v.Name }),
		apps:         newTable(func(v *AppView) string { return v.Key }),
		routes:       newTable(func(v *RouteView) string { return v.Key }),
		certificates: newTable(func(v *CertificateView) string { return v.Key }),
		subscribers:  map[*Subscription]struct{}{},
		allSynced:    make(chan struct{}),
	}
}

// markSynced sets bit; the caller holds mu.
func (p *Projections) markSynced(bit uint8) {
	was := p.synced
	p.synced |= bit
	if was != syncedAll && p.synced == syncedAll {
		close(p.allSynced)
	}
}

// IsSynced reports whether every informer has completed its initial LIST.
func (p *Projections) IsSynced() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.synced == syncedAll
}

// Synced is closed once every informer has completed its initial LIST.
func (p *Projections) Synced() <-chan struct{} { return p.allSynced }

// PendingKinds are the informers that have not completed their initial
// LIST yet.
func (p *Projections) PendingKinds() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	pending := []string{}
	for _, k := range []struct {
		bit  uint8
		kind string
	}{
		{syncedPods, "pods"}, {syncedProjects, "projects"}, {syncedEnvironments, "environments"}, {syncedApps, "apps"},
	} {
		if p.synced&k.bit == 0 {
			pending = append(pending, k.kind)
		}
	}
	return pending
}

// Seq is the current global sequence (also used as ETag).
func (p *Projections) Seq() uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.seq
}

// publish numbers a delta and offers it to every subscriber; the caller
// holds mu, so every subscriber sees the deltas in sequence order.
func (p *Projections) publish(build func(seq uint64) Delta) {
	p.seq++
	d := build(p.seq)
	for s := range p.subscribers {
		s.offer(d)
	}
}

func (p *Projections) resync() {
	p.publish(func(seq uint64) Delta { return Resync{Seq: seq} })
}

// Snapshot is a point-in-time view of everything, tagged with the sequence
// it reflects.
type Snapshot struct {
	Seq          uint64             `json:"seq"`
	Pods         []*PodView         `json:"pods"`
	Projects     []*ProjectView     `json:"projects"`
	Environments []*EnvironmentView `json:"environments"`
	Apps         []*AppView         `json:"apps"`
}

// Snapshot is everything, at the current sequence.
func (p *Projections) Snapshot() Snapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	return Snapshot{
		Seq:          p.seq,
		Pods:         p.pods.sorted(),
		Projects:     p.projects.sorted(),
		Environments: p.environments.sorted(),
		Apps:         p.apps.sorted(),
	}
}

// ---- pods ----

// UpsertPod stores pod and publishes it when it changed.
func (p *Projections) UpsertPod(pod PodView) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if v := p.pods.upsert(pod); v != nil {
		p.publish(func(seq uint64) Delta { return PodUpsert{Seq: seq, Pod: v} })
	}
}

// RemovePod removes the pod `namespace/name` and publishes it when it
// existed.
func (p *Projections) RemovePod(key string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.pods.remove(key) {
		p.publish(func(seq uint64) Delta { return PodDelete{Seq: seq, Key: key} })
	}
}

// Pod is the pod `namespace/name`.
func (p *Projections) Pod(key string) (*PodView, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.pods.get(key)
}

// PodCount is the number of pods.
func (p *Projections) PodCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.pods.items)
}

// PodsOfApp are the pods of one app, sorted by key.
func (p *Projections) PodsOfApp(namespace, app string) []*PodView {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := []*PodView{}
	for _, pod := range p.pods.sorted() {
		if a, ok := pod.App.Get(); ok && a == app && pod.Namespace == namespace {
			out = append(out, pod)
		}
	}
	return out
}

// ReplacePods replaces the whole pod set after a LIST.
func (p *Projections) ReplacePods(pods []PodView) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pods.replace(pods)
	p.markSynced(syncedPods)
	p.resync()
}

// ---- projects ----

// UpsertProject stores project and publishes it when it changed.
func (p *Projections) UpsertProject(project ProjectView) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if v := p.projects.upsert(project); v != nil {
		p.publish(func(seq uint64) Delta { return ProjectUpsert{Seq: seq, Project: v} })
	}
}

// RemoveProject removes the project name and publishes it when it existed.
func (p *Projections) RemoveProject(name string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.projects.remove(name) {
		p.publish(func(seq uint64) Delta { return ProjectDelete{Seq: seq, Key: name} })
	}
}

// Projects are every project, sorted by name.
func (p *Projections) Projects() []*ProjectView {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.projects.sorted()
}

// Project is the project name.
func (p *Projections) Project(name string) (*ProjectView, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.projects.get(name)
}

// ReplaceProjects replaces the whole project set after a LIST.
func (p *Projections) ReplaceProjects(projects []ProjectView) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.projects.replace(projects)
	p.markSynced(syncedProjects)
	p.resync()
}

// ---- environments ----

// UpsertEnvironment stores environment and publishes it when it changed.
func (p *Projections) UpsertEnvironment(environment EnvironmentView) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if v := p.environments.upsert(environment); v != nil {
		p.publish(func(seq uint64) Delta { return EnvironmentUpsert{Seq: seq, Environment: v} })
	}
}

// RemoveEnvironment removes the environment name and publishes it when it
// existed.
func (p *Projections) RemoveEnvironment(name string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.environments.remove(name) {
		p.publish(func(seq uint64) Delta { return EnvironmentDelete{Seq: seq, Key: name} })
	}
}

// Environments are every environment, sorted by name.
func (p *Projections) Environments() []*EnvironmentView {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.environments.sorted()
}

// Environment is the environment name.
func (p *Projections) Environment(name string) (*EnvironmentView, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.environments.get(name)
}

// ReplaceEnvironments replaces the whole environment set after a LIST.
func (p *Projections) ReplaceEnvironments(environments []EnvironmentView) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.environments.replace(environments)
	p.markSynced(syncedEnvironments)
	p.resync()
}

// ---- apps ----

// UpsertApp stores app and publishes it when it changed.
func (p *Projections) UpsertApp(app AppView) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if v := p.apps.upsert(app); v != nil {
		p.publish(func(seq uint64) Delta { return AppUpsert{Seq: seq, App: v} })
	}
}

// RemoveApp removes the app `namespace/name` and publishes it when it
// existed.
func (p *Projections) RemoveApp(key string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.apps.remove(key) {
		p.publish(func(seq uint64) Delta { return AppDelete{Seq: seq, Key: key} })
	}
}

// Apps are every app, sorted by key.
func (p *Projections) Apps() []*AppView {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.apps.sorted()
}

// App is the app namespace/name.
func (p *Projections) App(namespace, name string) (*AppView, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.apps.get(namespace + "/" + name)
}

// ReplaceApps replaces the whole app set after a LIST.
func (p *Projections) ReplaceApps(apps []AppView) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.apps.replace(apps)
	p.markSynced(syncedApps)
	p.resync()
}

// ---- routes and certificates (M2.3) ----

// exposureChanged publishes that the app key's exposure changed; the
// caller holds mu.
func (p *Projections) exposureChanged(key string) {
	p.publish(func(seq uint64) Delta { return ExposureChanged{Seq: seq, Key: key} })
}

// UpsertRoute stores route; its app's exposure changed when it did.
func (p *Projections) UpsertRoute(route RouteView) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if v := p.routes.upsert(route); v != nil {
		p.exposureChanged(v.Key)
	}
}

// RemoveRoute removes the route `namespace/name`.
func (p *Projections) RemoveRoute(key string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.routes.remove(key) {
		p.exposureChanged(key)
	}
}

// ReplaceRoutes replaces the whole route set after a LIST.
func (p *Projections) ReplaceRoutes(routes []RouteView) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.routes.replace(routes)
	p.resync()
}

// Route is the route of the app namespace/name.
func (p *Projections) Route(namespace, name string) (*RouteView, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.routes.get(namespace + "/" + name)
}

// Routes are every app route, sorted by key.
func (p *Projections) Routes() []*RouteView {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.routes.sorted()
}

// routesUsing are the routes whose hosts use the certificate
// `namespace/name`; the caller holds mu.
func (p *Projections) routesUsing(certificate string) []string {
	namespace, name, ok := strings.Cut(certificate, "/")
	if !ok {
		return nil
	}
	var out []string
	for _, r := range p.routes.sorted() {
		inNamespace := slices.ContainsFunc(r.Gateways, func(g string) bool {
			ns, _, ok := strings.Cut(g, "/")
			return ok && ns == namespace
		})
		usesIt := slices.ContainsFunc(r.Domains, func(d render.DomainClaim) bool {
			return render.HostSecretName(d.Host) == name
		})
		if inNamespace && usesIt {
			out = append(out, r.Key)
		}
	}
	return out
}

// UpsertCertificate stores certificate; the exposure of every app whose
// hosts use it changed when it did.
func (p *Projections) UpsertCertificate(certificate CertificateView) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if v := p.certificates.upsert(certificate); v != nil {
		for _, route := range p.routesUsing(v.Key) {
			p.exposureChanged(route)
		}
	}
}

// RemoveCertificate removes the certificate `namespace/name`.
func (p *Projections) RemoveCertificate(key string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.certificates.remove(key) {
		for _, route := range p.routesUsing(key) {
			p.exposureChanged(route)
		}
	}
}

// ReplaceCertificates replaces the whole certificate set after a LIST.
func (p *Projections) ReplaceCertificates(certificates []CertificateView) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.certificates.replace(certificates)
	p.resync()
}

// Exposure is how the app namespace/name is reached, once its route
// exists. A host with a listener of its own (`h-<hash>`) has a certificate
// of its own; a route without listener names is served without TLS.
func (p *Projections) Exposure(namespace, name string) (ExposureView, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	route, ok := p.routes.get(namespace + "/" + name)
	if !ok {
		return ExposureView{}, false
	}
	gatewayNS := opt.None[string]()
	if len(route.Gateways) > 0 {
		if ns, _, ok := strings.Cut(route.Gateways[0], "/"); ok {
			gatewayNS = opt.Some(ns)
		}
	}
	hosts := make([]HostExposure, 0, len(route.Domains))
	for _, domain := range route.Domains {
		hosts = append(hosts, p.hostExposure(route, domain, gatewayNS))
	}
	return ExposureView{Accepted: route.Accepted, Message: route.Message, Hosts: hosts}, true
}

// hostExposure is how one domain of route is reached; the caller holds mu.
func (p *Projections) hostExposure(route *RouteView, domain render.DomainClaim, gatewayNS opt.Val[string]) HostExposure {
	tls := "auto"
	mode := domain.Mode().Kind
	switch {
	case len(route.Sections) == 0 || mode == render.TLSPlain:
		tls = "none"
	case mode == render.TLSSecret:
		tls = "secret"
	}
	own := tls == "auto" && slices.Contains(route.Sections, render.HostListenerName(domain.Host))
	var certificate *CertificateView
	if ns, ok := gatewayNS.Get(); ok && own {
		if c, ok := p.certificates.get(ns + "/" + render.HostSecretName(domain.Host)); ok {
			certificate = c
		}
	}
	h := HostExposure{Host: domain.Host, TLS: tls}
	if own {
		h.CertificateReady = opt.Some(certificate != nil && certificate.Ready)
	}
	switch {
	case certificate != nil:
		h.CertificateMessage = certificate.Message
	case own:
		h.CertificateMessage = opt.Some("not issued yet")
	}
	return h
}
