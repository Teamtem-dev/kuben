-- M2.6 (plan §11.2): the installer keeps a local journal before any database
-- exists; once the server is up it copies the journal here, one row per host,
-- for Doctor, support and the console. The host file stays the authority for
-- the installer; this is its latest copy. Installation-wide, not tenant data.
CREATE TABLE install_journals (
  host        TEXT PRIMARY KEY,
  journal     JSONB NOT NULL,
  recorded_at BIGINT NOT NULL
);
