package api_test

import (
	"math/big"
	"testing"

	"github.com/Teamtem-dev/kuben/go/hub/internal/api"
	"github.com/Teamtem-dev/kuben/go/hub/internal/api/gen"
	"github.com/Teamtem-dev/kuben/go/kubenapi/v1alpha1"
)

// sampleSpec is apps/mod.rs sample_spec: a one-process web app.
func sampleSpec(t *testing.T) v1alpha1.AppSpec {
	t.Helper()
	spec, err := v1alpha1.DecodeAppSpec([]byte(`{"source":{"image":"nginx:1.27"},` +
		`"runtime":{"processes":{"web":{"port":80,"replicas":{"min":1,"max":1}}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	return spec
}

// spec.rs update_changes_only_given_fields.
func TestUpdateChangesOnlyGivenFields(t *testing.T) {
	s := sampleSpec(t)
	u := gen.UpdateApp{Image: gen.NewOptNilString(" nginx:1.28 "), Replicas: gen.NewOptNilInt32(3)}
	if err := api.ApplyUpdate(&s, &u); err != nil {
		t.Fatal(err)
	}
	if s.Source.Image == nil || *s.Source.Image != "nginx:1.28" {
		t.Fatalf("image: %+v", s.Source)
	}
	web := s.Runtime.Processes["web"]
	if web.Replicas.Min != 3 || web.Replicas.Max != 3 || web.Port == nil || *web.Port != 80 {
		t.Fatalf("web: %+v", web)
	}
}

// spec.rs update_validates.
func TestUpdateValidates(t *testing.T) {
	env := func(vars ...gen.EnvVarDto) gen.OptNilEnvVarDtoArray { return gen.NewOptNilEnvVarDtoArray(vars) }
	bad := []gen.UpdateApp{
		{Replicas: gen.NewOptNilInt32(500)},
		{Env: env(
			gen.EnvVarDto{Name: "A", Value: gen.NewOptNilString("1")},
			gen.EnvVarDto{Name: "A", Value: gen.NewOptNilString("2")},
		)},
		{Env: env(gen.EnvVarDto{
			Name: "B", Value: gen.NewOptNilString("x"),
			Secret: gen.NewOptNilSecretRef(gen.SecretRef{Name: "s", Key: "k"}),
		})},
		{HealthCheckPath: gen.NewOptNilString("healthz")},
		{Schedule: gen.NewOptNilString("every night")},
		{Port: gen.NewOptNilInt32(70000)},
	}
	for i, u := range bad {
		s := sampleSpec(t)
		if err := api.ApplyUpdate(&s, &u); err == nil {
			t.Errorf("update %d is accepted", i)
		}
	}
}

// spec.rs env_values_are_hidden_without_permission.
func TestEnvValuesAreHiddenWithoutPermission(t *testing.T) {
	prod := "prod"
	plain := v1alpha1.EnvVar{Name: "MODE", Value: &prod}
	if v, ok := api.FromCRDEnv(plain, true).Value.Get(); !ok || v != "prod" {
		t.Fatal("with permission")
	}
	if api.FromCRDEnv(plain, false).Value.IsSet() {
		t.Fatal("without permission")
	}
	secret := api.ToCRDEnv(gen.EnvVarDto{
		Name: "TOKEN", Value: gen.NewOptNilString("ignored"),
		Secret: gen.NewOptNilSecretRef(gen.SecretRef{Name: "api", Key: "token"}),
	})
	if secret.Value != nil {
		t.Fatal("secret-backed vars never carry a value")
	}
}

func volume(name, path, size string) gen.VolumeDto {
	return gen.VolumeDto{Name: name, MountPath: path, Size: gen.NewOptString(size)}
}

// spec.rs quantities_and_volume_rules.
func TestQuantitiesAndVolumeRules(t *testing.T) {
	for text, want := range map[string]int64{"5Gi": 5 << 30, "500Mi": 500 << 20, "10G": 10_000_000_000} {
		if got, ok := api.QuantityBytes(text); !ok || got.Cmp(big.NewInt(want)) != 0 {
			t.Errorf("%s: %v", text, got)
		}
	}
	if _, ok := api.QuantityBytes("1.5Gi"); ok {
		t.Error("1.5Gi")
	}
	if err := api.ValidateVolumes([]gen.VolumeDto{volume("data", "/data", "5Gi")}); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []gen.VolumeDto{
		volume("data", "/", "1Gi"), volume("data", "data", "1Gi"), volume("data", "/d", "lots"), volume("Data", "/d", "1Gi"),
	} {
		if api.ValidateVolumes([]gen.VolumeDto{bad}) == nil {
			t.Errorf("%+v", bad)
		}
	}
	if api.ValidateVolumes([]gen.VolumeDto{volume("a", "/x", "1Gi"), volume("b", "/x", "1Gi")}) == nil {
		t.Error("duplicate path")
	}
	current := api.ToCRDVolumes([]gen.VolumeDto{volume("data", "/data", "5Gi")})
	if err := api.CheckNoShrink(current, []gen.VolumeDto{volume("data", "/data", "10Gi")}); err != nil {
		t.Error(err)
	}
	if api.CheckNoShrink(current, []gen.VolumeDto{volume("data", "/data", "1Gi")}) == nil {
		t.Error("shrink")
	}
}

func createBody(schedule string, port int32, replicas int32, volumes []gen.VolumeDto) gen.CreateApp {
	body := gen.CreateApp{
		Name: "api", Image: gen.NewOptNilString("nginx:1.27"), Replicas: gen.NewOptInt32(replicas),
		Size: gen.NewOptString("small"), Volumes: volumes,
	}
	if schedule != "" {
		body.Schedule = gen.NewOptNilString(schedule)
	}
	if port != 0 {
		body.Port = gen.NewOptNilInt32(port)
	}
	return body
}

// spec.rs create_spec_applies_controller_rules.
func TestCreateSpecAppliesControllerRules(t *testing.T) {
	body := createBody("0 3 * * *", 0, 1, nil)
	job, err := api.SpecFromCreate(&body)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := job.Runtime.Processes["job"]; !ok {
		t.Fatalf("%+v", job.Runtime)
	}
	if err := api.ValidateSpec(&job); err != nil {
		t.Fatal(err)
	}
	body = createBody("@daily", 80, 1, nil)
	bad, err := api.SpecFromCreate(&body)
	if err != nil || api.ValidateSpec(&bad) == nil {
		t.Fatal("scheduled with port")
	}
	body = createBody("", 80, 2, []gen.VolumeDto{volume("data", "/data", "1Gi")})
	scaled, err := api.SpecFromCreate(&body)
	if err != nil || api.ValidateSpec(&scaled) == nil {
		t.Fatal("volume needs one replica")
	}

	git := createBody("", 8080, 1, nil)
	git.Image = gen.OptNilString{}
	git.Git = gen.NewOptNilPutSource(gen.PutSource{
		InstallationId: 7, Repository: "acme/shop", Branch: "main", ImageRepository: "ghcr.io/acme/shop",
	})
	fromGit, err := api.SpecFromCreate(&git)
	if err != nil || fromGit.Source.Image != nil || fromGit.Source.Git == nil || fromGit.Source.Git.Repo != "acme/shop" {
		t.Fatalf("git: %+v %v", fromGit.Source, err)
	}
	if err := api.ValidateSpec(&fromGit); err != nil {
		t.Fatalf("a git app awaits its build: %v", err)
	}
	both := git
	both.Image = gen.NewOptNilString("nginx:1.27")
	if _, err := api.SpecFromCreate(&both); err == nil {
		t.Fatal("image and git together")
	}
	both.Git = gen.OptNilPutSource{}
	both.Image = gen.NewOptNilString("  ")
	if _, err := api.SpecFromCreate(&both); err == nil {
		t.Fatal("neither")
	}
}
