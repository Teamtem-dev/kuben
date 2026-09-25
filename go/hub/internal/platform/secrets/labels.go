// Package secrets is Kuben's managed secrets on the cluster side. It
// replaces crates/kuben-platform/src/secrets.rs; for now it holds only the
// labels of the revision objects, which the routes select on. The keyring
// and the sealing of revisions arrive with slice S2.
package secrets

// SecretID labels a revision object with the SQL id of its secret
// (secrets.rs SECRET_ID).
const SecretID = "kuben.dev/secret-id" //nolint:gosec // G101: a label name, not a credential

// SecretRevision labels a revision object with its revision
// (secrets.rs SECRET_REVISION).
const SecretRevision = "kuben.dev/secret-revision" //nolint:gosec // G101: a label name, not a credential
