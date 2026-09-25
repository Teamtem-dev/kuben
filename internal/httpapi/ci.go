package httpapi

// External CI trust (routes/ci.rs, M4.2): trust policies of an
// organization, and the exchange of a GitHub Actions OIDC token for a
// short-lived Kuben token.
//
// The exchange is served outside the session and CSRF layers (the caller
// has no session) and trusts only a verified provider token. Every refusal
// looks the same to the caller; the reason is logged.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/Teamtem-dev/kuben/internal/core/authz"
	"github.com/Teamtem-dev/kuben/internal/core/ci"
	"github.com/Teamtem-dev/kuben/internal/core/ids"
	"github.com/Teamtem-dev/kuben/internal/core/kerrors"
	"github.com/Teamtem-dev/kuben/internal/core/model"
	"github.com/Teamtem-dev/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/internal/core/perm"
	"github.com/Teamtem-dev/kuben/internal/httpapi/access"
	"github.com/Teamtem-dev/kuben/internal/httpapi/auth"
	"github.com/Teamtem-dev/kuben/internal/httpapi/gen"
	"github.com/Teamtem-dev/kuben/internal/httpapi/problem"
	"github.com/Teamtem-dev/kuben/internal/integrations/oidc"
	"github.com/Teamtem-dev/kuben/internal/integrations/outbound"
	"github.com/Teamtem-dev/kuben/internal/jsonx"
	"github.com/Teamtem-dev/kuben/internal/store"
)

const (
	// ciExchangePath is where GitHub Actions exchanges its OIDC token.
	ciExchangePath = "/api/v1/ci/github/token"
	// ciExchangeBodyLimit is the largest exchange request body.
	ciExchangeBodyLimit = 4 << 10
)

func ciPolicyDto(p store.CIPolicy) *gen.CiPolicyDto {
	var environment gen.OptNilUUID
	if e, ok := p.Environment.Get(); ok {
		environment = gen.NewOptNilUUID(e.UUID())
	} else {
		environment.SetToNull()
	}
	return &gen.CiPolicyDto{
		ID:                p.ID,
		Name:              p.Name,
		Project:           p.Project.UUID(),
		Environment:       environment,
		Repository:        p.Repository,
		RepositoryId:      int64(p.Policy.RepositoryID),      //nolint:gosec // stored as BIGINT
		RepositoryOwnerId: int64(p.Policy.RepositoryOwnerID), //nolint:gosec // stored as BIGINT
		Refs:              p.Policy.Refs,
		Environments:      p.Policy.Environments,
		Events:            p.Policy.Events,
		Role:              p.Policy.Role.String(),
		TokenTtlSecs:      int32(p.Policy.TokenTTLSecs), //nolint:gosec // at most MaxCITokenTTLSecs
		CreatedBy:         p.CreatedBy.String(),
		CreatedAt:         p.CreatedAt,
		RevokedAt:         optNilInt64(p.RevokedAt),
	}
}

func trimmed(list []string) []string {
	out := make([]string, 0, len(list))
	for _, s := range list {
		out = append(out, strings.TrimSpace(s))
	}
	return out
}

// trustPolicy is the policy a request asks for: deny by default, events
// defaulted, refs and environments trimmed, then validated.
func trustPolicy(req *gen.CreateCiPolicy) (ci.TrustPolicy, error) {
	events := req.Events
	if len(events) == 0 {
		events = ci.DefaultEvents()
	}
	role, err := perm.ParseRole(req.Role.Or(string(perm.Developer)))
	if err != nil {
		return ci.TrustPolicy{}, err //nolint:wrapcheck // a kerrors already
	}
	ttl := ci.DefaultCITokenTTLSecs
	if v, ok := req.TokenTtlSecs.Get(); ok {
		ttl = uint32(v) //nolint:gosec // the contract's minimum is 0
	}
	p := ci.TrustPolicy{
		RepositoryID:      uint64(req.RepositoryId),      //nolint:gosec // the contract's minimum is 0
		RepositoryOwnerID: uint64(req.RepositoryOwnerId), //nolint:gosec // the contract's minimum is 0
		Refs:              trimmed(req.Refs),
		Environments:      trimmed(req.Environments),
		Events:            events,
		Role:              role,
		TokenTTLSecs:      ttl,
	}
	if err := p.Validate(); err != nil {
		return ci.TrustPolicy{}, kerrors.New(kerrors.Validation, "%s", err.Error())
	}
	return p, nil
}

// ciAdmin is the access and organization every policy operation acts on,
// with the proof of org-admin.
func (s *Server) ciAdmin(ctx context.Context, forbidToken bool) (access.Access, ids.OrgID, error) {
	a, err := s.access(ctx)
	if err != nil {
		return access.Access{}, ids.OrgID{}, err
	}
	if forbidToken {
		if err := a.ForbidToken(); err != nil {
			return access.Access{}, ids.OrgID{}, err //nolint:wrapcheck // a kerrors already
		}
	}
	org, err := orgOf(a)
	if err != nil {
		return access.Access{}, ids.OrgID{}, err
	}
	if _, err := a.Require(perm.OrgAdmin, authz.OrgChain(org)); err != nil {
		return access.Access{}, ids.OrgID{}, err //nolint:wrapcheck // a kerrors already
	}
	return a, org, nil
}

// ListCiTrustPolicies is the organization's CI trust policies.
func (s *Server) ListCiTrustPolicies(ctx context.Context) (gen.ListCiTrustPoliciesRes, error) {
	_, org, err := s.ciAdmin(ctx, false)
	if err != nil {
		return nil, err
	}
	policies, err := s.deps.Store.CIPolicies(ctx, org)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	out := make(gen.ListCiTrustPoliciesOKApplicationJSON, 0, len(policies))
	for _, p := range policies {
		out = append(out, *ciPolicyDto(p))
	}
	return &out, nil
}

// CreateCiTrustPolicy trusts one GitHub repository's workflows to deploy
// into a project. Exchanged tokens act with the creator's authority,
// capped at the policy's role.
func (s *Server) CreateCiTrustPolicy(ctx context.Context, req *gen.CreateCiPolicy) (gen.CreateCiTrustPolicyRes, error) {
	a, org, err := s.ciAdmin(ctx, true)
	if err != nil {
		return nil, err
	}
	name := strings.TrimSpace(req.Name)
	if !ValidTokenName(name) {
		return nil, kerrors.New(kerrors.Validation, "name must be 1–64 printable characters")
	}
	policy, err := trustPolicy(req)
	if err != nil {
		return nil, err
	}
	own, ok := a.OrgRole(org).Get()
	if !ok || policy.Role.Rank() > own.Rank() {
		return nil, kerrors.ErrForbidden
	}
	n := store.NewCIPolicy{
		Org:        org,
		Name:       name,
		Repository: strings.TrimSpace(req.Repository),
		Policy:     policy,
		CreatedBy:  a.Current.User.ID,
	}
	if env, ok := req.Environment.Get(); ok {
		found, err := s.findEnvironment(ctx, a, req.Project, env)
		if err != nil {
			return nil, err
		}
		n.Project, n.Environment = found.project.project.ID, opt.Some(found.env.ID)
	} else {
		found, err := s.findProject(ctx, a, req.Project)
		if err != nil {
			return nil, err
		}
		n.Project = found.project.ID
	}
	made, err := s.deps.Store.CreateCIPolicy(ctx, n)
	if err != nil {
		return nil, duplicate(err, fmt.Sprintf("trust policy `%s`", name))
	}
	return ciPolicyDto(made), nil
}

// RevokeCiTrustPolicy revokes a trust policy and every token exchanged
// under it.
func (s *Server) RevokeCiTrustPolicy(ctx context.Context, params gen.RevokeCiTrustPolicyParams) (gen.RevokeCiTrustPolicyRes, error) {
	_, org, err := s.ciAdmin(ctx, true)
	if err != nil {
		return nil, err
	}
	revoked, err := s.deps.Store.RevokeCIPolicy(ctx, org, params.Policy)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	if !revoked {
		return nil, kerrors.New(kerrors.NotFound, "an active trust policy `%s`", params.Policy)
	}
	return &gen.RevokeCiTrustPolicyNoContent{}, nil
}

// githubOIDC is the GitHub Actions verifier when CI trust is on and its
// audience is known (serve.rs github_oidc).
func (s *Server) githubOIDC() opt.Val[*oidc.Github] {
	cfg := s.deps.Config
	audience, ok := cfg.GithubOIDCAudience()
	if !ok {
		if cfg.CI.GithubActions {
			s.deps.Logger.Warn("ci.github_actions is set without an audience (ci.github_oidc_audience or " +
				"server.public_url): CI tokens are refused")
		}
		return opt.None[*oidc.Github]()
	}
	s.deps.Logger.Info("GitHub Actions OIDC exchange enabled", "issuer", cfg.CI.GithubOIDCIssuer, "audience", audience)
	return opt.Some(oidc.NewGithub(cfg.CI.GithubOIDCIssuer, audience,
		outbound.New(false, oidc.Timeout), s.deps.Clock, s.deps.Logger))
}

// ciExchange is `POST /api/v1/ci/github/token` on the verifier the
// configuration names.
func (s *Server) ciExchange() http.Handler { return s.ciExchangeOn(s.githubOIDC()) }

// ciExchangeOn is the exchange on verifier, with its own body limit; only
// POST is routed to it, as axum's post() did.
func (s *Server) ciExchangeOn(verifier opt.Val[*oidc.Github]) http.Handler {
	exchange := limitBody(ciExchangeBodyLimit, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.exchangeCIToken(w, r, verifier)
	}))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		exchange.ServeHTTP(w, r)
	})
}

// writeCIJSON answers v as axum's Json did: application/json, no HTML
// escaping.
func writeCIJSON(w http.ResponseWriter, status int, v any) {
	var body bytes.Buffer
	enc := json.NewEncoder(&body)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v) //nolint:errcheck // plain structs always encode
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(bytes.TrimSuffix(body.Bytes(), []byte("\n"))) //nolint:errcheck // the client is gone
}

// detail is a JSON body with only a detail.
type detail struct {
	Detail string `json:"detail"`
}

func (s *Server) untrusted(w http.ResponseWriter, reason string) {
	s.deps.Logger.Warn("a CI token exchange was refused", "reason", reason)
	writeCIJSON(w, http.StatusUnauthorized, struct {
		Detail string `json:"detail"`
		Title  string `json:"title"`
	}{Detail: "the CI token is not trusted", Title: "unauthorized"})
}

// bearer is the token of an `Authorization: Bearer …` header, trimmed.
func bearer(h http.Header) (string, bool) {
	token, ok := strings.CutPrefix(h.Get("Authorization"), "Bearer ")
	token = strings.TrimSpace(token)
	return token, ok && token != ""
}

// exchangePolicy is the policy id of an exchange request, `{"policy":
// "<id>"}` and nothing else.
func exchangePolicy(body []byte) (uuid.UUID, bool) {
	var o jsonx.Object
	if json.Unmarshal(body, &o) != nil {
		return uuid.Nil, false
	}
	for key := range o {
		if key != "policy" {
			return uuid.Nil, false
		}
	}
	var text string
	if jsonx.Required(o, "policy", &text) != nil {
		return uuid.Nil, false
	}
	id, err := uuid.Parse(text)
	return id, err == nil
}

func (s *Server) exchangeCIToken(w http.ResponseWriter, r *http.Request, verifier opt.Val[*oidc.Github]) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	v, ok := verifier.Get()
	if !ok {
		writeCIJSON(w, http.StatusNotFound, detail{Detail: "CI trust is not configured"})
		return
	}
	providerToken, ok := bearer(r.Header)
	if !ok {
		s.untrusted(w, "no bearer token")
		return
	}
	policy, ok := exchangePolicy(body)
	if !ok {
		writeCIJSON(w, http.StatusUnprocessableEntity, detail{Detail: `send {"policy": "<id>"}`})
		return
	}
	claims, err := v.Verify(r.Context(), providerToken)
	var unavailable oidc.UnavailableError
	switch {
	case errors.As(err, &unavailable):
		s.deps.Logger.Error("the CI issuer's keys are unavailable", "error", unavailable.Reason)
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	case err != nil:
		s.untrusted(w, err.Error())
		return
	}
	if reason, err := s.issueCIToken(r.Context(), w, v.Issuer(), claims, policy); err != nil {
		problem.Write(w, s.deps.Logger, err)
	} else if reason != "" {
		s.untrusted(w, reason)
	}
}

// ciTokenName is a token name that says where it came from, at most 64
// characters.
func ciTokenName(policy store.CIPolicy, claims ci.GithubClaims) string {
	name := []rune(fmt.Sprintf("ci:%s:%s#%s", policy.Name, claims.Repository, claims.RunID.Or("?")))
	return string(name[:min(len(name), 64)])
}

// issuedCIToken is the body of an exchange's answer.
type issuedCIToken struct {
	Environment opt.Val[ids.EnvironmentID] `json:"environment"`
	ExpiresAt   int64                      `json:"expiresAt"`
	Project     ids.ProjectID              `json:"project"`
	Role        perm.Role                  `json:"role"`
	Token       string                     `json:"token"`
}

// issueCIToken issues a token under policyID for claims and answers it;
// a non-empty reason is why nothing was issued (answered as untrusted).
func (s *Server) issueCIToken(ctx context.Context, w http.ResponseWriter, issuer string, claims ci.GithubClaims, policyID uuid.UUID) (reason string, err error) {
	policy, found, err := s.deps.Store.CIPolicy(ctx, policyID)
	switch {
	case err != nil:
		return "", err //nolint:wrapcheck // a store error, answered as internal
	case !found:
		return "no such policy", nil
	case policy.RevokedAt.IsSome():
		return fmt.Sprintf("policy %s is revoked", policyID), nil
	}
	if denied := policy.Policy.Evaluate(claims); denied != nil {
		return fmt.Sprintf("policy %s: %v", policyID, denied), nil
	}
	plaintext, id, secretHash := auth.NewAPIToken()
	expiresAt := s.deps.Clock.NowMs() + int64(policy.Policy.TokenTTLSecs)*1000
	scope := model.TokenScope{Role: policy.Policy.Role, Project: opt.Some(policy.Project.UUID())}
	if e, ok := policy.Environment.Get(); ok {
		scope.Environment = opt.Some(e.UUID())
	}
	outcome, err := s.deps.Store.ExchangeCIToken(ctx, store.CIExchange{
		Policy:            policy.ID,
		Issuer:            issuer,
		Jti:               claims.Jti,
		ProviderExpiresAt: (claims.Exp + ci.ClockLeewaySecs) * 1000,
		Token: store.NewToken{
			ID: id, OrgID: policy.Org, Owner: policy.CreatedBy, Name: ciTokenName(policy, claims),
			Prefix: auth.TokenDisplayPrefix(plaintext), SecretHash: secretHash, Scope: scope,
			ExpiresAt: opt.Some(expiresAt),
		},
	})
	if err != nil {
		return "", err //nolint:wrapcheck // a store error, answered as internal
	}
	switch o := outcome.(type) {
	case store.ExchangedIssued:
		s.deps.Logger.Info("a CI token was issued", "policy", policy.ID, "token", o.Token.ID,
			"repository", claims.Repository, "git_ref", claims.GitRef, "run", claims.RunID.Or(""))
		writeCIJSON(w, http.StatusCreated, issuedCIToken{
			Environment: policy.Environment, ExpiresAt: expiresAt, Project: policy.Project,
			Role: policy.Policy.Role, Token: plaintext,
		})
		return "", nil
	case store.ExchangedReplayed:
		return fmt.Sprintf("provider token %s was used before", claims.Jti), nil
	case store.ExchangedPolicyRevoked:
		return fmt.Sprintf("policy %s was revoked", policyID), nil
	}
	return "", fmt.Errorf("an exchange ended as %T", outcome)
}
