package support_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/Teamtem-dev/kuben/go/hub/internal/core/kerr"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/support"
)

func TestVersionsParseToMajorAndMinor(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want support.Minor
		ok   bool
	}{
		{"v1.36.4+k3s1", support.Minor{Major: 1, Minor: 36}, true},
		{"1.31", support.Minor{Major: 1, Minor: 31}, true},
		{"17-alpine", support.Minor{Major: 17}, true},
		{"v1.30.0-eks-abc", support.Minor{Major: 1, Minor: 30}, true},
		{"1.32+", support.Minor{Major: 1, Minor: 32}, true},
		{" 17 ", support.Minor{Major: 17}, true},
		{"1..5", support.Minor{Major: 1}, true},
		{"1.99999999999", support.Minor{Major: 1}, true},
		{"x", support.Minor{}, false},
		{"", support.Minor{}, false},
		{".5", support.Minor{}, false},
		{"+1.2", support.Minor{}, false},
		{"99999999999.1", support.Minor{}, false},
	} {
		got, ok := support.ParseMinor(tc.in)
		if got != tc.want || ok != tc.ok {
			t.Errorf("%q: got %v, %v", tc.in, got, ok)
		}
	}
	if got := (support.Minor{Major: 1, Minor: 36}).String(); got != "1.36" {
		t.Errorf("got %s", got)
	}
}

func TestVersionsFitTheEnvelope(t *testing.T) {
	for _, tc := range []struct {
		r    support.VersionRange
		v    support.Minor
		want support.Fit
	}{
		{support.Kubernetes(), support.Minor{Major: 1, Minor: 33}, support.Supported},
		{support.Kubernetes(), support.Minor{Major: 1, Minor: 30}, support.Untested},
		{support.Kubernetes(), support.Minor{Major: 1, Minor: 40}, support.Untested},
		{support.Kubernetes(), support.Minor{Major: 1, Minor: 28}, support.Unsupported},
		{support.PostgreSQL(), support.Minor{Major: 17}, support.Supported},
		{support.PostgreSQL(), support.Minor{Major: 14}, support.Unsupported},
		{support.PostgreSQL(), support.Minor{Major: 19}, support.Untested},
	} {
		if got := tc.r.Fit(tc.v); got != tc.want {
			t.Errorf("%s %s: got %s, want %s", tc.r.Name, tc.v, got, tc.want)
		}
	}
	fit, text := support.Kubernetes().Describe(support.Minor{Major: 1, Minor: 28})
	if fit != support.Unsupported || !strings.Contains(text, "1.29 or newer") {
		t.Errorf("got %s, %s", fit, text)
	}
	_, text = support.PostgreSQL().Describe(support.Minor{Major: 14})
	if text != "PostgreSQL 14 is not supported: use 15 or newer (tested 15 to 18)" {
		t.Errorf("got %s", text)
	}
}

func TestDescribePinsEveryLine(t *testing.T) {
	for _, tc := range []struct {
		v    support.Minor
		fit  support.Fit
		text string
	}{
		{support.Minor{Major: 1, Minor: 33}, support.Supported, "Kubernetes 1.33 is supported (tested 1.31 to 1.36)"},
		{
			support.Minor{Major: 1, Minor: 40}, support.Untested,
			"Kubernetes 1.40 is outside the tested range 1.31 to 1.36; it may work, but it is not supported",
		},
		{
			support.Minor{Major: 1, Minor: 28}, support.Unsupported,
			"Kubernetes 1.28 is not supported: use 1.29 or newer (tested 1.31 to 1.36)",
		},
	} {
		fit, text := support.Kubernetes().Describe(tc.v)
		if fit != tc.fit || text != tc.text {
			t.Errorf("%s: got %s, %q", tc.v, fit, text)
		}
	}
}

func TestTheEnvelopeIsComplete(t *testing.T) {
	e := support.Envelope()
	k8s, _ := e["kubernetes"].(map[string]any)
	if diff := cmp.Diff([]any{"1.31", "1.36"}, k8s["tested"]); diff != "" {
		t.Error(diff)
	}
	pg, _ := e["postgresql"].(map[string]any)
	if diff := cmp.Diff([]any{"15", "18"}, pg["tested"]); diff != "" {
		t.Error(diff)
	}
	if limits, _ := e["limits"].([]any); len(limits) != len(support.Limits()) {
		t.Errorf("got %d limits", len(limits))
	}
	for _, r := range []support.VersionRange{
		support.Kubernetes(), support.PostgreSQL(), support.GatewayAPI(), support.CertManager(),
	} {
		if r.WorksFrom.Compare(r.Tested[0]) > 0 || r.Tested[0].Compare(r.Tested[1]) > 0 {
			t.Errorf("%s: the range is not ordered", r.Name)
		}
	}
}

func TestTheEnvelopeKeepsItsJSON(t *testing.T) {
	raw, err := json.Marshal(support.Envelope())
	if err != nil {
		t.Fatal(err)
	}
	want := `{"architectures":["amd64","arm64"],` +
		`"certManager":{"name":"cert-manager","tested":["1.16","1.21"],"worksFrom":"1.15"},` +
		`"gatewayApi":{"name":"Gateway API","tested":["1.2","1.5"],"worksFrom":"1.1"},` +
		`"hosts":["Ubuntu 22.04, 24.04","Debian 12, 13","RHEL, Rocky and AlmaLinux 9, 10"],` +
		`"kubernetes":{"name":"Kubernetes","tested":["1.31","1.36"],"worksFrom":"1.29"},` +
		`"limits":[{"area":"clusters","claim":"one cluster per installation"},` +
		`{"area":"placements","claim":"one placement (namespace) per environment"},` +
		`{"area":"workloads","claim":"stateless web and worker processes; production data in an external PostgreSQL"},` +
		`{"area":"availability","claim":"single failure domain: no high-availability claim (M7)"},` +
		`{"area":"nodes","claim":"node lifecycle (add, drain, replace) is the operator's (M6)"},` +
		`{"area":"previews","claim":"one preview environment per GitHub pull request, with a lifetime; a fork's preview gets no secrets"},` +
		`{"area":"registries","claim":"OCI registries with basic or token auth; GitHub for Git sources"},` +
		`{"area":"gateways","claim":"one Gateway API implementation; Traefik (k3s) is the tested one"}],` +
		`"postgresql":{"name":"PostgreSQL","tested":["15","18"],"worksFrom":"15"},` +
		`"profile":"supported-mvp"}`
	if diff := cmp.Diff(want, string(raw)); diff != "" {
		t.Error(diff)
	}
}

func TestFitsParseAndPinTheirStrings(t *testing.T) {
	for fit, s := range map[support.Fit]string{
		support.Supported: "supported", support.Untested: "untested", support.Unsupported: "unsupported",
	} {
		raw, err := json.Marshal(fit)
		if err != nil || string(raw) != `"`+s+`"` || fit.String() != s {
			t.Errorf("%s: %s, %v", s, raw, err)
		}
		if got, perr := support.ParseFit(s); perr != nil || got != fit {
			t.Errorf("%s: got %s, %v", s, got, perr)
		}
	}
	if _, err := support.ParseFit("Supported"); !errors.Is(err, kerr.ErrValidation) {
		t.Errorf("got %v", err)
	}
}
