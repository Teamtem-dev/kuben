package preview_test

import (
	"encoding/json"
	"math"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/preview"
)

const (
	hourMs int64 = 3_600_000
	maxMs        = int64(preview.MaxTTLHours) * hourMs
)

func decode(t *testing.T, text string) map[string]any {
	t.Helper()
	var object map[string]any
	if err := json.Unmarshal([]byte(text), &object); err != nil {
		t.Fatal(err)
	}
	return object
}

func encode(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestSlugsCarryTheNumberAndEpoch(t *testing.T) {
	tests := []struct {
		number, epoch uint64
		want          string
		ok            bool
	}{
		{12, 1, "pr12-1", true},
		{12, 2, "pr12-2", true},
		{1_234_567_890, 123_456_789, "", false},
		{1_234_567_890, 1_234_567, "pr1234567890-1234567", true}, // Exactly 20 characters.
		{1_234_567_890, 12_345_678, "", false},
		{0, 0, "pr0-0", true},
		{math.MaxUint64, math.MaxUint64, "", false},
	}
	for _, tt := range tests {
		got, ok := preview.Slug(tt.number, tt.epoch)
		if got != tt.want || ok != tt.ok {
			t.Errorf("slug(%d, %d): got %q, %v", tt.number, tt.epoch, got, ok)
		}
	}
}

func TestLifetimesAreBounded(t *testing.T) {
	expiries := []struct {
		name     string
		now      int64
		ttlHours uint32
		current  opt.Val[int64]
		want     int64
	}{
		{"two hours", 0, 2, opt.None[int64](), 2 * hourMs},
		{"never shortened", 0, 2, opt.Some(5 * hourMs), 5 * hourMs},
		{"lengthened", 0, 6, opt.Some(5 * hourMs), 6 * hourMs},
		{"at most the maximum", 0, 10_000, opt.None[int64](), maxMs},
		{"at least an hour", 0, 0, opt.None[int64](), hourMs},
		{"saturates", math.MaxInt64 - 5, 2, opt.None[int64](), math.MaxInt64},
		{"saturates at the maximum lifetime", math.MaxInt64 - maxMs + 1, math.MaxUint32, opt.Some[int64](7), math.MaxInt64},
		{"does not wrap below", math.MinInt64, 1, opt.None[int64](), math.MinInt64 + hourMs},
	}
	for _, tt := range expiries {
		if got := preview.Expiry(tt.now, tt.ttlHours, tt.current); got != tt.want {
			t.Errorf("expiry %s: got %d, want %d", tt.name, got, tt.want)
		}
	}
	extensions := []struct {
		name           string
		now, expiresAt int64
		hours          uint32
		want           int64
	}{
		{"an expired preview counts from now", 10, 0, 1, 10 + hourMs},
		{"at most the maximum beyond now", 0, hourMs, 10_000, maxMs},
		{"a running preview counts from its expiry", 0, 2 * hourMs, 3, 5 * hourMs},
		{"no hours", 0, hourMs, 0, hourMs},
		{"an expiry beyond the limit is pulled in", 0, maxMs + hourMs, 1, maxMs},
		{"saturates", math.MaxInt64 - 5, math.MaxInt64 - 1, math.MaxUint32, math.MaxInt64},
		{"the largest extension does not wrap", math.MaxInt64 - maxMs, math.MaxInt64 - maxMs, math.MaxUint32, math.MaxInt64},
		{"does not wrap below", math.MinInt64, math.MinInt64, 1, math.MinInt64 + hourMs},
	}
	for _, tt := range extensions {
		if got := preview.Extend(tt.now, tt.expiresAt, tt.hours); got != tt.want {
			t.Errorf("extend %s: got %d, want %d", tt.name, got, tt.want)
		}
	}
}

const sourceConfig = `{
	"runtime": { "processes": { "web": { "port": 8080 } } },
	"env": [
		{ "name": "MODE", "value": "preview" },
		{ "name": "DB", "fromSecret": { "name": "db", "key": "url" } },
		{ "name": "API", "fromService": { "name": "api", "key": "url" } }
	],
	"domains": [{ "host": "shop.example.com" }],
	"imagePullSecrets": ["ghcr.r1"]
}`

func TestPreviewsCopyNoSecretsOrDomains(t *testing.T) {
	source := decode(t, sourceConfig)
	config, removed := preview.Config(source)

	wantEnv := `[{"name":"MODE","value":"preview"},{"fromService":{"key":"url","name":"api"},"name":"API"}]`
	if got := encode(t, config["env"]); got != wantEnv {
		t.Errorf("env: got %s", got)
	}
	if got := encode(t, config["domains"]); got != `[]` {
		t.Errorf("domains: got %s", got)
	}
	if _, ok := config["imagePullSecrets"]; ok {
		t.Error("image pull secrets are not copied")
	}
	if diff := cmp.Diff(source["runtime"], config["runtime"]); diff != "" {
		t.Errorf("runtime (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]string{"env DB", "domain shop.example.com", "image pull secrets"}, removed); diff != "" {
		t.Errorf("removed (-want +got):\n%s", diff)
	}
	want := `{"domains":[],"env":[{"name":"MODE","value":"preview"},{"fromService":{"key":"url","name":"api"},"name":"API"}],` +
		`"runtime":{"processes":{"web":{"port":8080}}}}`
	if got := encode(t, config); got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

func TestTheSourceConfigurationIsNotChanged(t *testing.T) {
	source := decode(t, sourceConfig)
	config, _ := preview.Config(source)
	if diff := cmp.Diff(decode(t, sourceConfig), source); diff != "" {
		t.Fatalf("the source changed (-want +got):\n%s", diff)
	}
	// The copy shares nothing that can be written to with its source.
	web, ok := config["runtime"].(map[string]any)["processes"].(map[string]any)["web"].(map[string]any)
	if !ok {
		t.Fatal("runtime.processes.web is an object")
	}
	web["port"] = 1
	first, ok := config["env"].([]any)[0].(map[string]any)
	if !ok {
		t.Fatal("env[0] is an object")
	}
	first["name"] = "CHANGED"
	if diff := cmp.Diff(decode(t, sourceConfig), source); diff != "" {
		t.Errorf("writing to the copy changed the source (-want +got):\n%s", diff)
	}
}

func TestPreviewConfigEdgeCases(t *testing.T) {
	tests := []struct {
		name        string
		source      string
		want        string
		wantRemoved []string
	}{
		{"nothing to remove", `{"env":[{"name":"A","value":"1"}]}`, `{"env":[{"name":"A","value":"1"}]}`, []string{}},
		{"empty", `{}`, `{}`, []string{}},
		{"a null secret reference is none", `{"env":[{"name":"A","fromSecret":null}]}`, `{"env":[{"fromSecret":null,"name":"A"}]}`, []string{}},
		{"every variable removed", `{"env":[{"name":"A","fromSecret":{}}]}`, `{"env":[]}`, []string{"env A"}},
		{"a secret without a name", `{"env":[{"fromSecret":"s"},{"name":7,"fromSecret":"s"}]}`, `{"env":[]}`, []string{"env ?", "env ?"}},
		{"entries that are not objects are kept", `{"env":["x",3,null,["fromSecret"]]}`, `{"env":["x",3,null,["fromSecret"]]}`, []string{}},
		{"env that is not a list", `{"env":{"fromSecret":"s"}}`, `{"env":{"fromSecret":"s"}}`, []string{}},
		{"domains without a host", `{"domains":[{"host":"a.example"},{},"b",{"host":1}]}`, `{"domains":[]}`, []string{"domain a.example", "domain ?", "domain ?", "domain ?"}},
		{"domains that are not a list", `{"domains":"a.example"}`, `{"domains":"a.example"}`, []string{}},
		{"null domains", `{"domains":null,"env":null}`, `{"domains":null,"env":null}`, []string{}},
		{"null image pull secrets", `{"imagePullSecrets":null}`, `{}`, []string{"image pull secrets"}},
		{"nested keys are left alone", `{"a":{"imagePullSecrets":[1],"domains":[{"host":"h"}]}}`, `{"a":{"domains":[{"host":"h"}],"imagePullSecrets":[1]}}`, []string{}},
		{
			"removed in order", `{"imagePullSecrets":[],"domains":[{"host":"b"},{"host":"a"}],"env":[{"name":"Z","fromSecret":1},{"name":"Y","fromSecret":2}]}`,
			`{"domains":[],"env":[]}`,
			[]string{"env Z", "env Y", "domain b", "domain a", "image pull secrets"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config, removed := preview.Config(decode(t, tt.source))
			if got := encode(t, config); got != tt.want {
				t.Errorf("got  %s\nwant %s", got, tt.want)
			}
			if diff := cmp.Diff(tt.wantRemoved, removed); diff != "" {
				t.Errorf("removed (-want +got):\n%s", diff)
			}
		})
	}
	config, removed := preview.Config(nil)
	if config != nil || len(removed) != 0 {
		t.Errorf("no configuration: got %v, %v", config, removed)
	}
}
