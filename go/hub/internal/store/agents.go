package store

// Cluster agents; a partial port of repo/agents.rs: how a target's runs
// reach its cluster ([Delivery]) and the runtime observations agents
// report, which the catalog and the materializer read. Enrollment, links
// and the rest of the agent protocol follow with the agent work.

import (
	"context"

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
