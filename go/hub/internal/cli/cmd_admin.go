package cli

// `kuben reset-admin` (cli/admin.rs) and `kuben agent-token`
// (cli/agent.rs, ADR-027).

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/Teamtem-dev/kuben/go/hub/internal/api/auth"
	"github.com/Teamtem-dev/kuben/go/hub/internal/bootstrap"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/config"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/model"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/agentlink"
	"github.com/Teamtem-dev/kuben/go/hub/internal/serve"
	"github.com/Teamtem-dev/kuben/go/hub/internal/store"
)

func resetAdminCmd(g *globals) *cobra.Command {
	var password string
	cmd := &cobra.Command{
		Use:   "reset-admin",
		Short: "Reset (or create) the admin user's password",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			cfg, err := g.load()
			if err != nil {
				return err
			}
			ctx, stop := runCtx(c)
			defer stop()
			return resetAdmin(ctx, cfg, password, g.stdout, serve.Logger(cfg.Telemetry))
		},
	}
	cmd.Flags().StringVar(&password, "password", "", "New password. If omitted, a random one is generated and printed")
	bindEnv(cmd.Flags(), "password", "KUBEN_ADMIN_PASSWORD")
	return cmd
}

// resetAdmin sets the admin's password (creating the admin when there is
// none) and revokes every session of the account.
func resetAdmin(ctx context.Context, cfg config.Config, password string, out io.Writer, logger *slog.Logger) error {
	st, err := store.Connect(ctx, cfg.Database)
	if err != nil {
		return err //nolint:wrapcheck // explains itself
	}
	defer st.Close()
	hasher := auth.HasherFromConfig(cfg.Security)
	if password == "" {
		if password, err = bootstrap.RandomPassword(); err != nil {
			return err //nolint:wrapcheck // explains itself
		}
	}
	hash, err := hasher.Hash(password)
	if err != nil {
		return fmt.Errorf("hash: %w", err)
	}
	email := cfg.Bootstrap.AdminEmail
	user, err := adminUser(ctx, cfg, st, hasher, email, hash, password, logger)
	if err != nil {
		return err
	}
	revoked, err := st.RevokeAllSessions(ctx, user.ID)
	if err != nil {
		return err //nolint:wrapcheck // a store error
	}
	_, err = fmt.Fprintf(out, "admin: %s\npassword: %s\nsessions revoked: %d\n", user.Email, password, revoked)
	return err //nolint:wrapcheck // stdout
}

func adminUser(ctx context.Context, cfg config.Config, st *store.Store, hasher *auth.Hasher, email, hash, password string, logger *slog.Logger) (model.User, error) {
	c, found, err := st.FindUserByEmail(ctx, email)
	if err != nil {
		return model.User{}, err //nolint:wrapcheck // a store error
	}
	if found {
		if err := st.SetPasswordHash(ctx, c.User.ID, hash); err != nil {
			return model.User{}, err //nolint:wrapcheck // a store error
		}
		return c.User, nil
	}
	cfg.Bootstrap.AdminPassword = config.Secret(password)
	if _, err := bootstrap.EnsureAdmin(ctx, cfg, st, hasher, logger); err != nil {
		return model.User{}, err //nolint:wrapcheck // explains itself
	}
	c, found, err = st.FindUserByEmail(ctx, email)
	if err != nil {
		return model.User{}, err //nolint:wrapcheck // a store error
	}
	if !found {
		return model.User{}, errors.New("admin not created")
	}
	return c.User, nil
}

// agentTokenOpts are the options of `kuben agent-token`.
type agentTokenOpts struct {
	cluster    string
	org        string
	ttlMinutes uint64
}

func agentTokenCmd(g *globals) *cobra.Command {
	var opts agentTokenOpts
	cmd := &cobra.Command{
		Use:   "agent-token",
		Short: "Issue a bootstrap token for a cluster's agent: printed once, only its hash is kept",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			cfg, err := g.load()
			if err != nil {
				return err
			}
			ctx, stop := runCtx(c)
			defer stop()
			return agentToken(ctx, cfg, opts, g.stdout, serve.Logger(cfg.Telemetry))
		},
	}
	flags := cmd.Flags()
	flags.StringVar(&opts.cluster, "cluster", "primary", "The cluster the agent runs in; made if it does not exist yet")
	flags.StringVar(&opts.org, "org", "", "The organization (slug); the bootstrap organization by default")
	flags.Uint64Var(&opts.ttlMinutes, "ttl-minutes", 30, "How long the token stays valid, in minutes")
	return cmd
}

// agentToken issues a bootstrap token for a cluster's agent. The token is
// printed once; only its hash is kept.
func agentToken(ctx context.Context, cfg config.Config, opts agentTokenOpts, out io.Writer, logger *slog.Logger) error {
	st, err := store.Connect(ctx, cfg.Database)
	if err != nil {
		return err //nolint:wrapcheck // explains itself
	}
	defer st.Close()
	slug := opts.org
	if slug == "" {
		slug = cfg.Bootstrap.OrgSlug
	}
	org, found, err := st.FindOrgBySlug(ctx, slug)
	if err != nil {
		return err //nolint:wrapcheck // a store error
	}
	if !found {
		return fmt.Errorf("no organization `%s`", slug)
	}
	minutes := max(opts.ttlMinutes, 1)
	token, err := agentlink.NewToken()
	if err != nil {
		return err //nolint:wrapcheck // explains itself
	}
	tenant, err := st.Tenant(ctx, org.ID)
	if err != nil {
		return err //nolint:wrapcheck // a store error
	}
	defer tenant.Rollback(ctx) //nolint:errcheck // after a commit it does nothing
	cluster, err := tenant.EnsureCluster(ctx, opts.cluster)
	if err != nil {
		return err //nolint:wrapcheck // a store error
	}
	ttl := time.Duration(min(minutes, uint64(1<<31))) * time.Minute
	created, err := tenant.CreateAgentToken(ctx, cluster, agentlink.TokenHash(token), ttl, "cli:agent-token")
	if err != nil {
		return err //nolint:wrapcheck // a store error
	}
	if !created {
		return fmt.Errorf("cluster `%s` is not in organization `%s`", opts.cluster, slug)
	}
	if err := tenant.Commit(ctx); err != nil {
		return err //nolint:wrapcheck // a store error
	}

	// The agent pins this CA; the hub issues under it (made here if the hub
	// has not started yet, from the same state directory).
	dir := agentlink.Directory(cfg.StateDir())
	if _, err := agentlink.ClusterCAIn(dir, logger); err != nil {
		return err //nolint:wrapcheck // names the file
	}
	_, err = fmt.Fprintf(out, "cluster:     %s (%s)\n"+
		"token:       %s\n"+
		"valid for:   %d minutes, redeemed once\n"+
		"hub CA:      %s\n"+
		"\n"+
		"Give the agent the token through a file or stdin, never as an argument:\n"+
		"  kuben-agent --hub <host:port> --hub-ca ca.crt --cluster %s --token-file <file>\n",
		opts.cluster, cluster, token, minutes, filepath.Join(dir, agentlink.CACertificate), cluster)
	return err //nolint:wrapcheck // stdout
}
