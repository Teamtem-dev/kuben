package api

// One-click templates (scenario 8, routes/templates.rs): a small, reviewed
// catalogue of single-process services. Generated credentials are stored in
// a Secret named `<app>-credentials`; the App only *references* it, so no
// secret value ever becomes part of an App spec, a release snapshot or the
// audit log.

import (
	"context"
	"crypto/rand"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Teamtem-dev/kuben/go/hub/internal/api/gen"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/kerr"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/perm"
	"github.com/Teamtem-dev/kuben/go/kubenapi/v1alpha1"
)

// SecretLen is the length of generated passwords and keys (alphanumeric, ~190 bits).
const SecretLen = 32

const alphanumericChars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

// valKind tells a literal environment value from a generated secret.
type valKind int

const (
	valLit valKind = iota
	valSec
)

// envVal is a template environment value: literal text, or the key of a
// generated secret.
type envVal struct {
	Kind valKind
	Text string
}

func lit(s string) envVal { return envVal{Kind: valLit, Text: s} }
func sec(s string) envVal { return envVal{Kind: valSec, Text: s} }

// templateVolume is a volume a template mounts.
type templateVolume struct {
	Name      string
	MountPath string
	Size      string
}

// templateDerived is a secret value built from a pattern over the
// generated ones.
type templateDerived struct {
	Key     string
	Pattern string
}

// templateEnv is one environment variable of a template.
type templateEnv struct {
	Name string
	Val  envVal
}

// TemplateDef is a one-click service or database of the catalogue
// (routes/templates.rs).
type TemplateDef struct {
	ID          string
	Name        string
	Description string
	Category    string
	Image       string
	Port        uint16
	Protocol    v1alpha1.Protocol
	Command     []string
	Env         []templateEnv
	Generated   []string
	Derived     []templateDerived
	Volumes     []templateVolume
	Health      string
	Size        string
	FSGroup     *int64
}

func int64Ptr(v int64) *int64 { return &v }

var templatesCatalogue = []TemplateDef{
	{
		ID:          "postgres",
		Name:        "PostgreSQL 17",
		Description: "Relational database. Other apps connect with the `url` key of the credentials secret.",
		Category:    "database",
		Image:       "postgres:17-alpine",
		Port:        5432,
		Protocol:    v1alpha1.ProtocolTCP,
		Command:     nil,
		Env: []templateEnv{
			{"POSTGRES_USER", lit("app")},
			{"POSTGRES_DB", lit("app")},
			{"POSTGRES_PASSWORD", sec("password")},
			{"PGDATA", lit("/var/lib/postgresql/data/pgdata")},
		},
		Generated: []string{"password"},
		Derived: []templateDerived{
			{"url", "postgres://app:{password}@{name}:5432/app"},
			{"host", "{name}"},
			{"port", "5432"},
			{"username", "app"},
			{"database", "app"},
		},
		Volumes: []templateVolume{{"data", "/var/lib/postgresql/data", "5Gi"}},
		Health:  "",
		Size:    "small",
		FSGroup: nil,
	},
	{
		ID:          "redis",
		Name:        "Redis 7",
		Description: "In-memory cache and queue with append-only persistence and a password.",
		Category:    "database",
		Image:       "redis:7-alpine",
		Port:        6379,
		Protocol:    v1alpha1.ProtocolTCP,
		Command: []string{
			"sh",
			"-c",
			"exec redis-server --appendonly yes --requirepass \"$REDIS_PASSWORD\"",
		},
		Env:       []templateEnv{{"REDIS_PASSWORD", sec("password")}},
		Generated: []string{"password"},
		Derived: []templateDerived{
			{"url", "redis://:{password}@{name}:6379/0"},
			{"host", "{name}"},
			{"port", "6379"},
		},
		Volumes: []templateVolume{{"data", "/data", "1Gi"}},
		Health:  "",
		Size:    "nano",
		FSGroup: nil,
	},
	{
		ID:          "mariadb",
		Name:        "MariaDB 11",
		Description: "MySQL-compatible database. Connect with the `url` key of the credentials secret.",
		Category:    "database",
		Image:       "mariadb:11",
		Port:        3306,
		Protocol:    v1alpha1.ProtocolTCP,
		Command:     nil,
		Env: []templateEnv{
			{"MARIADB_DATABASE", lit("app")},
			{"MARIADB_USER", lit("app")},
			{"MARIADB_PASSWORD", sec("password")},
			{"MARIADB_ROOT_PASSWORD", sec("root-password")},
		},
		Generated: []string{"password", "root-password"},
		Derived: []templateDerived{
			{"url", "mysql://app:{password}@{name}:3306/app"},
			{"host", "{name}"},
			{"port", "3306"},
			{"username", "app"},
			{"database", "app"},
		},
		Volumes: []templateVolume{{"data", "/var/lib/mysql", "5Gi"}},
		Health:  "",
		Size:    "medium",
		FSGroup: nil,
	},
	{
		ID:          "n8n",
		Name:        "n8n",
		Description: "Workflow automation. Credentials are encrypted with a generated key.",
		Category:    "automation",
		Image:       "n8nio/n8n:stable",
		Port:        5678,
		Protocol:    v1alpha1.ProtocolHTTP,
		Command:     nil,
		Env: []templateEnv{
			{"N8N_ENCRYPTION_KEY", sec("encryption-key")},
			{"N8N_PORT", lit("5678")},
			{"GENERIC_TIMEZONE", lit("UTC")},
		},
		Generated: []string{"encryption-key"},
		Derived:   nil,
		Volumes:   []templateVolume{{"data", "/home/node/.n8n", "1Gi"}},
		Health:    "/healthz",
		Size:      "small",
		FSGroup:   int64Ptr(1000),
	},
	{
		ID:          "uptime-kuma",
		Name:        "Uptime Kuma",
		Description: "Self-hosted uptime monitoring with status pages.",
		Category:    "monitoring",
		Image:       "louislam/uptime-kuma:1",
		Port:        3001,
		Protocol:    v1alpha1.ProtocolHTTP,
		Command:     nil,
		Env:         nil,
		Generated:   nil,
		Derived:     nil,
		Volumes:     []templateVolume{{"data", "/app/data", "1Gi"}},
		Health:      "",
		Size:        "small",
		FSGroup:     nil,
	},
	{
		ID:          "vaultwarden",
		Name:        "Vaultwarden",
		Description: "Bitwarden-compatible password manager. Sign-ups are closed; invite users from /admin with the generated admin token.",
		Category:    "security",
		Image:       "vaultwarden/server:latest",
		Port:        80,
		Protocol:    v1alpha1.ProtocolHTTP,
		Command:     nil,
		Env: []templateEnv{
			{"SIGNUPS_ALLOWED", lit("false")},
			{"ADMIN_TOKEN", sec("admin-token")},
		},
		Generated: []string{"admin-token"},
		Derived:   nil,
		Volumes:   []templateVolume{{"data", "/data", "1Gi"}},
		Health:    "/alive",
		Size:      "small",
		FSGroup:   nil,
	},
	{
		ID:          "gitea",
		Name:        "Gitea",
		Description: "Lightweight Git hosting (rootless image). Finish the installer on first visit.",
		Category:    "development",
		Image:       "gitea/gitea:1-rootless",
		Port:        3000,
		Protocol:    v1alpha1.ProtocolHTTP,
		Command:     nil,
		Env:         nil,
		Generated:   nil,
		Derived:     nil,
		Volumes: []templateVolume{
			{"data", "/var/lib/gitea", "5Gi"},
			{"config", "/etc/gitea", "100Mi"},
		},
		Health:  "/api/healthz",
		Size:    "small",
		FSGroup: int64Ptr(1000),
	},
	{
		ID:          "whoami",
		Name:        "whoami",
		Description: "Tiny HTTP echo service to test domains, TLS and routing.",
		Category:    "sample",
		Image:       "traefik/whoami:v1.10",
		Port:        8080,
		Protocol:    v1alpha1.ProtocolHTTP,
		Command:     []string{"/whoami", "--port", "8080"},
		Env:         nil,
		Generated:   nil,
		Derived:     nil,
		Volumes:     nil,
		Health:      "",
		Size:        "nano",
		FSGroup:     nil,
	},
}

// CredentialsSecret is the name of the Secret holding a template app's generated credentials.
func CredentialsSecret(app string) string {
	return fmt.Sprintf("%s-credentials", app)
}

func randomSecret() (string, error) {
	const maxByte = 256 - (256 % len(alphanumericChars))
	b := make([]byte, 1)
	out := make([]byte, SecretLen)
	for i := 0; i < SecretLen; {
		if _, err := rand.Read(b); err != nil {
			return "", err
		}
		if int(b[0]) < maxByte {
			out[i] = alphanumericChars[int(b[0])%len(alphanumericChars)]
			i++
		}
	}
	return string(out), nil
}

// RenderedTemplate is a template made into an app: its spec and the values
// of the Secret it reads.
type RenderedTemplate struct {
	Spec   v1alpha1.AppSpec
	Secret map[string]string
}

func renderTemplate(t TemplateDef, app string) (RenderedTemplate, error) {
	secret := make(map[string]string, len(t.Generated)+len(t.Derived))
	for _, k := range t.Generated {
		val, err := randomSecret()
		if err != nil {
			return RenderedTemplate{}, err
		}
		secret[k] = val
	}
	for _, d := range t.Derived {
		val := strings.ReplaceAll(d.Pattern, "{name}", app)
		for _, k := range t.Generated {
			if secVal, ok := secret[k]; ok {
				val = strings.ReplaceAll(val, "{"+k+"}", secVal)
			}
		}
		secret[d.Key] = val
	}

	secretName := CredentialsSecret(app)
	envVars := make([]v1alpha1.EnvVar, len(t.Env))
	for i, e := range t.Env {
		switch e.Val.Kind {
		case valLit:
			v := e.Val.Text
			envVars[i] = v1alpha1.EnvVar{Name: e.Name, Value: &v}
		case valSec:
			envVars[i] = v1alpha1.EnvVar{
				Name: e.Name,
				FromSecret: &v1alpha1.KeyRef{
					Name: secretName,
					Key:  e.Val.Text,
				},
			}
		}
	}

	volumes := make([]v1alpha1.Volume, len(t.Volumes))
	for i, v := range t.Volumes {
		volumes[i] = v1alpha1.Volume{
			Name:      v.Name,
			MountPath: v.MountPath,
			Size:      v.Size,
		}
	}

	var healthCheck *v1alpha1.HealthCheck
	if t.Health != "" {
		healthCheck = &v1alpha1.HealthCheck{Path: t.Health}
	}

	port := t.Port
	spec := v1alpha1.AppSpec{
		Source: v1alpha1.SourceFromImage(t.Image),
		Runtime: v1alpha1.Runtime{
			Processes: map[string]v1alpha1.Process{
				"web": {
					Command:  t.Command,
					Port:     &port,
					Size:     t.Size,
					Replicas: v1alpha1.DefaultReplicas(),
					Protocol: t.Protocol,
				},
			},
			HealthCheck: healthCheck,
			FSGroup:     t.FSGroup,
		},
		Env:     envVars,
		Volumes: volumes,
	}

	return RenderedTemplate{Spec: spec, Secret: secret}, nil
}

func templateDto(t TemplateDef) gen.TemplateDto {
	keys := make([]string, 0, len(t.Generated)+len(t.Derived))
	keys = append(keys, t.Generated...)
	for _, d := range t.Derived {
		keys = append(keys, d.Key)
	}
	sort.Strings(keys)

	volumes := make([]string, len(t.Volumes))
	for i, v := range t.Volumes {
		volumes[i] = fmt.Sprintf("%s (%s)", v.MountPath, v.Size)
	}

	protocol := "http"
	if !t.Protocol.IsHTTP() {
		protocol = "tcp"
	}

	return gen.TemplateDto{
		Category:       t.Category,
		ConnectionKeys: keys,
		Description:    t.Description,
		ID:             t.ID,
		Image:          t.Image,
		Name:           t.Name,
		Port:           int32(t.Port),
		Protocol:       protocol,
		Volumes:        volumes,
	}
}

func findTemplate(id string) (TemplateDef, bool) {
	for _, t := range templatesCatalogue {
		if t.ID == id {
			return t, true
		}
	}
	return TemplateDef{}, false
}

// ListTemplates returns the template catalogue.
func (s *Server) ListTemplates(ctx context.Context) ([]gen.TemplateDto, error) {
	if _, err := s.access(ctx); err != nil {
		return nil, err
	}
	dtos := make([]gen.TemplateDto, len(templatesCatalogue))
	for i, t := range templatesCatalogue {
		dtos[i] = templateDto(t)
	}
	return dtos, nil
}

// DeployTemplate deploys a template into an environment.
func (s *Server) DeployTemplate(
	ctx context.Context, req *gen.DeployTemplate, params gen.DeployTemplateParams,
) (gen.DeployTemplateRes, error) {
	if req == nil {
		return nil, kerr.New(kerr.Validation, "missing request body")
	}
	acc, err := s.access(ctx)
	if err != nil {
		return nil, err
	}
	e, err := s.findEnvironment(ctx, acc, params.Project, params.Environment)
	if err != nil {
		return nil, err
	}
	chain := e.chain()
	if _, err := acc.Require(perm.AppWrite, chain); err != nil {
		return nil, err //nolint:wrapcheck // a kerr already
	}
	if _, err := acc.Require(perm.SecretWrite, chain); err != nil {
		return nil, err //nolint:wrapcheck // a kerr already
	}
	t, found := findTemplate(params.Template)
	if !found {
		return nil, scopeNotFound("template", params.Template)
	}
	if err := DNSLabel("name", req.Name, 40); err != nil {
		return nil, err
	}
	rendered, err := renderTemplate(t, req.Name)
	if err != nil {
		return nil, err
	}
	if err := validateSpec(&rendered.Spec); err != nil {
		return nil, err
	}

	cluster, err := s.cluster()
	if err != nil {
		return nil, err
	}

	secretName := CredentialsSecret(req.Name)
	namespace := e.namespace()
	secrets := cluster.Typed.CoreV1().Secrets(namespace)

	_, err = secrets.Get(ctx, secretName, metav1.GetOptions{})
	if err == nil {
		return nil, kerr.New(kerr.Conflict, "secret `%s` already exists", secretName)
	} else if !apierrors.IsNotFound(err) {
		return nil, kubeError(err, secretName)
	}

	secretData := make(map[string][]byte, len(rendered.Secret))
	for k, v := range rendered.Secret {
		secretData[k] = []byte(v)
	}
	secObj := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: namespace,
			Labels: map[string]string{
				v1alpha1.LabelManagedBy:   v1alpha1.LabelManagerValue,
				v1alpha1.LabelEnvironment: e.resourceName(),
				v1alpha1.LabelApp:         req.Name,
			},
		},
		Type: corev1.SecretTypeOpaque,
		Data: secretData,
	}
	if _, err := secrets.Create(ctx, secObj, metav1.CreateOptions{}); err != nil {
		return nil, kubeError(err, secretName)
	}

	appDto, err := s.createApp(ctx, acc, e, req.Name, rendered.Spec)
	if err != nil {
		// Do not leave orphaned credentials behind.
		_ = secrets.Delete(ctx, secretName, metav1.DeleteOptions{})
		return nil, err
	}

	return &gen.DeployedTemplate{
		App:               appDto,
		CredentialsSecret: secretName,
		ConnectionKeys:    templateDto(t).ConnectionKeys,
	}, nil
}
