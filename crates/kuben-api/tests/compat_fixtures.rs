//! Fixtures for the Go port (plan §6): password hashes, API tokens, session
//! hashes and webhook signatures as this Rust code makes them. Run on demand:
//!
//!   cargo test -p kuben-api --test compat_fixtures -- --ignored
//!
//! Removed with the Rust code (phase G7).

use std::path::PathBuf;

use argon2::Params;
use base64::{Engine as _, engine::general_purpose::STANDARD};
use kuben_api::{
    auth::{password::Hasher, session},
    notify,
};
use serde_json::json;

#[test]
#[ignore = "writes fixtures for the Go port"]
fn write_compat_fixtures() {
    let passwords: Vec<_> = [
        (
            "correct horse battery staple",
            Params::new(19_456, 2, 1, None).expect("params"),
        ),
        (
            "\u{633}\u{644}\u{627}\u{645}-\u{2713}-<&>",
            Params::new(8, 1, 1, None).expect("params"),
        ),
    ]
    .into_iter()
    .map(|(password, params)| {
        let phc = Hasher::with_params(params).hash(password).expect("hash");
        json!({ "password": password, "phc": phc })
    })
    .collect();

    let tokens: Vec<_> = (0..3)
        .map(|_| {
            let (plaintext, id, hash) = session::new_api_token();
            let (parsed, secret) = session::parse_api_token(&plaintext).expect("parse");
            assert_eq!(parsed, id);
            json!({
                "plaintext": plaintext,
                "id": id.to_string(),
                "secret": secret,
                "sha256": STANDARD.encode(hash),
                "displayPrefix": session::token_display_prefix(&plaintext),
            })
        })
        .collect();

    let signatures: Vec<_> = [
        ("whsec_test", 1_789_639_200_i64, r#"{"event":"ping"}"#),
        ("whsec_\u{fc}nicode", 0, ""),
        ("s", -1, "<>&\n"),
    ]
    .into_iter()
    .map(|(secret, t, body)| {
        json!({
            "secret": secret,
            "t": t,
            "body": body,
            "header": notify::signature(secret.as_bytes(), t, body.as_bytes()),
        })
    })
    .collect();

    let dir = PathBuf::from(env!("CARGO_MANIFEST_DIR")).join("../../go/hub/testdata/compat");
    std::fs::create_dir_all(&dir).expect("fixture dir");
    let text = serde_json::to_string_pretty(&json!({
        "passwords": passwords,
        "tokens": tokens,
        "signatures": signatures,
        "session": { "input": "session-id", "sha256": STANDARD.encode(session::sha256(b"session-id")) },
    }))
    .expect("json");
    std::fs::write(dir.join("auth.json"), text + "\n").expect("write");
}
