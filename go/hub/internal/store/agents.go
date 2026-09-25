package store

// AgentLink records (ADR-027, migration 0011); the port of
// repo/agents.rs: enrollment tokens, the agent linked to each cluster, how
// a target's runs reach its cluster ([Delivery]) and the runtime
// observations agents report.
//
// An enrolling agent is anonymous until its token is redeemed, so the
// [Store] functions read these tables by token hash and cluster id; the
// tenant functions ([Tenant.CreateAgentToken],
// [Tenant.RevokeClusterAgent]) only touch clusters of the caller's
// organization.
//
//   - A token is kept as its SHA-256, bound to one cluster, expires, and is
//     redeemed once; the device that redeemed it may resume it for a grace
//     period after expiry (a lost answer never strands it).
//   - A cluster has one current agent device. Only that device, unrevoked,
//     may link or renew; a fresh enrollment with another device replaces a
//     revoked one, and the same device stays revoked (a trigger holds it).

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Teamtem-dev/kuben/go/hub/internal/core/clock"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/ids"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
)

const (
	clusterOfOrg     = "SELECT EXISTS (SELECT 1 FROM clusters WHERE id = $1 AND org_id = $2)"
	insertAgentToken = "INSERT INTO agent_tokens " +
		"(token_hash, org_id, cluster_id, expires_at, created_by, created_at) " +
		"VALUES ($1, $2, $3, kuben_now_ms() + $4, $5, kuben_now_ms())"
	tokenForUpdate = "SELECT org_id, cluster_id, expires_at, device_id, kuben_now_ms() AS now " +
		"FROM agent_tokens WHERE token_hash = $1 FOR UPDATE"
	redeemToken = "UPDATE agent_tokens SET device_id = $2, redeemed_at = kuben_now_ms() " +
		"WHERE token_hash = $1 AND device_id IS NULL"
	recordCertificate = "INSERT INTO cluster_agents " +
		"(cluster_id, org_id, device_id, certificate_not_after, updated_at) " +
		"VALUES ($1, $2, $3, $4, kuben_now_ms()) " +
		"ON CONFLICT (cluster_id) DO UPDATE SET " +
		"revoked_at = CASE WHEN cluster_agents.device_id = EXCLUDED.device_id " +
		"THEN cluster_agents.revoked_at END, " +
		"device_id = EXCLUDED.device_id, " +
		"certificate_not_after = EXCLUDED.certificate_not_after, " +
		"updated_at = EXCLUDED.updated_at " +
		"WHERE cluster_agents.org_id = EXCLUDED.org_id"
	clusterAgent = "SELECT cluster_id, org_id, device_id, certificate_not_after, protocol_version, " +
		"features::text AS features, agent_version, linked_at, last_seen_at, revoked_at " +
		"FROM cluster_agents WHERE cluster_id = $1"
	recordLink = "UPDATE cluster_agents " +
		"SET protocol_version = $3, features = $4::jsonb, agent_version = $5, " +
		"linked_at = kuben_now_ms(), last_seen_at = kuben_now_ms(), updated_at = kuben_now_ms() " +
		"WHERE cluster_id = $1 AND device_id = $2 AND revoked_at IS NULL"
	touchAgent = "UPDATE cluster_agents SET last_seen_at = kuben_now_ms() " +
		"WHERE cluster_id = $1 AND device_id = $2 AND revoked_at IS NULL"
	tokenOrg    = "SELECT org_id FROM agent_tokens WHERE cluster_id = $1 AND device_id = $2 LIMIT 1" //nolint:gosec // SQL, not a credential
	revokeAgent = "UPDATE cluster_agents SET revoked_at = kuben_now_ms(), updated_at = kuben_now_ms() " +
		"WHERE cluster_id = $1 AND org_id = $2 AND revoked_at IS NULL"
)

const recordObservation = "INSERT INTO runtime_observations " +
	"(target_id, org_id, project_id, generation, phase, reason, message, observed_at) " +
	"SELECT t.id, t.org_id, t.project_id, $3, $4, $5, $6, kuben_now_ms() " +
	"FROM application_targets t " +
	"JOIN environment_placements p ON p.id = t.placement_id AND p.org_id = t.org_id " +
	"WHERE t.id = $1 AND p.cluster_id = $2 AND t.org_id = $7 " +
	"ON CONFLICT (target_id) DO UPDATE " +
	"SET generation = EXCLUDED.generation, phase = EXCLUDED.phase, reason = EXCLUDED.reason, " +
	"message = EXCLUDED.message, observed_at = EXCLUDED.observed_at " +
	"WHERE runtime_observations.generation <= EXCLUDED.generation"

const (
	handOver = "UPDATE application_targets t SET delivery = 'agent' " +
		"FROM environment_placements p " +
		"WHERE t.id = $1 AND t.org_id = $2 AND t.delivery = 'controller' AND NOT t.deleting " +
		"AND p.id = t.placement_id AND p.org_id = t.org_id " +
		"AND EXISTS (SELECT 1 FROM cluster_agents a WHERE a.cluster_id = p.cluster_id AND a.org_id = p.org_id " +
		"AND a.revoked_at IS NULL AND a.features @> jsonb_build_array($3::text))"
	targetDelivery = "SELECT delivery FROM application_targets WHERE id = $1 AND org_id = $2"
	observation    = "SELECT generation, phase, reason, message, observed_at " +
		"FROM runtime_observations WHERE target_id = $1 AND org_id = $2"
)

// RuntimeObservation is the latest observation an agent reported for a
// target.
type RuntimeObservation struct {
	Generation int64
	// Phase is `accepted`, `applying`, `ready`, `failed`, `rejected` or
	// `unknown`.
	Phase   string
	Reason  opt.Val[string]
	Message opt.Val[string]
	// ObservedAt is unix milliseconds.
	ObservedAt int64
}

// Delivery is how a target's runs reach its cluster.
type Delivery string

// The deliveries, with their stored names.
const (
	// DeliveryController: the materializer writes the target's App; the App
	// controller carries it out.
	DeliveryController Delivery = "controller"
	// DeliveryAgent: the cluster's agent carries the target's execution
	// envelopes out.
	DeliveryAgent Delivery = "agent"
)

func (d Delivery) String() string { return string(d) }

// parseDelivery reads a stored delivery; anything else is a decode error.
func parseDelivery(op, value string) (Delivery, error) {
	switch d := Delivery(value); d {
	case DeliveryController, DeliveryAgent:
		return d, nil
	}
	return "", decodeErr(op, "unknown delivery %s", rustQuote(value))
}

// RecordRuntimeObservation records what the agent of cluster observed of
// target: only for a target on that cluster, and never over a newer
// generation. False when nothing was recorded.
func (t *Tenant) RecordRuntimeObservation(
	ctx context.Context, cluster ids.ClusterID, target ids.TargetID, generation int64, phase string,
	reason, message opt.Val[string],
) (bool, error) {
	n, err := exec(ctx, t.tx, "record a runtime observation", recordObservation,
		target, cluster, generation, phase, reason.Ptr(), message.Ptr(), t.org.String())
	return n == 1, err
}

// TargetDelivery is how tgt of this organization is delivered, if it
// exists.
func (t *Tenant) TargetDelivery(ctx context.Context, tgt ids.TargetID) (Delivery, bool, error) {
	const op = "read a target's delivery"
	return queryOpt(ctx, t.tx, op, targetDelivery, func(row pgx.CollectableRow) (Delivery, error) {
		var d string
		if err := row.Scan(&d); err != nil {
			return "", err
		}
		return parseDelivery(op, d)
	}, tgt, t.org.String())
}

// RuntimeObservation is the latest observation of tgt, if its agent
// reported one.
func (t *Tenant) RuntimeObservation(ctx context.Context, tgt ids.TargetID) (RuntimeObservation, bool, error) {
	return queryOpt(ctx, t.tx, "read a runtime observation", observation,
		func(row pgx.CollectableRow) (RuntimeObservation, error) {
			var o RuntimeObservation
			var reason, message *string
			if err := row.Scan(&o.Generation, &o.Phase, &reason, &message, &o.ObservedAt); err != nil {
				return RuntimeObservation{}, err
			}
			o.Reason, o.Message = opt.FromPtr(reason), opt.FromPtr(message)
			return o, nil
		}, tgt, t.org.String())
}

// HandOverToAgent hands tgt over from the App controller to its cluster's
// agent: its runs go through the agent from now on, never back (migration
// 0012). Only a live target the App controller delivers, and only when its
// cluster has a linked, unrevoked agent that carries applications; false
// otherwise.
func (t *Tenant) HandOverToAgent(ctx context.Context, tgt ids.TargetID) (bool, error) {
	n, err := exec(ctx, t.tx, "hand a target over to its agent", handOver, tgt, t.org.String(), RuntimeFeature)
	return n == 1, err
}

// TokenRedemption is how a token was redeemed.
type TokenRedemption int

// The redemptions.
const (
	// TokenFirst: for the first time.
	TokenFirst TokenRedemption = iota + 1
	// TokenResumed: again, by the device that redeemed it.
	TokenResumed
)

// TokenRefusal is why a token was not redeemed. The agent hears one answer
// for all.
type TokenRefusal int

// The refusals.
const (
	// TokenUnknown: no such token.
	TokenUnknown TokenRefusal = iota + 1
	// TokenExpired: the token expired (and its grace, for its device).
	TokenExpired
	// TokenOtherCluster: the token is for another cluster.
	TokenOtherCluster
	// TokenOtherDevice: another device redeemed the token.
	TokenOtherDevice
)

func (r TokenRefusal) Error() string {
	switch r {
	case TokenUnknown:
		return "unknown token"
	case TokenExpired:
		return "expired token"
	case TokenOtherCluster:
		return "a token of another cluster"
	case TokenOtherDevice:
		return "a token of another device"
	}
	return "unknown token"
}

// RedeemedToken is a redeemed token: the cluster's organization and how it
// was redeemed.
type RedeemedToken struct {
	Org     ids.OrgID
	Cluster ids.ClusterID
	Kind    TokenRedemption
}

// ClusterAgent is the agent linked to a cluster.
type ClusterAgent struct {
	Org      ids.OrgID
	Cluster  ids.ClusterID
	DeviceID string
	// CertificateNotAfter is unix milliseconds.
	CertificateNotAfter int64
	ProtocolVersion     opt.Val[uint32]
	Features            []string
	AgentVersion        opt.Val[string]
	LinkedAt            opt.Val[int64]
	LastSeenAt          opt.Val[int64]
	RevokedAt           opt.Val[int64]
}

// Accepts says whether device may link or renew for this cluster.
func (a ClusterAgent) Accepts(device string) bool {
	return a.RevokedAt.IsNone() && a.DeviceID == device
}

// CreateAgentToken stores a bootstrap token, hashed as hash, for cluster
// of this organization, valid for ttl. False when the organization has no
// such cluster.
func (t *Tenant) CreateAgentToken(ctx context.Context, cluster ids.ClusterID, hash [32]byte, ttl time.Duration, createdBy string) (bool, error) {
	const op = "create an agent token"
	org := t.org.String()
	var known bool
	if err := queryOne(ctx, t.tx, op, clusterOfOrg, []any{&known}, cluster, org); err != nil {
		return false, err
	}
	if !known {
		return false, nil
	}
	if _, err := exec(ctx, t.tx, op, insertAgentToken, hash[:], org, cluster, millis(ttl), createdBy); err != nil {
		return false, err
	}
	return true, nil
}

// RevokeClusterAgent revokes the agent of cluster: it may no longer link or
// renew until another device enrolls. False when nothing was revoked.
func (t *Tenant) RevokeClusterAgent(ctx context.Context, cluster ids.ClusterID) (bool, error) {
	n, err := exec(ctx, t.tx, "revoke a cluster agent", revokeAgent, cluster, t.org.String())
	return n == 1, err
}

type tokenRow struct {
	org       ids.OrgID
	cluster   ids.ClusterID
	expiresAt int64
	device    *string
	now       int64
}

// RedeemAgentToken redeems the token hashed as hash for device of cluster,
// by the database clock. The device that redeemed it may resume it until
// resumeGrace after its expiry. A refusal is a [TokenRefusal] error.
func (s *Store) RedeemAgentToken(ctx context.Context, hash [32]byte, cluster ids.ClusterID, device string, resumeGrace time.Duration) (_ RedeemedToken, err error) {
	const op = "redeem an agent token"
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return RedeemedToken{}, dbErr(op, err)
	}
	defer func() { err = errors.Join(err, rollback(ctx, tx)) }()
	row, found, err := queryOpt(ctx, tx, op, tokenForUpdate, func(r pgx.CollectableRow) (tokenRow, error) {
		var tr tokenRow
		err := r.Scan(&tr.org, &tr.cluster, &tr.expiresAt, &tr.device, &tr.now)
		return tr, err
	}, hash[:])
	switch {
	case err != nil:
		return RedeemedToken{}, err
	case !found:
		return RedeemedToken{}, TokenUnknown
	case row.cluster != cluster:
		return RedeemedToken{}, TokenOtherCluster
	}
	kind := TokenFirst
	switch {
	case row.device != nil && *row.device == device:
		if row.now >= clock.SaturatingAdd(row.expiresAt, millis(resumeGrace)) {
			return RedeemedToken{}, TokenExpired
		}
		kind = TokenResumed
	case row.device != nil:
		return RedeemedToken{}, TokenOtherDevice
	case row.now >= row.expiresAt:
		return RedeemedToken{}, TokenExpired
	default:
		if _, err := exec(ctx, tx, op, redeemToken, hash[:], device); err != nil {
			return RedeemedToken{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return RedeemedToken{}, dbErr(op, err)
	}
	return RedeemedToken{Org: row.org, Cluster: cluster, Kind: kind}, nil
}

// RecordAgentCertificate records the certificate issued to device for
// cluster: it becomes the cluster's agent device. Another device than a
// revoked one clears the revocation; the same device stays revoked.
func (s *Store) RecordAgentCertificate(ctx context.Context, org ids.OrgID, cluster ids.ClusterID, device string, notAfterMs int64) error {
	_, err := exec(ctx, s.db, "record an agent certificate", recordCertificate, cluster, org.String(), device, notAfterMs)
	return err
}

// ClusterAgent is the agent of cluster, if one ever enrolled.
func (s *Store) ClusterAgent(ctx context.Context, cluster ids.ClusterID) (ClusterAgent, bool, error) {
	const op = "read a cluster agent"
	return queryOpt(ctx, s.db, op, clusterAgent, func(row pgx.CollectableRow) (ClusterAgent, error) {
		var a ClusterAgent
		var version *int32
		var features string
		var agentVersion *string
		var linked, seen, revoked *int64
		if err := row.Scan(&a.Cluster, &a.Org, &a.DeviceID, &a.CertificateNotAfter, &version, &features,
			&agentVersion, &linked, &seen, &revoked); err != nil {
			return ClusterAgent{}, err
		}
		if version != nil {
			if *version < 0 {
				return ClusterAgent{}, decodeErr(op, "protocol version %d is negative", *version)
			}
			a.ProtocolVersion = opt.Some(uint32(*version))
		}
		if err := json.Unmarshal([]byte(features), &a.Features); err != nil {
			return ClusterAgent{}, decodeErr(op, "features: %v", err)
		}
		a.AgentVersion = opt.FromPtr(agentVersion)
		a.LinkedAt, a.LastSeenAt, a.RevokedAt = opt.FromPtr(linked), opt.FromPtr(seen), opt.FromPtr(revoked)
		return a, nil
	}, cluster)
}

// RecordAgentLink records a link of the cluster's current, unrevoked
// device. False when device is not that device (or it was revoked).
func (s *Store) RecordAgentLink(ctx context.Context, cluster ids.ClusterID, device string, protocolVersion uint32, features []string, agentVersion string) (bool, error) {
	const op = "record an agent link"
	if features == nil {
		features = []string{}
	}
	text, err := json.Marshal(features)
	if err != nil {
		return false, decodeErr(op, "features: %v", err)
	}
	version := int32(min(protocolVersion, math.MaxInt32)) //nolint:gosec // clamped, as Rust's unwrap_or(i32::MAX)
	n, err := exec(ctx, s.db, op, recordLink, cluster, device, version, string(text), agentVersion)
	return n == 1, err
}

// AgentTokenOrg is the organization of cluster, as the token device
// redeemed names it: where the first certificate of a cluster belongs.
func (s *Store) AgentTokenOrg(ctx context.Context, cluster ids.ClusterID, device string) (ids.OrgID, bool, error) {
	return queryOpt(ctx, s.db, "read an agent token's organization", tokenOrg, pgx.RowTo[ids.OrgID], cluster, device)
}

// TouchAgent notes that the cluster's agent was heard from.
func (s *Store) TouchAgent(ctx context.Context, cluster ids.ClusterID, device string) error {
	_, err := exec(ctx, s.db, "touch an agent", touchAgent, cluster, device)
	return err
}
