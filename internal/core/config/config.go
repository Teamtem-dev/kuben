// Package config is Kuben's configuration, the contract with operators. It
// replaces the Rust module kuben-core/src/config.rs.
//
// Precedence (lowest → highest): built-in defaults → `/etc/kuben/config.toml`
// → `./kuben.toml` → `KUBEN_*` environment variables (nested keys separated
// by `__`, e.g. `KUBEN_SERVER__BIND=0.0.0.0:9000`). Keys nobody reads are
// ignored; a value of the wrong type refuses to load.
package config

import (
	"github.com/Teamtem-dev/kuben/internal/core/ci"
	"github.com/Teamtem-dev/kuben/internal/core/opt"
)

// Config is the whole configuration; every section is a TOML table.
type Config struct {
	Server    ServerCfg    `koanf:"server"`
	Database  DatabaseCfg  `koanf:"database"`
	Runtime   RuntimeCfg   `koanf:"runtime"`
	Kube      KubeCfg      `koanf:"kube"`
	Security  SecurityCfg  `koanf:"security"`
	Telemetry TelemetryCfg `koanf:"telemetry"`
	Bootstrap BootstrapCfg `koanf:"bootstrap"`
	Agent     AgentCfg     `koanf:"agent"`
	Git       GitCfg       `koanf:"git"`
	Build     BuildCfg     `koanf:"build"`
	CI        CiCfg        `koanf:"ci"`
	SSO       SsoCfg       `koanf:"sso"`
	Secrets   SecretsCfg   `koanf:"secrets"`
	Quota     QuotaCfg     `koanf:"quota"`
	Backup    BackupCfg    `koanf:"backup"`
	Notify    NotifyCfg    `koanf:"notify"`
	Retention RetentionCfg `koanf:"retention"`
	Domains   DomainsCfg   `koanf:"domains"`
}

// Default is the configuration of an installation that configured nothing.
func Default() Config {
	return Config{
		Server:    DefaultServerCfg(),
		Database:  DefaultDatabaseCfg(),
		Runtime:   DefaultRuntimeCfg(),
		Security:  DefaultSecurityCfg(),
		Telemetry: DefaultTelemetryCfg(),
		Bootstrap: DefaultBootstrapCfg(),
		Agent:     DefaultAgentCfg(),
		Git:       DefaultGitCfg(),
		Build:     DefaultBuildCfg(),
		CI:        DefaultCiCfg(),
		SSO:       DefaultSsoCfg(),
		Backup:    DefaultBackupCfg(),
		Retention: DefaultRetentionCfg(),
		Domains:   DefaultDomainsCfg(),
	}
}

// ServerCfg is `[server]`: where Kuben listens and what this process runs.
type ServerCfg struct {
	Bind          string          `koanf:"bind"`
	MetricsBind   string          `koanf:"metrics_bind"`
	ActivatorBind string          `koanf:"activator_bind"`
	PublicURL     opt.Val[string] `koanf:"public_url,omitempty"`
	Roles         []Role          `koanf:"roles"`
	// RequestTimeoutSecs is the request timeout for non-streaming endpoints,
	// seconds.
	RequestTimeoutSecs uint64 `koanf:"request_timeout_secs"`
	// MaxBodyBytes is the maximum request body, bytes.
	MaxBodyBytes uint64 `koanf:"max_body_bytes"`
	// StateDir is the directory for files that belong to this installation:
	// the setup token and a generated initial admin password. Default: see
	// [Config.StateDir].
	StateDir opt.Val[string] `koanf:"state_dir,omitempty"`
}

// DefaultServerCfg is `[server]` when nothing is configured.
func DefaultServerCfg() ServerCfg {
	return ServerCfg{
		Bind:               "0.0.0.0:8080",
		MetricsBind:        "0.0.0.0:9090",
		ActivatorBind:      "0.0.0.0:8081",
		Roles:              []Role{RoleAll},
		RequestTimeoutSecs: 30,
		MaxBodyBytes:       1 << 20,
	}
}

// DatabaseCfg is `[database]`.
type DatabaseCfg struct {
	// URL is `postgres://user:pass@host/db`. Required, with no default: Kuben
	// keeps its data in PostgreSQL (ADR-025).
	URL            Secret `koanf:"url"`
	MaxConnections uint32 `koanf:"max_connections"`
}

// DefaultDatabaseCfg is `[database]` when nothing is configured.
func DefaultDatabaseCfg() DatabaseCfg { return DatabaseCfg{MaxConnections: 4} }

// RuntimeCfg is `[runtime]`.
type RuntimeCfg struct {
	WorkerThreads      opt.Val[uint64] `koanf:"worker_threads,omitempty"`
	MaxBlockingThreads uint64          `koanf:"max_blocking_threads"`
	// Bulkhead (ADR-013): run controllers on a second runtime (off in phase 0).
	Bulkhead bool `koanf:"bulkhead"`
}

// DefaultRuntimeCfg is `[runtime]` when nothing is configured.
func DefaultRuntimeCfg() RuntimeCfg { return RuntimeCfg{MaxBlockingThreads: 16} }

// KubeCfg is `[kube]`: the cluster Kuben itself talks to.
type KubeCfg struct {
	Kubeconfig     opt.Val[string] `koanf:"kubeconfig,omitempty"`
	Context        opt.Val[string] `koanf:"context,omitempty"`
	WatchNamespace opt.Val[string] `koanf:"watch_namespace,omitempty"`
	// Required: if true, refuse to start when no cluster is reachable.
	Required bool `koanf:"required"`
	// Namespace Kuben itself runs in (controller lease, initial admin
	// Secret). Defaults to the pod's service-account namespace.
	Namespace opt.Val[string] `koanf:"namespace,omitempty"`
	// LeaderElection runs the controllers only on the replica holding the
	// `kuben-controller` Lease (ADR-023). Required whenever more than one
	// replica has the controller role; the Helm chart always enables it.
	LeaderElection bool `koanf:"leader_election"`
}

// TelemetryCfg is `[telemetry]`.
type TelemetryCfg struct {
	// LogFormat is `json` or `pretty`.
	LogFormat    string          `koanf:"log_format"`
	LogLevel     string          `koanf:"log_level"`
	OTLPEndpoint opt.Val[string] `koanf:"otlp_endpoint,omitempty"`
}

// DefaultTelemetryCfg is `[telemetry]` when nothing is configured.
func DefaultTelemetryCfg() TelemetryCfg {
	return TelemetryCfg{LogFormat: "json", LogLevel: "info"}
}

// BootstrapCfg is `[bootstrap]`: the first organization and its admin.
type BootstrapCfg struct {
	OrgSlug    string `koanf:"org_slug"`
	OrgName    string `koanf:"org_name"`
	AdminEmail string `koanf:"admin_email"`
	// AdminPassword is the initial admin password. If unset (or empty), a
	// random one is generated: in a pod it is stored in the
	// `kuben-initial-admin` Secret; elsewhere it is printed once to the
	// terminal or, without one, written to an owner-only file in
	// [Config.StateDir]. It is never logged.
	AdminPassword Secret `koanf:"admin_password"`
}

// DefaultBootstrapCfg is `[bootstrap]` when nothing is configured.
func DefaultBootstrapCfg() BootstrapCfg {
	return BootstrapCfg{OrgSlug: "default", OrgName: "Default", AdminEmail: "admin@kuben.local"}
}

// AgentCfg is `[agent]`: AgentLink, the hub's endpoint for cluster agents
// (ADR-027).
type AgentCfg struct {
	// Bind is where the hub listens for agents (`host:port`); unset, it does
	// not.
	Bind opt.Val[string] `koanf:"bind,omitempty"`
	// HubName is the name the hub's certificate carries; agents check it.
	HubName string `koanf:"hub_name"`
	// CertificateHours is the lifetime of the client certificates the hub
	// issues, hours.
	CertificateHours uint64 `koanf:"certificate_hours"`
	// HeartbeatSecs is how often agents send a heartbeat, seconds.
	HeartbeatSecs uint64 `koanf:"heartbeat_secs"`
	// Local enrolls an agent inside Kuben's own cluster (M2.8): the hub
	// publishes its address, CA and a bootstrap token in a Secret the agent
	// mounts.
	Local bool `koanf:"local"`
	// Advertise is the address agents in the cluster dial (`host:port`),
	// e.g. the Service of Kuben or the node's address.
	Advertise opt.Val[string] `koanf:"advertise,omitempty"`
	// Namespace of the local agent and its Secrets; default: Kuben's own,
	// else `kuben-system`.
	Namespace opt.Val[string] `koanf:"namespace,omitempty"`
}

// DefaultAgentCfg is `[agent]` when nothing is configured.
func DefaultAgentCfg() AgentCfg {
	return AgentCfg{HubName: "hub.kuben.internal", CertificateHours: 24, HeartbeatSecs: 10}
}

// GitCfg is `[git]`: Git providers (M3), the GitHub App Kuben acts as.
type GitCfg struct {
	// GithubAppID is the GitHub App's numeric id; unset, Git sources are off.
	GithubAppID opt.Val[uint64] `koanf:"github_app_id,omitempty"`
	// GithubPrivateKeyFile is the PEM file of the App's private key (PKCS#1
	// or PKCS#8 RSA).
	GithubPrivateKeyFile opt.Val[string] `koanf:"github_private_key_file,omitempty"`
	// GithubWebhookSecret is the secret GitHub signs webhook deliveries with.
	GithubWebhookSecret Secret `koanf:"github_webhook_secret"`
	// GithubAPIURL is the REST API root, for GitHub Enterprise Server.
	GithubAPIURL string `koanf:"github_api_url"`
	// GithubCloneURL is where build pods clone from.
	GithubCloneURL string `koanf:"github_clone_url"`
}

// DefaultGitCfg is `[git]` when nothing is configured.
func DefaultGitCfg() GitCfg {
	return GitCfg{GithubAPIURL: "https://api.github.com", GithubCloneURL: "https://github.com"}
}

// GithubEnabled reports whether the GitHub App is configured completely.
func (g GitCfg) GithubEnabled() bool {
	return g.GithubAppID.IsSome() && g.GithubPrivateKeyFile.Or("") != "" && g.GithubWebhookSecret.IsSet()
}

// NotifyCfg is `[notify]`: webhooks and commit statuses (M4.10).
type NotifyCfg struct {
	// AllowPrivateTargets delivers webhooks to private, loopback and
	// link-local addresses too (by default they are refused: an endpoint
	// could reach inside).
	AllowPrivateTargets bool `koanf:"allow_private_targets"`
	// AllowHTTP allows `http://` endpoints (by default only `https://`).
	AllowHTTP bool `koanf:"allow_http"`
}

// DomainsCfg is `[domains]`: domain claims and DNS providers (M5.2).
type DomainsCfg struct {
	// RequireClaim: apps may only use custom domains their organization
	// verified. Off by default; a domain another organization verified is
	// refused either way.
	RequireClaim bool `koanf:"require_claim"`
	// DohURL is the DNS-over-HTTPS resolver claims are checked with (JSON
	// API).
	DohURL string `koanf:"doh_url"`
	// CloudflareAPIURL is the Cloudflare API.
	CloudflareAPIURL string `koanf:"cloudflare_api_url"`
	// CnameTarget: records Kuben writes for an app point here (a CNAME)
	// instead of at the Gateway's addresses.
	CnameTarget opt.Val[string] `koanf:"cname_target,omitempty"`
}

// DefaultDomainsCfg is `[domains]` when nothing is configured.
func DefaultDomainsCfg() DomainsCfg {
	return DomainsCfg{
		DohURL:           "https://cloudflare-dns.com/dns-query",
		CloudflareAPIURL: "https://api.cloudflare.com/client/v4",
	}
}

// RetentionCfg is `[retention]`: how long rows that only describe the past
// are kept (M4.12). Audit events, runs, releases and evidence are not
// covered: they are kept.
type RetentionCfg struct {
	// OutboxDays: delivered outbox messages.
	OutboxDays uint32 `koanf:"outbox_days"`
	// WebhookDeliveryDays: delivered and given-up webhook deliveries.
	WebhookDeliveryDays uint32 `koanf:"webhook_delivery_days"`
	// ResolvedIncidentDays: resolved incidents.
	ResolvedIncidentDays uint32 `koanf:"resolved_incident_days"`
	// UsageDays: hourly usage of apps (M5.5).
	UsageDays uint32 `koanf:"usage_days"`
}

// DefaultRetentionCfg is `[retention]` when nothing is configured.
func DefaultRetentionCfg() RetentionCfg {
	return RetentionCfg{OutboxDays: 7, WebhookDeliveryDays: 30, ResolvedIncidentDays: 180, UsageDays: 7}
}

// BackupCfg is `[backup]`: backups of the database (M4.7). `kuben backup`
// writes them (a systemd timer or the chart's CronJob runs it); the server
// only watches their age.
type BackupCfg struct {
	// Dir is where backups go; default `backups` in [Config.StateDir]. Copy
	// them off this host: a backup on the same disk is no backup.
	Dir opt.Val[string] `koanf:"dir,omitempty"`
	// Keep is the number of backups kept in Dir; older ones are removed
	// after a good backup.
	Keep uint32 `koanf:"keep"`
	// MaxAgeHours: the server reports itself degraded when the newest good
	// backup is older than this; 0 turns the check off.
	MaxAgeHours uint32 `koanf:"max_age_hours"`
	// BeforeUpgrade takes a backup before a new version migrates the
	// database (where PostgreSQL's client tools are installed).
	BeforeUpgrade bool `koanf:"before_upgrade"`
}

// DefaultBackupCfg is `[backup]` when nothing is configured.
func DefaultBackupCfg() BackupCfg {
	return BackupCfg{Keep: 7, MaxAgeHours: 26, BeforeUpgrade: true}
}

// SecretsCfg is `[secrets]`: managed secrets (M4.4, ADR-030).
type SecretsCfg struct {
	// KeyringFile is the keyring of key-encryption keys:
	// `version:base64-key` lines, owner readable only. Every replica must
	// read the same file (mount it from a Kubernetes Secret) and it must be
	// backed up apart from the database. Default: `secrets.keyring` in
	// [Config.StateDir], created with one key when missing.
	KeyringFile opt.Val[string] `koanf:"keyring_file,omitempty"`
}

// CiCfg is `[ci]`: external CI trust (M4.2). GitHub Actions exchanges its
// OIDC token for a short-lived Kuben token under an organization's trust
// policy.
type CiCfg struct {
	// GithubActions accepts GitHub Actions OIDC tokens.
	GithubActions bool `koanf:"github_actions"`
	// GithubOIDCIssuer is the only issuer trusted; its JWKS is read from
	// `<issuer>/.well-known/jwks`, never from a token.
	GithubOIDCIssuer string `koanf:"github_oidc_issuer"`
	// GithubOIDCAudience is the audience workflows request (`id-token`
	// `audience`); defaults to `server.public_url`.
	GithubOIDCAudience opt.Val[string] `koanf:"github_oidc_audience,omitempty"`
}

// DefaultCiCfg is `[ci]` when nothing is configured.
func DefaultCiCfg() CiCfg { return CiCfg{GithubOIDCIssuer: ci.GithubActionsIssuer} }
