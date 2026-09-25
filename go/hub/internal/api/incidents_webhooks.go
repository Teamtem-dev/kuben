package api

// The webhook endpoints of an organization (M4.10; routes/incidents.rs).

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/Teamtem-dev/kuben/go/hub/internal/api/gen"
	"github.com/Teamtem-dev/kuben/go/hub/internal/api/notify"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/authz"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/kerr"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/perm"
	"github.com/Teamtem-dev/kuben/go/hub/internal/store"
)

// ListWebhooks is the organization's webhook endpoints.
func (s *Server) ListWebhooks(ctx context.Context) (gen.ListWebhooksRes, error) {
	a, err := s.access(ctx)
	if err != nil {
		return nil, err
	}
	t, err := s.orgTenant(ctx, a, perm.OrgAdmin)
	if err != nil {
		return nil, err
	}
	defer t.Rollback(ctx) //nolint:errcheck // read only
	found, err := t.Endpoints(ctx)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	out := make(gen.ListWebhooksOKApplicationJSON, 0, len(found))
	for _, e := range found {
		out = append(out, endpointDto(e))
	}
	return &out, nil
}

// newWebhookSecret is a fresh signing secret: `whsec_` and 32 random bytes.
func newWebhookSecret() string {
	var raw [32]byte
	_, _ = rand.Read(raw[:]) //nolint:errcheck // crypto/rand never fails
	return "whsec_" + base64.RawURLEncoding.EncodeToString(raw[:])
}

// CreateWebhook adds a webhook endpoint; the answer holds its signing
// secret, once.
func (s *Server) CreateWebhook(ctx context.Context, req *gen.CreateEndpoint) (gen.CreateWebhookRes, error) {
	a, err := s.access(ctx)
	if err != nil {
		return nil, err
	}
	if err := a.ForbidToken(); err != nil {
		return nil, err //nolint:wrapcheck // a kerr already
	}
	org, err := orgOf(a)
	if err != nil {
		return nil, err
	}
	if _, err := a.Require(perm.OrgAdmin, authz.OrgChain(org)); err != nil {
		return nil, err //nolint:wrapcheck // a kerr already
	}
	events, err := checkEndpoint(req)
	if err != nil {
		return nil, err
	}
	url := strings.TrimSpace(req.URL)
	if err := notify.CheckTarget(ctx, url, s.deps.Config.Notify); err != nil {
		return nil, kerr.New(kerr.Validation, "%s", err.Error())
	}
	keyring, ok := s.deps.Keyring.Get()
	if !ok {
		return nil, kerr.New(kerr.Unavailable, "secret encryption is not configured")
	}
	secret := newWebhookSecret()
	id := uuid.Must(uuid.NewV7())
	sealed, err := notify.SealSecret(keyring, org, id, []byte(secret))
	if err != nil {
		return nil, kerr.New(kerr.Internal, "%s", err.Error())
	}
	_, actor := a.Actor()
	name := strings.TrimSpace(req.Name)
	t, err := s.deps.Store.Tenant(ctx, org)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	defer t.Rollback(ctx) //nolint:errcheck // committed on success
	if err := t.CreateEndpoint(ctx, id, name, url, events, sealed, actor); err != nil {
		return nil, duplicate(err, "webhook `"+name+"`")
	}
	if err := t.AppendAudit(ctx, requestAudit(a, "webhook.created", "webhook", name)); err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	found, err := t.Endpoints(ctx)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	i := slices.IndexFunc(found, func(e store.Endpoint) bool { return e.ID == id })
	if i < 0 {
		return nil, kerr.New(kerr.Internal, "the new webhook is missing")
	}
	if err := t.Commit(ctx); err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	dto := endpointDto(found[i])
	dto.Secret = gen.NewOptNilString(secret)
	return &dto, nil
}

// DisableWebhook disables a webhook endpoint for good.
func (s *Server) DisableWebhook(ctx context.Context, params gen.DisableWebhookParams) (gen.DisableWebhookRes, error) {
	a, err := s.access(ctx)
	if err != nil {
		return nil, err
	}
	if err := a.ForbidToken(); err != nil {
		return nil, err //nolint:wrapcheck // a kerr already
	}
	t, err := s.orgTenant(ctx, a, perm.OrgAdmin)
	if err != nil {
		return nil, err
	}
	defer t.Rollback(ctx) //nolint:errcheck // committed on success
	disabled, err := t.DisableEndpoint(ctx, params.ID)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	if !disabled {
		return nil, kerr.New(kerr.NotFound, "enabled webhook `%s`", params.ID)
	}
	if err := t.AppendAudit(ctx, requestAudit(a, "webhook.disabled", "webhook", params.ID.String())); err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	if err := t.Commit(ctx); err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	return &gen.DisableWebhookNoContent{}, nil
}

// PingWebhook sends a `ping` event to an endpoint.
func (s *Server) PingWebhook(ctx context.Context, params gen.PingWebhookParams) (gen.PingWebhookRes, error) {
	a, err := s.access(ctx)
	if err != nil {
		return nil, err
	}
	t, err := s.orgTenant(ctx, a, perm.OrgAdmin)
	if err != nil {
		return nil, err
	}
	defer t.Rollback(ctx) //nolint:errcheck // committed on success
	payload := map[string]any{"type": "ping", "created_at": s.deps.Clock.NowMs()}
	queued, err := t.EnqueueTo(ctx, params.ID, "ping", payload)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	if !queued {
		return nil, kerr.New(kerr.NotFound, "enabled webhook `%s`", params.ID)
	}
	if err := t.Commit(ctx); err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	return &gen.PingWebhookAccepted{}, nil
}

// ListWebhookDeliveries is the newest deliveries to an endpoint.
func (s *Server) ListWebhookDeliveries(ctx context.Context, params gen.ListWebhookDeliveriesParams) ([]gen.DeliveryDto, error) {
	a, err := s.access(ctx)
	if err != nil {
		return nil, err
	}
	t, err := s.orgTenant(ctx, a, perm.OrgAdmin)
	if err != nil {
		return nil, err
	}
	defer t.Rollback(ctx) //nolint:errcheck // read only
	found, err := t.Deliveries(ctx, params.ID, 100)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	out := make([]gen.DeliveryDto, 0, len(found))
	for _, d := range found {
		out = append(out, deliveryDto(d))
	}
	return out, nil
}

// RetryWebhookDelivery tries a failed delivery again, now.
func (s *Server) RetryWebhookDelivery(ctx context.Context, params gen.RetryWebhookDeliveryParams) (gen.RetryWebhookDeliveryRes, error) {
	a, err := s.access(ctx)
	if err != nil {
		return nil, err
	}
	t, err := s.orgTenant(ctx, a, perm.OrgAdmin)
	if err != nil {
		return nil, err
	}
	defer t.Rollback(ctx) //nolint:errcheck // committed on success
	retried, err := t.RetryDelivery(ctx, params.ID, params.Delivery)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	if !retried {
		return nil, kerr.New(kerr.NotFound, "failed delivery `%s` of an enabled webhook", params.Delivery)
	}
	if err := t.AppendAudit(ctx, requestAudit(a, "webhook.delivery.retried", "webhook", params.ID.String())); err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	if err := t.Commit(ctx); err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	return &gen.RetryWebhookDeliveryAccepted{}, nil
}
