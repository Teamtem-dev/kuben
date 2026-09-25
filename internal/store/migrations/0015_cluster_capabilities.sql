-- M2.1 (ADR-031): what each cluster can do, as Kuben last discovered it —
-- Gateway API CRDs and their channel, GatewayClasses, cert-manager and its
-- ClusterIssuers, the Metrics API. Rendering, the gateway and Doctor gate
-- their features on these facts, not on what the configuration asks for.
--
-- One row per cluster of an organization. The facts are infrastructure
-- facts, but a cluster row belongs to its organization, so the row does too.
-- A newer observation replaces an older one, never the other way round.
CREATE TABLE cluster_capabilities (
  cluster_id  UUID PRIMARY KEY,
  org_id      TEXT NOT NULL,
  facts       JSONB NOT NULL,
  observed_at BIGINT NOT NULL,
  FOREIGN KEY (org_id, cluster_id) REFERENCES clusters (org_id, id) ON DELETE CASCADE
);

ALTER TABLE cluster_capabilities ENABLE ROW LEVEL SECURITY;
ALTER TABLE cluster_capabilities FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant ON cluster_capabilities
  USING (org_id = kuben_current_org()) WITH CHECK (org_id = kuben_current_org());
