// Package keyring is Kuben's managed secret values at rest (ADR-030, M4.4);
// it replaces crates/kuben-platform/src/secrets.rs.
//
// Every revision gets a random data key (DEK). The values are sealed with
// AES-256-GCM under the DEK, bound by associated data to the organization,
// secret and revision; the DEK is sealed under a versioned key-encryption
// key (KEK), bound to the same identity and the KEK version. Moving a
// ciphertext to another row, organization or revision makes it unreadable.
// The KEKs live in a keyring file outside the database and its backups;
// the newest version seals, every listed version opens. The byte layout
// (nonce ‖ ciphertext ‖ tag) and the associated data are the Rust ones,
// pinned by testdata/compat/secrets.json, which the Rust code sealed.
package keyring

import "strconv"

// SecretID labels a revision object with the SQL id of its secret
// (secrets.rs SECRET_ID).
const SecretID = "kuben.dev/secret-id" //nolint:gosec // G101: a label name, not a credential

// SecretRevision labels a revision object with its revision
// (secrets.rs SECRET_REVISION).
const SecretRevision = "kuben.dev/secret-revision" //nolint:gosec // G101: a label name, not a credential

// ObjectName is the Kubernetes Secret that carries revision of secret
// name. Secret names made before M4.4 are DNS labels, so a name with a dot
// never collides with one of them.
func ObjectName(name string, revision uint64) string {
	return name + ".r" + strconv.FormatUint(revision, 10)
}
