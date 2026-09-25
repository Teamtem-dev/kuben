package api

// Export and detach (routes/apps/export.rs; M4.11, plan §17 exit path, S08).
//
// An export is one JSON document:
//   - the portable release and configuration;
//   - the Kubernetes objects of the app's newest delivered run, as frozen by
//     the renderer (with its version), as a `v1/List` that `kubectl apply`
//     takes;
//   - the references the objects depend on (Secrets, pull Secrets, volumes,
//     hostnames, Gateway and issuer);
//   - the inventory;
//   - a runbook.
//
// Secret values are never in it.
//
// A detach is a deletion that leaves the app running: its export is frozen
// with the request, the materializer orphans the objects, and the app
// leaves Kuben. The record stays until an operator releases it; until then
// the environment's namespace outlives the environment.

import (
	"fmt"
	"strings"

	"github.com/go-faster/jx"

	"github.com/Teamtem-dev/kuben/internal/core/kerrors"
	wire "github.com/Teamtem-dev/kuben/internal/jsonx"
	"github.com/Teamtem-dev/kuben/internal/store"
	"github.com/Teamtem-dev/kuben/internal/version"
)

// ExportFormat is the export document's format.
const ExportFormat = "kuben.dev/export/v1"

// ExportSubject is where an exported app lives.
type ExportSubject struct {
	Project     string
	Environment string
	App         string
	Namespace   string
	Delivery    string
	ExportedAt  string
}

// member is v[key] as serde_json's Index read it: null unless v is an
// object holding key.
func member(v any, key string) any {
	if o, ok := v.(map[string]any); ok {
		return o[key]
	}
	return nil
}

// exportItems is the frozen objects, each namespaced in namespace unless
// it names a namespace already.
func exportItems(resources any, namespace string) []any {
	list, _ := resources.([]any) //nolint:errcheck // anything else is no objects, as in Rust
	items := make([]any, 0, len(list))
	for _, item := range list {
		if o, ok := item.(map[string]any); ok {
			if meta, ok := o["metadata"].(map[string]any); ok {
				if _, named := meta["namespace"]; !named {
					copied := make(map[string]any, len(meta)+1)
					for k, v := range meta {
						copied[k] = v
					}
					copied["namespace"] = namespace
					dup := make(map[string]any, len(o))
					for k, v := range o {
						dup[k] = v
					}
					dup["metadata"] = copied
					item = dup
				}
			}
		}
		items = append(items, item)
	}
	return items
}

// namesOf is the names of items of kind.
func namesOf(items []any, kind string) []string {
	names := []string{}
	for _, i := range items {
		if member(i, "kind") != kind {
			continue
		}
		if name, ok := member(member(i, "metadata"), "name").(string); ok {
			names = append(names, name)
		}
	}
	return names
}

// hostnamesOf is the hostnames every HTTPRoute of items serves.
func hostnamesOf(items []any) []string {
	hosts := []string{}
	for _, i := range items {
		if member(i, "kind") != "HTTPRoute" {
			continue
		}
		list, _ := member(member(i, "spec"), "hostnames").([]any) //nolint:errcheck // none unless a list
		for _, h := range list {
			if host, ok := h.(string); ok {
				hosts = append(hosts, host)
			}
		}
	}
	return hosts
}

// exportRunbook is what the new owner of the app does next.
func exportRunbook(s ExportSubject, gateway string, hosts, volumes []string, issuer string, hasIssuer bool) []string {
	steps := []string{
		fmt.Sprintf("Secret values are not in this export. The Secrets listed under references.secrets stay in "+
			"namespace %s; once detached they lose Kuben's managed-by label and are never garbage-collected. "+
			"Rotate them yourself from now on, registry pull Secrets included.", s.Namespace),
		"To run the app elsewhere, copy those Secrets first, then apply `manifests` " +
			"(for example `jq .manifests export.json | kubectl apply -f -`).",
		"The app's database and any other external service are not managed by Kuben: their lifecycle, " +
			"credentials and backups stay as they are.",
	}
	if len(hosts) > 0 {
		certificates := ""
		if hasIssuer {
			certificates = fmt.Sprintf(" whose HTTPS listeners carry `cert-manager.io/cluster-issuer: %s`, "+
				"so certificates keep renewing", issuer)
		}
		steps = append(steps, fmt.Sprintf("Routing: the HTTPRoute serves %s through Gateway %s. While Kuben runs, "+
			"its Gateway keeps serving these hostnames. Before you uninstall Kuben, attach the route to a Gateway "+
			"you own%s.", strings.Join(hosts, ", "), gateway, certificates))
	}
	if len(volumes) > 0 {
		steps = append(steps, fmt.Sprintf("Volumes %s are kept: Kuben never deletes them after a detach. "+
			"Back them up before you move the app.", strings.Join(volumes, ", ")))
	}
	return append(steps,
		fmt.Sprintf("A new app named `%s` in this environment would take these objects over: pick another "+
			"name, or remove the objects first.", s.App),
		"Release the detached app once you own it for good: afterwards, deleting the environment deletes "+
			"its namespace with everything in it.")
}

// ExportDocument is the export document of s from its newest delivered
// run m.
func ExportDocument(s ExportSubject, m store.ExportMaterial) map[string]any {
	items := exportItems(m.Resources, s.Namespace)
	gateway, hasGateway := member(m.Capabilities, "gateway").(string)
	if !hasGateway {
		gateway = "(none)"
	}
	issuer, hasIssuer := member(m.Capabilities, "clusterIssuer").(string)
	hosts := hostnamesOf(items)
	volumes := namesOf(items, "PersistentVolumeClaim")
	secrets := make([]any, 0, len(m.Secrets))
	inventory := make([]any, 0, len(items)+len(m.Secrets))
	for _, i := range items {
		inventory = append(inventory, map[string]any{
			"apiVersion": member(i, "apiVersion"), "kind": member(i, "kind"),
			"name": member(member(i, "metadata"), "name"), "namespace": s.Namespace,
		})
	}
	for _, r := range m.Secrets {
		secrets = append(secrets, map[string]any{
			"name": r.Name, "revision": r.Revision, "object": r.Object(), "kind": r.Kind, "registry": r.Registry,
		})
		inventory = append(inventory, map[string]any{
			"apiVersion": "v1", "kind": "Secret", "name": r.Object(), "namespace": s.Namespace,
		})
	}
	clusterIssuer := any(nil)
	if hasIssuer {
		clusterIssuer = issuer
	}
	return map[string]any{
		"format":     ExportFormat,
		"kuben":      version.Version,
		"exportedAt": s.ExportedAt,
		"app": map[string]any{
			"project": s.Project, "environment": s.Environment, "app": s.App,
			"namespace": s.Namespace, "delivery": s.Delivery,
		},
		"run": map[string]any{"id": m.Run.String(), "generation": m.Generation, "phase": m.Phase, "reason": m.Reason},
		"release": map[string]any{
			"id": m.Release.String(), "artifacts": m.Artifacts, "processContract": m.ProcessContract,
			"portableConfig": m.PortableConfig, "source": m.Source.Or(nil),
		},
		"config":    map[string]any{"revision": m.ConfigRevision, "spec": m.Config},
		"renderer":  map[string]any{"version": m.RendererVersion, "capabilities": m.Capabilities},
		"manifests": map[string]any{"apiVersion": "v1", "kind": "List", "items": items},
		"references": map[string]any{
			"secrets": secrets, "volumes": volumes, "hostnames": hosts,
			"gateway": member(m.Capabilities, "gateway"), "clusterIssuer": clusterIssuer,
		},
		"inventory": inventory,
		"runbook":   exportRunbook(s, gateway, hosts, volumes, issuer, hasIssuer),
	}
}

// rawObject is doc as the generated free-form object. Each member is the
// canonical text serde_json printed for it: keys sorted at every level,
// numbers and strings as serde wrote them (a stored export read back
// prints the same bytes as the one that was frozen).
func rawObject(doc map[string]any) (map[string]jx.Raw, error) {
	out := make(map[string]jx.Raw, len(doc))
	for key, value := range doc {
		text, err := wire.CanonicalValue(value)
		if err != nil {
			return nil, kerrors.Wrap(err, "encode the export")
		}
		out[key] = jx.Raw(text)
	}
	return out, nil
}
