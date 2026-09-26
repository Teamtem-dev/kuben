package store

// Incidents, webhook endpoints and deliveries, and the context of settled
// operations (M4.10, migration 0028); the port of repo/notify.rs.

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Teamtem-dev/kuben/internal/core/ids"
	"github.com/Teamtem-dev/kuben/internal/core/opt"
)

const (
	openIncidentSQL = "INSERT INTO incidents " +
		"(id, org_id, project_id, environment_id, target_id, kind, severity, dedupe_key, title, detail, " +
		"opened_at, last_seen_at) " +
		"VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $11) " +
		"ON CONFLICT (org_id, dedupe_key) WHERE resolved_at IS NULL DO UPDATE " +
		"SET last_seen_at = EXCLUDED.last_seen_at, occurrences = incidents.occurrences + 1, " +
		"detail = EXCLUDED.detail, severity = EXCLUDED.severity " +
		"RETURNING id, (xmax = 0) AS opened"
	resolveKey = "UPDATE incidents SET resolved_at = $3, resolved_by = $4 " +
		"WHERE org_id = $1 AND dedupe_key = $2 AND resolved_at IS NULL RETURNING id"
	resolveID = "UPDATE incidents SET resolved_at = $3, resolved_by = $4 " +
		"WHERE org_id = $1 AND id = $2 AND resolved_at IS NULL"
	acknowledgeIncident = "UPDATE incidents SET acknowledged_at = $3, acknowledged_by = $4 " +
		"WHERE org_id = $1 AND id = $2 AND resolved_at IS NULL AND acknowledged_at IS NULL"
	selectIncidents = "SELECT id, project_id, environment_id, target_id, kind, severity, dedupe_key, title, " +
		"detail, opened_at, last_seen_at, occurrences, acknowledged_at, acknowledged_by, resolved_at, resolved_by " +
		"FROM incidents WHERE org_id = $1 AND ($2 OR resolved_at IS NULL) ORDER BY opened_at DESC LIMIT $3"
	insertEndpoint = "INSERT INTO webhook_endpoints " +
		"(id, org_id, name, url, events, secret, wrapped_key, key_version, created_by, created_at) " +
		"VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)"
	selectEndpoints = "SELECT id, name, url, events, created_by, created_at, disabled_at, failures " +
		"FROM webhook_endpoints WHERE org_id = $1 ORDER BY name"
	disableEndpoint = "UPDATE webhook_endpoints SET disabled_at = $3 " +
		"WHERE org_id = $1 AND id = $2 AND disabled_at IS NULL"
	selectSubscribed = "SELECT id FROM webhook_endpoints " +
		"WHERE org_id = $1 AND disabled_at IS NULL AND ($2 = ANY(events) OR '*' = ANY(events))"
	selectEndpointSecret = "SELECT url, secret, wrapped_key, key_version FROM webhook_endpoints " +
		"WHERE org_id = $1 AND id = $2 AND disabled_at IS NULL"
	enqueueDelivery = "INSERT INTO webhook_deliveries " +
		"(id, org_id, endpoint_id, event_id, event, payload, next_attempt_at, created_at) " +
		"VALUES ($1, $2, $3, $4, $5, $6::jsonb, $7, $7) ON CONFLICT (endpoint_id, event_id) DO NOTHING"
	takeDeliveries = "UPDATE webhook_deliveries SET next_attempt_at = $2 + $3, attempts = attempts + 1 " +
		"WHERE id IN (SELECT id FROM webhook_deliveries " +
		"WHERE status = 'pending' AND next_attempt_at <= $2 " +
		"ORDER BY next_attempt_at, id FOR UPDATE SKIP LOCKED LIMIT $1) " +
		"RETURNING id, org_id, endpoint_id, event_id, event, payload::text AS payload, attempts, created_at"
	deliveredSQL = "UPDATE webhook_deliveries " +
		"SET status = 'delivered', last_status = $2, last_error = NULL, finished_at = $3 WHERE id = $1"
	resetFailures = "UPDATE webhook_endpoints SET failures = 0 " +
		"WHERE id = (SELECT endpoint_id FROM webhook_deliveries WHERE id = $1)"
	retryDeliverySQL = "UPDATE webhook_deliveries " +
		"SET next_attempt_at = $2, last_status = $3, last_error = $4 WHERE id = $1 AND status = 'pending'"
	giveUp = "UPDATE webhook_deliveries " +
		"SET status = 'failed', last_status = $2, last_error = $3, finished_at = $4 WHERE id = $1"
	countFailure = "UPDATE webhook_endpoints SET failures = failures + 1, " +
		"disabled_at = CASE WHEN failures + 1 >= $2 THEN $3 ELSE disabled_at END " +
		"WHERE id = (SELECT endpoint_id FROM webhook_deliveries WHERE id = $1)"
	retryFailed = "UPDATE webhook_deliveries d SET status = 'pending', next_attempt_at = $4, " +
		"attempts = 0, finished_at = NULL, last_error = NULL " +
		"FROM webhook_endpoints e " +
		"WHERE d.id = $3 AND d.endpoint_id = $2 AND d.org_id = $1 AND d.status = 'failed' " +
		"AND e.id = d.endpoint_id AND e.org_id = d.org_id AND e.disabled_at IS NULL"
	selectDeliveries = "SELECT id, event, status, attempts, last_status, last_error, created_at, finished_at " +
		"FROM webhook_deliveries WHERE org_id = $1 AND endpoint_id = $2 ORDER BY created_at DESC LIMIT $3"
	runContext = "SELECT r.project_id, p.environment_id, r.target_id, pr.slug AS project, " +
		"e.slug AS environment, a.slug AS app, r.reason, r.generation, rel.source::text AS source, " +
		"b.installation_id " +
		"FROM deployment_runs r " +
		"JOIN application_targets t ON t.id = r.target_id AND t.org_id = r.org_id " +
		"JOIN environment_placements p ON p.id = t.placement_id AND p.org_id = r.org_id " +
		"JOIN environments e ON e.id = p.environment_id AND e.org_id = r.org_id " +
		"JOIN projects pr ON pr.id = r.project_id AND pr.org_id = r.org_id " +
		"JOIN applications a ON a.id = t.application_id AND a.org_id = r.org_id " +
		"JOIN releases rel ON rel.id = r.release_id AND rel.org_id = r.org_id " +
		"LEFT JOIN source_bindings b ON b.target_id = r.target_id AND b.org_id = r.org_id " +
		"WHERE r.operation_id = $1 AND r.org_id = $2"
	buildContext = "SELECT ba.project_id, p.environment_id, ba.target_id, pr.slug AS project, " +
		"e.slug AS environment, a.slug AS app, ba.phase AS reason, 0::bigint AS generation, " +
		"jsonb_build_object('repository', ba.repository, 'commit', ba.commit_sha)::text AS source, " +
		"b.installation_id " +
		"FROM build_attempts ba " +
		"JOIN application_targets t ON t.id = ba.target_id AND t.org_id = ba.org_id " +
		"JOIN environment_placements p ON p.id = t.placement_id AND p.org_id = ba.org_id " +
		"JOIN environments e ON e.id = p.environment_id AND e.org_id = ba.org_id " +
		"JOIN projects pr ON pr.id = ba.project_id AND pr.org_id = ba.org_id " +
		"JOIN applications a ON a.id = ba.application_id AND a.org_id = ba.org_id " +
		"JOIN source_bindings b ON b.id = ba.binding_id AND b.org_id = ba.org_id " +
		"WHERE ba.operation_id = $1 AND ba.org_id = $2"
)

// MaxEndpointFailures is the number of given-up deliveries in a row after
// which an endpoint is disabled.
const MaxEndpointFailures int32 = 20

// maxDeliveryError is the longest last_error kept, in characters (the
// column's CHECK).
const maxDeliveryError = 1024

// NewIncident is an incident to open (or to count again).
type NewIncident struct {
	Project     opt.Val[ids.ProjectID]
	Environment opt.Val[ids.EnvironmentID]
	Target      opt.Val[ids.TargetID]
	Kind        string
	// Severity: `critical`, `warning` or `info`.
	Severity  string
	DedupeKey string
	Title     string
	Detail    opt.Val[string]
}

// Incident is an incident as stored.
type Incident struct {
	ID             uuid.UUID
	ProjectID      opt.Val[uuid.UUID]
	EnvironmentID  opt.Val[uuid.UUID]
	TargetID       opt.Val[uuid.UUID]
	Kind           string
	Severity       string
	DedupeKey      string
	Title          string
	Detail         opt.Val[string]
	OpenedAt       int64
	LastSeenAt     int64
	Occurrences    int64
	AcknowledgedAt opt.Val[int64]
	AcknowledgedBy opt.Val[string]
	ResolvedAt     opt.Val[int64]
	ResolvedBy     opt.Val[string]
}

// Endpoint is a webhook endpoint, without its secret.
type Endpoint struct {
	ID         uuid.UUID
	Name       string
	URL        string
	Events     []string
	CreatedBy  string
	CreatedAt  int64
	DisabledAt opt.Val[int64]
	Failures   int32
}

// WebhookDelivery is a delivery handed to a worker.
type WebhookDelivery struct {
	ID       uuid.UUID
	Org      ids.OrgID
	Endpoint uuid.UUID
	EventID  uuid.UUID
	Event    string
	// Payload is the event's JSON value (jsonx.DecodeAny).
	Payload   any
	Attempts  int32
	CreatedAt int64
}

// DeliveryRecord is a delivery as the API lists it.
type DeliveryRecord struct {
	ID         uuid.UUID
	Event      string
	Status     string
	Attempts   int32
	LastStatus opt.Val[int32]
	LastError  opt.Val[string]
	CreatedAt  int64
	FinishedAt opt.Val[int64]
}

// OperationContext is where a settled operation happened.
type OperationContext struct {
	ProjectID     uuid.UUID
	EnvironmentID uuid.UUID
	TargetID      uuid.UUID
	Project       string
	Environment   string
	App           string
	// Reason is a run's reason, or a build's phase.
	Reason     string
	Generation int64
	// Source is the release's (or build's) source as JSON text:
	// repository and commit, if any.
	Source opt.Val[string]
	// InstallationID is the GitHub App installation of the app's Git
	// source, if any.
	InstallationID opt.Val[int64]
}

// OpenIncident opens an incident, or counts it again while one with its key is open.
// It returns its id and whether it is new.
func (t *Tenant) OpenIncident(ctx context.Context, i NewIncident) (uuid.UUID, bool, error) {
	const op = "open an incident"
	var (
		id     uuid.UUID
		opened bool
	)
	err := queryOne(ctx, t.tx, op, openIncidentSQL, []any{&id, &opened},
		uuid.Must(uuid.NewV7()), t.org.String(), idArg(i.Project), idArg(i.Environment), idArg(i.Target),
		i.Kind, i.Severity, i.DedupeKey, i.Title, i.Detail.Ptr(), t.store.now())
	if err != nil {
		return uuid.Nil, false, err
	}
	return id, opened, nil
}

// ResolveIncidentKey resolves the open incident with dedupeKey, if there is
// one, and returns its id.
func (t *Tenant) ResolveIncidentKey(ctx context.Context, dedupeKey, by string) (uuid.UUID, bool, error) {
	return queryOpt(ctx, t.tx, "resolve an incident", resolveKey, pgx.RowTo[uuid.UUID],
		t.org.String(), dedupeKey, t.store.now(), by)
}

// ResolveIncident resolves incident id by hand. False when it is resolved
// already.
func (t *Tenant) ResolveIncident(ctx context.Context, id uuid.UUID, by string) (bool, error) {
	return t.touchIncident(ctx, "resolve an incident", resolveID, id, by)
}

// AcknowledgeIncident acknowledges incident id. False when it is resolved
// or acknowledged.
func (t *Tenant) AcknowledgeIncident(ctx context.Context, id uuid.UUID, by string) (bool, error) {
	return t.touchIncident(ctx, "acknowledge an incident", acknowledgeIncident, id, by)
}

func (t *Tenant) touchIncident(ctx context.Context, op, sql string, id uuid.UUID, by string) (bool, error) {
	n, err := exec(ctx, t.tx, op, sql, t.org.String(), id, t.store.now(), by)
	return n == 1, err
}

// Incidents are the newest limit incidents, open ones only unless all.
func (t *Tenant) Incidents(ctx context.Context, all bool, limit int64) ([]Incident, error) {
	return queryAll(ctx, t.tx, "list incidents", selectIncidents, scanIncident, t.org.String(), all, limit)
}

func scanIncident(row pgx.CollectableRow) (Incident, error) {
	var (
		i                                  Incident
		project, environment, target       *uuid.UUID
		detail, acknowledgedBy, resolvedBy *string
		acknowledgedAt, resolvedAt         *int64
	)
	if err := row.Scan(&i.ID, &project, &environment, &target, &i.Kind, &i.Severity, &i.DedupeKey, &i.Title,
		&detail, &i.OpenedAt, &i.LastSeenAt, &i.Occurrences, &acknowledgedAt, &acknowledgedBy,
		&resolvedAt, &resolvedBy); err != nil {
		return Incident{}, err
	}
	i.ProjectID, i.EnvironmentID, i.TargetID = opt.FromPtr(project), opt.FromPtr(environment), opt.FromPtr(target)
	i.Detail, i.AcknowledgedBy, i.ResolvedBy = opt.FromPtr(detail), opt.FromPtr(acknowledgedBy), opt.FromPtr(resolvedBy)
	i.AcknowledgedAt, i.ResolvedAt = opt.FromPtr(acknowledgedAt), opt.FromPtr(resolvedAt)
	return i, nil
}

// CreateEndpoint adds a webhook endpoint whose signing secret is secret
// (sealed for id, revision 0).
func (t *Tenant) CreateEndpoint(
	ctx context.Context, id uuid.UUID, name, url string, events []string, secret SealedBytes, by string,
) error {
	const op = "create a webhook endpoint"
	version, err := keyVersionColumn(op, secret.KeyVersion)
	if err != nil {
		return err
	}
	if events == nil {
		events = []string{}
	}
	_, err = exec(ctx, t.tx, op, insertEndpoint, id, t.org.String(), name, url, events,
		secret.Ciphertext, secret.WrappedKey, version, by, t.store.now())
	return err
}

// Endpoints are the organization's endpoints, by name.
func (t *Tenant) Endpoints(ctx context.Context) ([]Endpoint, error) {
	return queryAll(ctx, t.tx, "list webhook endpoints", selectEndpoints, func(row pgx.CollectableRow) (Endpoint, error) {
		var (
			e        Endpoint
			disabled *int64
		)
		if err := row.Scan(&e.ID, &e.Name, &e.URL, &e.Events, &e.CreatedBy, &e.CreatedAt, &disabled, &e.Failures); err != nil {
			return Endpoint{}, err
		}
		e.DisabledAt = opt.FromPtr(disabled)
		return e, nil
	}, t.org.String())
}

// DisableEndpoint disables endpoint id for good. False when there is no
// enabled one.
func (t *Tenant) DisableEndpoint(ctx context.Context, id uuid.UUID) (bool, error) {
	n, err := exec(ctx, t.tx, "disable a webhook endpoint", disableEndpoint, t.org.String(), id, t.store.now())
	return n == 1, err
}

// EnqueueEvent queues event for every enabled endpoint subscribed to it and
// returns the number queued (a repeated event id is queued once).
func (t *Tenant) EnqueueEvent(ctx context.Context, eventID uuid.UUID, event string, payload any) (uint64, error) {
	const op = "queue a webhook event"
	org := t.org.String()
	endpoints, err := queryAll(ctx, t.tx, op, selectSubscribed, pgx.RowTo[uuid.UUID], org, event)
	if err != nil {
		return 0, err
	}
	text, err := canonical(op, payload)
	if err != nil {
		return 0, err
	}
	var queued uint64
	for _, endpoint := range endpoints {
		n, err := exec(ctx, t.tx, op, enqueueDelivery,
			uuid.Must(uuid.NewV7()), org, endpoint, eventID, event, text, t.store.now())
		if err != nil {
			return 0, err
		}
		queued += n
	}
	return queued, nil
}

// EnqueueTo queues event for the enabled endpoint alone (a ping). False
// when it is not enabled.
func (t *Tenant) EnqueueTo(ctx context.Context, endpoint uuid.UUID, event string, payload any) (bool, error) {
	const op = "queue a webhook event"
	if _, _, ok, err := t.EndpointSecret(ctx, endpoint); err != nil || !ok {
		return false, err
	}
	text, err := canonical(op, payload)
	if err != nil {
		return false, err
	}
	_, err = exec(ctx, t.tx, op, enqueueDelivery, uuid.Must(uuid.NewV7()), t.org.String(), endpoint,
		uuid.Must(uuid.NewV7()), event, text, t.store.now())
	return err == nil, err
}

// EndpointSecret is the URL and sealed secret of the enabled endpoint id.
func (t *Tenant) EndpointSecret(ctx context.Context, id uuid.UUID) (string, SealedBytes, bool, error) {
	const op = "read a webhook secret"
	type row struct {
		url    string
		sealed SealedBytes
	}
	found, ok, err := queryOpt(ctx, t.tx, op, selectEndpointSecret, func(r pgx.CollectableRow) (row, error) {
		var (
			out     row
			version int32
		)
		if err := r.Scan(&out.url, &out.sealed.Ciphertext, &out.sealed.WrappedKey, &version); err != nil {
			return row{}, err
		}
		v, err := keyVersion(op, version)
		out.sealed.KeyVersion = v
		return out, err
	}, t.org.String(), id)
	if err != nil || !ok {
		return "", SealedBytes{}, false, err
	}
	return found.url, found.sealed, true, nil
}

// Deliveries are the newest limit deliveries to endpoint id.
func (t *Tenant) Deliveries(ctx context.Context, id uuid.UUID, limit int64) ([]DeliveryRecord, error) {
	return queryAll(ctx, t.tx, "list webhook deliveries", selectDeliveries, func(row pgx.CollectableRow) (DeliveryRecord, error) {
		var (
			d          DeliveryRecord
			lastStatus *int32
			lastError  *string
			finished   *int64
		)
		if err := row.Scan(&d.ID, &d.Event, &d.Status, &d.Attempts, &lastStatus, &lastError, &d.CreatedAt, &finished); err != nil {
			return DeliveryRecord{}, err
		}
		d.LastStatus, d.LastError, d.FinishedAt = opt.FromPtr(lastStatus), opt.FromPtr(lastError), opt.FromPtr(finished)
		return d, nil
	}, t.org.String(), id, limit)
}

// RetryDelivery tries failed delivery of the enabled endpoint again, now.
// False when it is not a failed delivery of an enabled endpoint.
func (t *Tenant) RetryDelivery(ctx context.Context, endpoint, delivery uuid.UUID) (bool, error) {
	n, err := exec(ctx, t.tx, "retry a webhook delivery", retryFailed, t.org.String(), endpoint, delivery, t.store.now())
	return n == 1, err
}

// OperationContext is where the settled operation happened: a deployment
// run's or (with build) a build's target, names and source.
func (t *Tenant) OperationContext(ctx context.Context, operation ids.OperationID, build bool) (OperationContext, bool, error) {
	sql := runContext
	if build {
		sql = buildContext
	}
	return queryOpt(ctx, t.tx, "read an operation's context", sql, func(row pgx.CollectableRow) (OperationContext, error) {
		var (
			c            OperationContext
			source       *string
			installation *int64
		)
		if err := row.Scan(&c.ProjectID, &c.EnvironmentID, &c.TargetID, &c.Project, &c.Environment, &c.App,
			&c.Reason, &c.Generation, &source, &installation); err != nil {
			return OperationContext{}, err
		}
		c.Source, c.InstallationID = opt.FromPtr(source), opt.FromPtr(installation)
		return c, nil
	}, operation.UUID(), t.org.String())
}

// TakeDeliveries hands out up to limit due deliveries, hidden for leaseMs.
func (s *Store) TakeDeliveries(ctx context.Context, limit, leaseMs int64) ([]WebhookDelivery, error) {
	const op = "take webhook deliveries"
	type deliveryRow struct {
		d       WebhookDelivery
		org     string
		payload string
	}
	rows, err := queryAll(ctx, s.db, op, takeDeliveries, func(row pgx.CollectableRow) (deliveryRow, error) {
		var r deliveryRow
		err := row.Scan(&r.d.ID, &r.org, &r.d.Endpoint, &r.d.EventID, &r.d.Event, &r.payload, &r.d.Attempts, &r.d.CreatedAt)
		return r, err
	}, limit, s.now(), leaseMs)
	if err != nil {
		return nil, err
	}
	out := make([]WebhookDelivery, 0, len(rows))
	for _, r := range rows {
		if r.d.Org, err = orgID(op, r.org); err != nil {
			return nil, err
		}
		if r.d.Payload, err = jsonValue(op, r.payload); err != nil {
			return nil, err
		}
		out = append(out, r.d)
	}
	return out, nil
}

// DeliverySucceeded records that delivery id was accepted with HTTP status.
func (s *Store) DeliverySucceeded(ctx context.Context, id uuid.UUID, status int32) (err error) {
	const op = "record a webhook delivery"
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return dbErr(op, err)
	}
	defer func() { err = errors.Join(err, rollback(ctx, tx)) }()
	if _, err := exec(ctx, tx, op, deliveredSQL, id, status, s.now()); err != nil {
		return err
	}
	if _, err := exec(ctx, tx, op, resetFailures, id); err != nil {
		return err
	}
	return dbErr(op, tx.Commit(ctx))
}

// DeliveryFailed records that delivery id failed: it is tried again at
// retryAt or, without one, given up and counted against its endpoint.
func (s *Store) DeliveryFailed(ctx context.Context, id uuid.UUID, status opt.Val[int32], reason string, retryAt opt.Val[int64]) (err error) {
	const op = "record a webhook delivery"
	reason = truncateChars(reason, maxDeliveryError)
	if at, ok := retryAt.Get(); ok {
		_, err := exec(ctx, s.db, op, retryDeliverySQL, id, at, status.Ptr(), reason)
		return err
	}
	now := s.now()
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return dbErr(op, err)
	}
	defer func() { err = errors.Join(err, rollback(ctx, tx)) }()
	if _, err := exec(ctx, tx, op, giveUp, id, status.Ptr(), reason, now); err != nil {
		return err
	}
	if _, err := exec(ctx, tx, op, countFailure, id, MaxEndpointFailures, now); err != nil {
		return err
	}
	return dbErr(op, tx.Commit(ctx))
}
