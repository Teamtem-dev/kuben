package store

// Cluster agents; a partial port of repo/agents.rs: how a target's runs
// reach its cluster ([Delivery]) and the runtime observations agents
// report, which the catalog and the materializer read. Enrollment, links
// and the rest of the agent protocol follow with the agent work.

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/Teamtem-dev/kuben/go/hub/internal/core/ids"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
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
