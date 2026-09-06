# ADR-023: Lease-based leader election for the controllers

**Status:** decided · **Date:** 2026-09-11

## Context

The Helm chart allowed `replicaCount > 1` with PostgreSQL, but every replica
ran the full controller set. Two replicas then reconciled the same objects
concurrently and both server-side-applied with `force`. The chart also used
the `Recreate` strategy even with PostgreSQL, so every upgrade had downtime.
Login throttling was kept in each process's memory, so N replicas gave an
attacker N times the budget.

## Decision

- Only the holder of the `coordination.k8s.io/v1` Lease `kuben-controller`,
  in Kuben's own namespace, runs the controllers (`kuben_platform::leader`).
  Every replica still serves the API and runs the informers, because the API
  reads from the projections.
- Every write to the Lease is a compare-and-swap on its `resourceVersion`.
  A candidate treats the Lease as expired only after it has seen the record
  unchanged for the lease duration (15 s). It never compares `renewTime` with
  its own clock, so skew between nodes cannot produce two leaders.
- The leader renews every 2 s. If renewal fails for 10 s it stops its
  controllers before the Lease can expire for the others. On shutdown it
  releases the Lease, so the next replica takes over at once.
- The Lease right is a namespaced `Role`, not part of the `ClusterRole`.
- With PostgreSQL the chart uses rolling updates (`maxUnavailable: 0`) and a
  PodDisruptionBudget. SQLite stays on one replica with `Recreate`.
- Login-throttle windows move to the database (`login_throttle`, keyed by
  SHA-256 hashes), so all replicas share one budget.
- Leader election is off by default for the plain binary
  (`kube.leader_election`) and always on in the chart.

## Consequences

- After a crash the controllers pause for up to 15 s. After a graceful stop
  the pause is a few seconds.
- Anything that must happen once per cluster has to run under the Lease, never
  on every replica.
- Session revocation reaches other replicas within the session-cache TTL
  (5 s), not instantly.
- The API role and the controller role can later get separate service
  accounts with different RBAC (ADR-022). This decision does not require it.
