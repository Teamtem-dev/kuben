package httpapi_test

import (
	"testing"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/httpapi/gen"
)

// The 2.1 routes: fixed segments win over the ids beside them.
func TestIntegrationRoutesResolve(t *testing.T) {
	routes, err := gen.NewServer(gen.UnimplementedHandler{})
	if err != nil {
		t.Fatal(err)
	}
	app := "/api/v1/projects/p/environments/e/apps/a"
	cases := []struct{ method, path, want string }{
		{"GET", "/api/v1/git/connections", "listGitConnections"},
		{"POST", "/api/v1/git/connections", "createGitConnection"},
		{"POST", "/api/v1/git/connections/test", "testNewGitConnection"},
		{"GET", "/api/v1/git/connections/c1", "getGitConnection"},
		{"PATCH", "/api/v1/git/connections/c1", "updateGitConnection"},
		{"DELETE", "/api/v1/git/connections/c1", "deleteGitConnection"},
		{"POST", "/api/v1/git/connections/c1/test", "testGitConnection"},
		{"GET", "/api/v1/git/connections/c1/repositories", "listGitConnectionRepositories"},
		{"GET", "/api/v1/git/connections/c1/branches", "listGitConnectionBranches"},
		{"GET", "/api/v1/git/installations", "listGitInstallations"},
		{"GET", "/api/v1/registries", "listOrgRegistries"},
		{"POST", "/api/v1/registries", "createOrgRegistry"},
		{"GET", "/api/v1/registries/presets", "listRegistryPresets"},
		{"POST", "/api/v1/registries/test", "testNewOrgRegistry"},
		{"GET", "/api/v1/registries/r1", "getOrgRegistry"},
		{"PUT", "/api/v1/registries/r1", "updateOrgRegistry"},
		{"DELETE", "/api/v1/registries/r1", "deleteOrgRegistry"},
		{"POST", "/api/v1/registries/r1/test", "testOrgRegistry"},
		{"POST", app + "/builds", "triggerBuild"},
		{"GET", app + "/builds", "listBuilds"},
		{"GET", app + "/builds/b1/logs", "getBuildLogs"},
		{"POST", app + "/builds/b1/cancel", "cancelBuild"},
	}
	for _, c := range cases {
		route, ok := routes.FindRoute(c.method, c.path)
		if !ok || route.OperationID() != c.want {
			t.Errorf("%s %s: %q %v, want %s", c.method, c.path, route.OperationID(), ok, c.want)
		}
	}
}
