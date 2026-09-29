package projection_test

import (
	"encoding/json"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/kube/projection"
)

func buildChange(org string) projection.BuildChanged {
	return projection.BuildChanged{
		Org: org, App: "kb-shop-prod/web", Project: "shop", Environment: "prod", Name: "web",
		Build: json.RawMessage(`{"id":"b1","phase":"running"}`),
	}
}

func TestBuildChangesReachTheirOrganizationOnly(t *testing.T) {
	p := projection.New()
	if p.Subscribed() {
		t.Fatal("nobody yet")
	}
	p.PublishBuild(buildChange("acme"))
	if p.Seq() != 0 {
		t.Fatal("published to nobody")
	}
	sub := p.Subscribe()
	defer sub.Close()
	p.PublishBuild(buildChange("acme"))
	p.PublishBuild(buildChange("other"))
	mine := projection.NewVisibility([]string{"acme"})
	var admitted []string
	for range 2 {
		got, ok := sub.TryRecv()
		if !ok {
			t.Fatal("a delta is missing")
		}
		if mine.Admit(got.Delta) {
			data, err := got.Delta.MarshalJSON()
			if err != nil {
				t.Fatal(err)
			}
			admitted = append(admitted, string(data))
		}
	}
	want := []string{`{"kind":"build","seq":1,"org":"acme","app":"kb-shop-prod/web","project":"shop",` +
		`"environment":"prod","name":"web","build":{"id":"b1","phase":"running"}}`}
	if diff := cmp.Diff(want, admitted); diff != "" {
		t.Error(diff)
	}
}
