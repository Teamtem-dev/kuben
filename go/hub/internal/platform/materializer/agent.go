package materializer

// Delivery through the cluster's agent (M1.9, ADR-027). For a target whose
// `delivery` is `agent`, the run's frozen plan goes to the cluster's agent
// as an execution envelope instead of an App object, and the run follows
// what the agent observed.
//
//   - The envelope carries the frozen plan as canonical JSON with its
//     digest, so the agent checks it before writing anything.
//   - Delivery is durable without a queue of its own: while the cluster's
//     agent is not linked the operation waits and is claimed again, and an
//     envelope that brings no observation is sent again (the apiserver takes
//     the same envelope again).
//   - The agent's observations, recorded in SQL by the hub, move the run:
//     accepted → acceptedByCluster; applying → verifying; ready →
//     succeeded; failed or rejected → failed with the agent's reason; a
//     newer generation → superseded.
//   - A target handed over from the App controller loses its App object
//     first: marked (the App controller then leaves it alone), then deleted
//     with orphan propagation, so the agent adopts the workloads in place
//     instead of making new ones.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/Teamtem-dev/kuben/go/hub/internal/core/ops/run"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
	"github.com/Teamtem-dev/kuben/go/hub/internal/store"
	"github.com/Teamtem-dev/kuben/go/hub/internal/wire"
	"github.com/Teamtem-dev/kuben/go/kubenapi/protocol"
	"github.com/Teamtem-dev/kuben/go/kubenapi/v1alpha1"
)

// AgentDispatch hands envelopes to the agent of a cluster: the hub in
// `kuben serve`.
type AgentDispatch interface {
	// Send sends apply to the agent of cluster over its live link; false
	// when that agent is not linked, or did not negotiate the runtime
	// feature.
	Send(ctx context.Context, cluster string, apply protocol.Apply) bool
}

const (
	// resend is how long an envelope may go without an observation before
	// it is sent again.
	resend = time.Minute
	// agentWait is how long a run waits for its cluster's agent before it
	// is claimed again.
	agentWait = 10 * time.Second
)

// Envelope is the envelope of m's run, carrying its frozen plan.
func Envelope(m *store.Materialization, plan store.RunPlan) (protocol.Apply, error) {
	resources, err := wire.CanonicalValue(plan.Resources)
	if err != nil {
		return protocol.Apply{}, fmt.Errorf("the plan's resources: %w", err)
	}
	digest := wire.SHA256(resources)
	if uint64(m.Generation) > math.MaxInt64 {
		return protocol.Apply{}, fmt.Errorf("generation %d is out of range", uint64(m.Generation))
	}
	generation := int64(m.Generation) //nolint:gosec // checked above
	spec := v1alpha1.ApplicationRuntimeSpec{
		TargetID:     m.Target.String(),
		LifecycleUID: m.LifecycleUID.String(),
		ControlEpoch: 0,
		Generation:   generation,
		InputHash:    wire.SHA256(fmt.Sprintf("%s/%s/%d/%s", m.Release, m.ConfigRevision, generation, digest)),
		ReleaseID:    m.Release.String(),
		Plan: v1alpha1.PlanEnvelope{
			ID: plan.ID.String(), RendererVersion: plan.RendererVersion, Digest: digest, Resources: resources,
		},
	}
	text, err := marshalUnescaped(spec)
	if err != nil {
		return protocol.Apply{}, err
	}
	return protocol.Apply{Target: m.Target.String(), Namespace: m.Namespace, Name: m.ApplicationSlug, Spec: text}, nil
}

// marshalUnescaped is serde_json::to_string of v: no HTML escaping, no
// trailing newline.
func marshalUnescaped(v any) (string, error) {
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(false)
	if err := e.Encode(v); err != nil {
		return "", fmt.Errorf("the envelope: %w", err)
	}
	return strings.TrimSuffix(b.String(), "\n"), nil
}

// Heard is what the agent's latest observation says about the run's
// generation.
//
//sumtype:decl
type Heard interface{ isHeard() }

type (
	// HeardNothing means nothing about this generation yet.
	HeardNothing struct{}
	// HeardAccepted means the envelope is written to the cluster.
	HeardAccepted struct{}
	// HeardApplying means its workloads are rolling out.
	HeardApplying struct{}
	// HeardReady means its workloads are available.
	HeardReady struct{}
	// HeardFailed means the agent failed it, or refused the envelope.
	HeardFailed struct{ Reason string }
	// HeardNewer means the agent already carries a newer generation.
	HeardNewer struct{}
)

func (HeardNothing) isHeard()  {}
func (HeardAccepted) isHeard() {}
func (HeardApplying) isHeard() {}
func (HeardReady) isHeard()    {}
func (HeardFailed) isHeard()   {}
func (HeardNewer) isHeard()    {}

// HeardOf reads observation for the run of generation.
func HeardOf(observation opt.Val[store.RuntimeObservation], generation int64) Heard {
	o, ok := observation.Get()
	switch {
	case !ok || o.Generation < generation:
		return HeardNothing{}
	case o.Generation > generation:
		return HeardNewer{}
	}
	switch o.Phase {
	case "accepted":
		return HeardAccepted{}
	case "applying":
		return HeardApplying{}
	case "ready":
		return HeardReady{}
	case "failed":
		return HeardFailed{Reason: o.Reason.Or("AgentFailed")}
	case "rejected":
		return HeardFailed{Reason: o.Reason.Or("EnvelopeRejected")}
	}
	return HeardNothing{}
}

// agentPhases are the phases an agent-delivered run is carried in.
var agentPhases = []run.Phase{run.PendingDelivery, run.AcceptedByCluster, run.Preflight, run.Applying, run.Verifying}

// driveAgent carries a run of an agent-delivered target.
func (w *Worker) driveAgent(ctx context.Context, claim store.Claim, m *store.Materialization) stop {
	phase := m.Phase
	if phase == run.Planned {
		var st stop
		if phase, st = w.advance(ctx, claim, m, run.EventReadyForDelivery); st != nil {
			return st
		}
	}
	if !slices.Contains(agentPhases, phase) {
		return stopRetry{err: unsupported(phase)}
	}
	apply, st := w.prepareEnvelope(ctx, claim, m)
	if st != nil {
		return st
	}
	return w.followAgent(ctx, claim, m, phase, apply)
}

// prepareEnvelope writes everything the envelope needs (the project and
// environment objects, the namespace, the frozen plan), then makes it.
func (w *Worker) prepareEnvelope(ctx context.Context, claim store.Claim, m *store.Materialization) (protocol.Apply, stop) {
	rendered, rerr := Render(m)
	if rerr != nil {
		return protocol.Apply{}, refused(rerr.Code)
	}
	if st := w.prepare(ctx, claim, m, &rendered); st != nil {
		return protocol.Apply{}, st
	}
	// A target handed over from the App controller still has its App
	// object: delete it, orphaning its workloads, and wait until it is
	// gone. The agent then adopts them in place, without new pods; with
	// the App left, they would have two controllers.
	if st := w.orphanAppObject(ctx, m.Namespace, m.ApplicationSlug, m.Org, m.Target); st != nil {
		return protocol.Apply{}, st
	}
	type planned struct {
		plan  store.RunPlan
		found bool
	}
	p, st := readTenant(ctx, w, m.Org, func(t *store.Tenant) (planned, error) {
		plan, found, err := t.RunRenderPlan(ctx, m.Run)
		return planned{plan, found}, err //nolint:wrapcheck // said as a store error
	})
	switch {
	case st != nil:
		return protocol.Apply{}, st
	case !p.found:
		return protocol.Apply{}, refused("PlanMissing")
	}
	apply, err := Envelope(m, p.plan)
	if err != nil {
		return protocol.Apply{}, refused("RenderFailed")
	}
	return apply, nil
}

// followAgent sends the envelope and follows the agent's observations
// until the run settles.
func (w *Worker) followAgent(ctx context.Context, claim store.Claim, m *store.Materialization, phase run.Phase, apply protocol.Apply) stop {
	agents, linked := w.d.Agents.Get()
	if !linked || agents == nil {
		return stopWait{after: agentWait, code: "NoAgentLink"}
	}
	cluster := m.Cluster.String()
	generation := int64(min(uint64(m.Generation), math.MaxInt64)) //nolint:gosec // clamped
	deadline := w.d.Clock.NowMs() + w.d.VerifyDeadline.Milliseconds()
	sent := opt.None[int64]()
	for {
		renewed, err := w.d.Store.RenewLease(ctx, claim, Lease)
		switch {
		case err != nil:
			return retryStore(err)
		case !renewed:
			return stopFenced{}
		}
		observation, st := readTenant(ctx, w, m.Org, func(t *store.Tenant) (opt.Val[store.RuntimeObservation], error) {
			o, found, err := t.RuntimeObservation(ctx, m.Target)
			if !found {
				return opt.None[store.RuntimeObservation](), err //nolint:wrapcheck // said as a store error
			}
			return opt.Some(o), err //nolint:wrapcheck // said as a store error
		})
		if st != nil {
			return st
		}
		h := HeardOf(observation, generation)
		if phase, st = w.heard(ctx, claim, m, phase, h); st != nil {
			return st
		}
		if _, nothing := h.(HeardNothing); nothing {
			if st := w.send(ctx, agents, cluster, apply, m, &sent); st != nil {
				return st
			}
		}
		if w.d.Clock.NowMs() >= deadline {
			return refused("VerifyTimeout")
		}
		if st := pause(ctx); st != nil {
			return st
		}
	}
}

// heard moves the run by what the agent observed; a stop once it settles.
func (w *Worker) heard(ctx context.Context, claim store.Claim, m *store.Materialization, phase run.Phase, h Heard) (run.Phase, stop) {
	var st stop
	switch h := h.(type) {
	case HeardNewer:
		return phase, stopSuperseded{}
	case HeardFailed:
		return phase, refused(h.Reason)
	case HeardReady:
		if phase, st = w.accepted(ctx, claim, m, phase); st != nil {
			return phase, st
		}
		if _, st = w.toVerifying(ctx, claim, m, phase); st != nil {
			return phase, st
		}
		done, st := w.advance(ctx, claim, m, run.EventVerified)
		if st != nil {
			return phase, st
		}
		return phase, settled(done, "")
	case HeardApplying:
		if phase, st = w.accepted(ctx, claim, m, phase); st != nil {
			return phase, st
		}
		return w.toVerifying(ctx, claim, m, phase)
	case HeardAccepted:
		return w.accepted(ctx, claim, m, phase)
	case HeardNothing:
	}
	return phase, nil
}

// send sends the envelope when it was never sent or went unanswered for
// resend; a paused target is held before the first send.
func (w *Worker) send(ctx context.Context, agents AgentDispatch, cluster string, apply protocol.Apply, m *store.Materialization, sent *opt.Val[int64]) stop {
	now := w.d.Clock.NowMs()
	at, wasSent := sent.Get()
	if wasSent && now-at < resend.Milliseconds() {
		return nil
	}
	if !wasSent {
		if st := w.holdIfPaused(ctx, m); st != nil {
			return st
		}
	}
	if !agents.Send(ctx, cluster, apply) {
		return stopWait{after: agentWait, code: "AgentUnavailable"}
	}
	*sent = opt.Some(now)
	return nil
}

// accepted: the agent persisted the envelope, so the run is accepted by
// the cluster.
func (w *Worker) accepted(ctx context.Context, claim store.Claim, m *store.Materialization, phase run.Phase) (run.Phase, stop) {
	if phase == run.PendingDelivery {
		return w.advance(ctx, claim, m, run.EventAcceptedByCluster)
	}
	return phase, nil
}
