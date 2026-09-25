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
	"slices"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Teamtem-dev/kuben/api/v1alpha1"
	"github.com/Teamtem-dev/kuben/internal/core/kerrors"
	"github.com/Teamtem-dev/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/internal/core/perm"
	"github.com/Teamtem-dev/kuben/internal/httpapi/gen"
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
	// Health is the path of the HTTP health check, if the template has one.
	Health opt.Val[string]
	Size   string
	// FSGroup is set for images that run as a non-root user and must write
	// their volume.
	FSGroup opt.Val[int64]
}

// templatesCatalogue is the reviewed catalogue (TEMPLATES). It is built
// afresh on every call, so no caller can change what another one sees.
//
//nolint:funlen // one literal: the catalogue itself
func templatesCatalogue() []TemplateDef {
	return []TemplateDef{
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
			Health:  opt.None[string](),
			Size:    "small",
			FSGroup: opt.None[int64](),
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
			Health:  opt.None[string](),
			Size:    "nano",
			FSGroup: opt.None[int64](),
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
			Health:  opt.None[string](),
			Size:    "medium",
			FSGroup: opt.None[int64](),
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
			Health:    opt.Some("/healthz"),
			Size:      "small",
			FSGroup:   opt.Some[int64](1000),
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
			Health:      opt.None[string](),
			Size:        "small",
			FSGroup:     opt.None[int64](),
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
			Health:    opt.Some("/alive"),
			Size:      "small",
			FSGroup:   opt.None[int64](),
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
			Health:  opt.Some("/api/healthz"),
			Size:    "small",
			FSGroup: opt.Some[int64](1000),
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
			Health:      opt.None[string](),
			Size:        "nano",
			FSGroup:     opt.None[int64](),
		},
	}
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
			return "", fmt.Errorf("generate a credential: %w", err)
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
	if path, ok := t.Health.Get(); ok {
		healthCheck = &v1alpha1.HealthCheck{Path: path}
	}

	port := t.Port
	spec := v1alpha1.AppSpec{
		Source: v1alpha1.SourceFromImage(t.Image),
		Runtime: v1alpha1.Runtime{
			Processes: map[string]v1alpha1.Process{
				"web": {
					Command:  slices.Clone(t.Command),
					Port:     &port,
					Size:     t.Size,
					Replicas: v1alpha1.DefaultReplicas(),
					Protocol: t.Protocol,
				},
			},
			HealthCheck: healthCheck,
			FSGroup:     t.FSGroup.Ptr(),
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
	catalogue := templatesCatalogue()
	if i := slices.IndexFunc(catalogue, func(t TemplateDef) bool { return t.ID == id }); i >= 0 {
		return catalogue[i], true
	}
	return TemplateDef{}, false
}

// ListTemplates returns the template catalogue.
func (s *Server) ListTemplates(ctx context.Context) ([]gen.TemplateDto, error) {
	if _, err := s.access(ctx); err != nil {
		return nil, err
	}
	catalogue := templatesCatalogue()
	dtos := make([]gen.TemplateDto, len(catalogue))
	for i, t := range catalogue {
		dtos[i] = templateDto(t)
	}
	return dtos, nil
}

// DeployTemplate deploys a template into an environment.
func (s *Server) DeployTemplate(
	ctx context.Context, req *gen.DeployTemplate, params gen.DeployTemplateParams,
) (gen.DeployTemplateRes, error) {
	if req == nil {
		return nil, kerrors.New(kerrors.Validation, "missing request body")
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
		return nil, err //nolint:wrapcheck // a kerrors already
	}
	if _, err := acc.Require(perm.SecretWrite, chain); err != nil {
		return nil, err //nolint:wrapcheck // a kerrors already
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

	secretName := CredentialsSecret(req.Name)
	secrets, err := s.createCredentials(ctx, e, req.Name, rendered.Secret)
	if err != nil {
		return nil, err
	}

	appDto, err := s.createApp(ctx, acc, e, req.Name, rendered.Spec)
	if err != nil {
		s.dropOrphanedCredentials(ctx, secrets, secretName)
		return nil, err
	}

	return &gen.DeployedTemplate{
		App:               appDto,
		CredentialsSecret: secretName,
		ConnectionKeys:    templateDto(t).ConnectionKeys,
	}, nil
}

// createCredentials stores the generated values of app in its credentials
// Secret, which must not exist yet, and returns the client to remove it with.
func (s *Server) createCredentials(
	ctx context.Context, e envScope, app string, values map[string]string,
) (secretDeleter, error) {
	cluster, err := s.cluster()
	if err != nil {
		return nil, err
	}
	secretName := CredentialsSecret(app)
	namespace := e.namespace()
	secrets := cluster.Typed.CoreV1().Secrets(namespace)

	_, err = secrets.Get(ctx, secretName, metav1.GetOptions{})
	if err == nil {
		return nil, kerrors.New(kerrors.Conflict, "secret `%s` already exists", secretName)
	} else if !apierrors.IsNotFound(err) {
		return nil, kubeError(err, secretName)
	}

	data := make(map[string][]byte, len(values))
	for k, v := range values {
		data[k] = []byte(v)
	}
	object := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: namespace,
			Labels: map[string]string{
				v1alpha1.LabelManagedBy:   v1alpha1.LabelManagerValue,
				v1alpha1.LabelEnvironment: e.resourceName(),
				v1alpha1.LabelApp:         app,
			},
		},
		Type: corev1.SecretTypeOpaque,
		Data: data,
	}
	if _, err := secrets.Create(ctx, object, metav1.CreateOptions{}); err != nil {
		return nil, kubeError(err, secretName)
	}
	return secrets, nil
}

// secretDeleter is the part of the Secrets client the cleanup needs.
type secretDeleter interface {
	Delete(ctx context.Context, name string, opts metav1.DeleteOptions) error
}

// dropOrphanedCredentials removes the credentials of an app that could not
// be created, so none are left behind. It runs even when the request was
// cancelled; a failure is logged and does not change the answer, which stays
// the creation's error (Rust discarded the failure).
func (s *Server) dropOrphanedCredentials(ctx context.Context, secrets secretDeleter, name string) {
	err := secrets.Delete(context.WithoutCancel(ctx), name, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		s.deps.Logger.Error("orphaned template credentials were not removed", "secret", name, "error", err)
	}
}
