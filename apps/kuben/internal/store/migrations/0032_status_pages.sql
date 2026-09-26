-- M5.3: public status pages of projects.
--
-- A project may publish a status page under a global slug; anyone can read
-- it without signing in. The page lists the apps of the chosen
-- environments by display name, their state and their incidents, and
-- nothing else. The table has no row-level security: the public lookup
-- needs the slug before it knows the organization, and it holds only the
-- slug, the ids and the title.

CREATE TABLE status_pages (
  project_id      UUID PRIMARY KEY,
  org_id          TEXT NOT NULL REFERENCES organizations (id),
  slug            TEXT NOT NULL UNIQUE CHECK (slug ~ '^[a-z0-9]([-a-z0-9]{1,61}[a-z0-9])$'),
  title           TEXT NOT NULL CHECK (char_length(title) BETWEEN 1 AND 100),
  enabled         BOOLEAN NOT NULL DEFAULT TRUE,
  environment_ids UUID[] NOT NULL CHECK (cardinality(environment_ids) BETWEEN 1 AND 20),
  updated_by      TEXT NOT NULL,
  updated_at      BIGINT NOT NULL,
  FOREIGN KEY (org_id, project_id) REFERENCES projects (org_id, id)
);
