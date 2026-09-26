package httpapi_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/uuid"

	"github.com/Teamtem-dev/kuben/internal/core/ids"
	"github.com/Teamtem-dev/kuben/internal/httpapi"
	"github.com/Teamtem-dev/kuben/internal/jsonx"
	"github.com/Teamtem-dev/kuben/internal/store"
	"github.com/Teamtem-dev/kuben/internal/version"
)

// exportMaterial is routes/apps/export.rs material().
func exportMaterial(t *testing.T) store.ExportMaterial {
	t.Helper()
	return store.ExportMaterial{
		Run:             ids.From[ids.DeploymentRun](uuid.Nil),
		Generation:      3,
		Phase:           "succeeded",
		Reason:          "deploy",
		Release:         uuid.Nil,
		Artifacts:       jsonValue(t, `{"web":"ghcr.io/acme/web@sha256:aa"}`),
		ProcessContract: jsonValue(t, `{}`),
		PortableConfig:  jsonValue(t, `{"env":{"MODE":"prod"}}`),
		ConfigRevision:  2,
		Config:          jsonValue(t, `{"replicas":2}`),
		RendererVersion: "kuben-renderer/2",
		Capabilities:    jsonValue(t, `{"gateway":"kuben-system/kuben","clusterIssuer":"letsencrypt"}`),
		Resources: jsonValue(t, `[
			{"apiVersion":"v1","kind":"PersistentVolumeClaim","metadata":{"name":"web-data"}},
			{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"web-web"}},
			{"apiVersion":"gateway.networking.k8s.io/v1","kind":"HTTPRoute",
			 "metadata":{"name":"web","namespace":"other"},"spec":{"hostnames":["web.example.com"]}}]`),
		Secrets: []store.SecretReference{{Name: "db", Revision: 4, Kind: "opaque"}},
	}
}

// exportSubject is routes/apps/export.rs subject().
func exportSubject() httpapi.ExportSubject {
	return httpapi.ExportSubject{
		Project: "shop", Environment: "prod", App: "web", Namespace: "shop-prod", Delivery: "controller",
		ExportedAt: "2026-09-17T00:00:00Z",
	}
}

// generic is doc as the JSON the API answers, read back as serde_json's
// Value.
func generic(t *testing.T, doc map[string]any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	v, err := jsonx.DecodeAny(raw)
	if err != nil {
		t.Fatal(err)
	}
	out, _ := v.(map[string]any)
	return out
}

// jsonAt is v at path (object keys and array indices), nil when absent.
func jsonAt(v any, path ...any) any {
	for _, step := range path {
		switch k := step.(type) {
		case string:
			o, _ := v.(map[string]any)
			v = o[k]
		case int:
			a, _ := v.([]any)
			if k >= len(a) {
				return nil
			}
			v = a[k]
		}
	}
	return v
}

func TestAnExportHoldsManifestsReferencesAndARunbook(t *testing.T) {
	doc := generic(t, httpapi.ExportDocument(exportSubject(), exportMaterial(t)))
	checks := []struct {
		path []any
		want any
	}{
		{[]any{"format"}, httpapi.ExportFormat},
		{[]any{"manifests", "kind"}, "List"},
		{[]any{"manifests", "items", 0, "metadata", "namespace"}, "shop-prod"},
		{[]any{"manifests", "items", 2, "metadata", "namespace"}, "other"},
		{[]any{"references", "volumes"}, []any{"web-data"}},
		{[]any{"references", "hostnames"}, []any{"web.example.com"}},
		{[]any{"references", "secrets", 0, "object"}, "db.r4"},
		{[]any{"renderer", "version"}, "kuben-renderer/2"},
	}
	for _, c := range checks {
		if diff := cmp.Diff(c.want, jsonAt(doc, c.path...)); diff != "" {
			t.Errorf("%v (-want +got):\n%s", c.path, diff)
		}
	}
	inventory, _ := doc["inventory"].([]any)
	if len(inventory) != 4 {
		t.Errorf("inventory: %v", inventory)
	}
	secret := false
	for _, i := range inventory {
		secret = secret || (jsonAt(i, "kind") == "Secret" && jsonAt(i, "name") == "db.r4")
	}
	if !secret {
		t.Errorf("no Secret in the inventory: %v", inventory)
	}
	runbook, err := json.Marshal(doc["runbook"])
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"web.example.com", "cert-manager.io/cluster-issuer: letsencrypt", "web-data"} {
		if !strings.Contains(string(runbook), want) {
			t.Errorf("the runbook lacks %s", want)
		}
	}
}

func TestAnAppWithoutRoutesOrVolumesGetsAShorterRunbook(t *testing.T) {
	m := exportMaterial(t)
	m.Resources = jsonValue(t, `[{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"w"}}]`)
	m.Secrets = nil
	doc := generic(t, httpapi.ExportDocument(exportSubject(), m))
	steps, _ := doc["runbook"].([]any)
	if len(steps) != 5 {
		t.Errorf("steps: %v", steps)
	}
	runbook, err := json.Marshal(steps)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(runbook), "Gateway kuben-system") {
		t.Errorf("a routing step: %s", runbook)
	}
	// Explicit nulls where Rust wrote them.
	if refs, _ := doc["references"].(map[string]any); refs["secrets"] == nil || jsonAt(doc, "release", "source") != nil {
		t.Errorf("references: %v", refs)
	}
}

// The runbook steps every export has, from routes/apps/export.rs.
const (
	stepSecrets = "Secret values are not in this export. The Secrets listed under references.secrets stay in " +
		"namespace shop-prod; once detached they lose Kuben's managed-by label and are never garbage-collected. " +
		"Rotate them yourself from now on, registry pull Secrets included."
	stepApply = "To run the app elsewhere, copy those Secrets first, then apply `manifests` " +
		"(for example `jq .manifests export.json | kubectl apply -f -`)."
	stepExternal = "The app's database and any other external service are not managed by Kuben: their lifecycle, " +
		"credentials and backups stay as they are."
	stepNewApp = "A new app named `web` in this environment would take these objects over: pick another name, or " +
		"remove the objects first."
	stepRelease = "Release the detached app once you own it for good: afterwards, deleting the environment deletes " +
		"its namespace with everything in it."
)

func TestTheRunbookSaysWhatRustSaid(t *testing.T) {
	doc := generic(t, httpapi.ExportDocument(exportSubject(), exportMaterial(t)))
	want := []any{
		stepSecrets, stepApply, stepExternal,
		"Routing: the HTTPRoute serves web.example.com through Gateway kuben-system/kuben. While Kuben runs, " +
			"its Gateway keeps serving these hostnames. Before you uninstall Kuben, attach the route to a Gateway " +
			"you own whose HTTPS listeners carry `cert-manager.io/cluster-issuer: letsencrypt`, so certificates " +
			"keep renewing.",
		"Volumes web-data are kept: Kuben never deletes them after a detach. Back them up before you move the app.",
		stepNewApp, stepRelease,
	}
	if diff := cmp.Diff(want, doc["runbook"]); diff != "" {
		t.Errorf("runbook (-want +got):\n%s", diff)
	}
}

// The whole document as serde_json printed it (keys sorted, explicit
// nulls, empty lists), for a plan without routes, volumes or issuer.
func TestTheExportWireFormIsPinned(t *testing.T) {
	m := exportMaterial(t)
	m.Resources = jsonValue(t, `[{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"w"}}]`)
	m.Capabilities = jsonValue(t, `{"gateway":"kuben-system/kuben"}`)
	got, err := jsonx.CanonicalValue(httpapi.ExportDocument(exportSubject(), m))
	if err != nil {
		t.Fatal(err)
	}
	runbook, err := json.Marshal([]string{stepSecrets, stepApply, stepExternal, stepNewApp, stepRelease})
	if err != nil {
		t.Fatal(err)
	}
	const nilID = "00000000-0000-0000-0000-000000000000"
	want := `{"app":{"app":"web","delivery":"controller","environment":"prod","namespace":"shop-prod","project":"shop"},` +
		`"config":{"revision":2,"spec":{"replicas":2}},` +
		`"exportedAt":"2026-09-17T00:00:00Z",` +
		`"format":"kuben.dev/export/v1",` +
		`"inventory":[{"apiVersion":"apps/v1","kind":"Deployment","name":"w","namespace":"shop-prod"},` +
		`{"apiVersion":"v1","kind":"Secret","name":"db.r4","namespace":"shop-prod"}],` +
		`"kuben":"` + version.Version + `",` +
		`"manifests":{"apiVersion":"v1","items":[{"apiVersion":"apps/v1","kind":"Deployment",` +
		`"metadata":{"name":"w","namespace":"shop-prod"}}],"kind":"List"},` +
		`"references":{"clusterIssuer":null,"gateway":"kuben-system/kuben","hostnames":[],` +
		`"secrets":[{"kind":"opaque","name":"db","object":"db.r4","registry":null,"revision":4}],"volumes":[]},` +
		`"release":{"artifacts":{"web":"ghcr.io/acme/web@sha256:aa"},"id":"` + nilID + `",` +
		`"portableConfig":{"env":{"MODE":"prod"}},"processContract":{},"source":null},` +
		`"renderer":{"capabilities":{"gateway":"kuben-system/kuben"},"version":"kuben-renderer/2"},` +
		`"run":{"generation":3,"id":"` + nilID + `","phase":"succeeded","reason":"deploy"},` +
		`"runbook":` + string(runbook) + `}`
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("wire form (-want +got):\n%s", diff)
	}
}

// A capability snapshot without a Gateway: "(none)" in the runbook, null
// in the references (Rust indexed a missing member as null).
func TestAnExportWithoutAGatewaySaysNone(t *testing.T) {
	m := exportMaterial(t)
	m.Capabilities = nil
	doc := generic(t, httpapi.ExportDocument(exportSubject(), m))
	refs, _ := doc["references"].(map[string]any)
	if gw, has := refs["gateway"]; !has || gw != nil {
		t.Errorf("gateway: %v %v", gw, has)
	}
	if issuer, has := refs["clusterIssuer"]; !has || issuer != nil {
		t.Errorf("clusterIssuer: %v %v", issuer, has)
	}
	steps, _ := doc["runbook"].([]any)
	if len(steps) != 7 {
		t.Fatalf("steps: %v", steps)
	}
	want := "Routing: the HTTPRoute serves web.example.com through Gateway (none). While Kuben runs, its Gateway " +
		"keeps serving these hostnames. Before you uninstall Kuben, attach the route to a Gateway you own."
	if diff := cmp.Diff(want, steps[3]); diff != "" {
		t.Errorf("routing step (-want +got):\n%s", diff)
	}
}
