//! Fixtures for the Go port (plan §6): what this Rust code writes, so the Go
//! code can prove it reads and writes the same bytes. Run on demand:
//!
//!   cargo test -p kuben-platform --lib compat_fixtures -- --ignored
//!
//! Removed with the Rust code (phase G7).

use std::{collections::BTreeMap, path::PathBuf};

use base64::{Engine as _, engine::general_purpose::STANDARD};
use serde_json::{Value, json};

use crate::{
    render::{canonical, sha256},
    secrets::{Identity, Keyring},
};

fn out(name: &str) -> PathBuf {
    let dir = PathBuf::from(env!("CARGO_MANIFEST_DIR")).join("../../go/hub/testdata/compat");
    std::fs::create_dir_all(&dir).expect("fixture dir");
    dir.join(name)
}

/// JSON texts chosen to pin serde_json's number and string printing.
const CANONICAL_INPUTS: &[&str] = &[
    r#"{"b":1,"a":{"d":[3,2,1],"c":null},"A":true,"\u00e9":"\u00e9","_":false}"#,
    r"[0,-0,1,-1,9007199254740993,18446744073709551615,-9223372036854775808]",
    r"[0.0,-0.0,1.0,1.5,0.1,0.30000000000000004,100.0,1e21,1e22,123456789012345680000.0,1e-7,0.000001,1.5e-10,2.5e300,-3.25]",
    r#"["<>&'","\"quoted\" \\ back","tab\tline\nfeed\rcr\b\f","\u0000\u001f\u007f","\u2028\u2029","emoji \ud83d\ude00 \u2713","\u0641\u0627\u0631\u0633\u06cc"]"#,
    r#"{"z":{},"y":[],"x":"","w":[{}],"v":[[]]}"#,
    r#"{"b":{"b":1,"a":2},"a":[{"y":1,"x":2}],"B":0,"\u00e4":1,"z":2,"aa":3,"a b":4,"a\u0000":5}"#,
];

fn canonical_fixtures() {
    let cases: Vec<Value> = CANONICAL_INPUTS
        .iter()
        .map(|text| {
            let value: Value = serde_json::from_str(text).expect("valid JSON");
            let canonical = canonical(&value);
            json!({ "input": text, "canonical": canonical, "sha256": sha256(&canonical) })
        })
        .collect();
    write("canonical.json", &json!({ "cases": cases }));
}

/// How serde_json prints f64 (and parses number text), over edge cases and
/// a deterministic pseudo-random spread of bit patterns.
fn float_fixtures() {
    let mut values: Vec<f64> = vec![
        0.0,
        -0.0,
        f64::MIN_POSITIVE,
        f64::MAX,
        f64::MIN,
        f64::EPSILON,
        5e-324,
        1.0 / 3.0,
        2.0 / 3.0,
    ];
    for exp in -30..=30 {
        for mantissa in [1.0, 1.5, 9.999, 1.234_567_890_123_456_7] {
            values.push(mantissa * 10f64.powi(exp));
            values.push(-mantissa * 10f64.powi(exp));
        }
    }
    let mut x: u64 = 0x9E37_79B9_7F4A_7C15;
    while values.len() < 4000 {
        x ^= x << 13;
        x ^= x >> 7;
        x ^= x << 17;
        let f = f64::from_bits(x);
        if f.is_finite() {
            values.push(f);
        }
    }
    let printed: Vec<Value> = values
        .iter()
        .map(|f| json!({ "bits": f.to_bits().to_string(), "text": canonical(&json!(f)) }))
        .collect();
    let texts = [
        "0",
        "-0",
        "1",
        "-1",
        "1.0",
        "1e2",
        "1E2",
        "1e-2",
        "0.1",
        "1.00000000000000011102230246251565404236316680908203125",
        "18446744073709551615",
        "18446744073709551616",
        "-9223372036854775808",
        "-9223372036854775809",
        "123456789012345678901234567890",
        "2.2250738585072011e-308",
        "4.9406564584124654e-324",
        "1.7976931348623157e308",
    ];
    let parsed: Vec<Value> = texts
        .iter()
        .map(|t| {
            let v: Value = serde_json::from_str(t).expect("number");
            json!({ "input": t, "canonical": canonical(&v) })
        })
        .collect();
    write("floats.json", &json!({ "printed": printed, "parsed": parsed }));
}

fn secret_fixtures() {
    let keys: [(u32, [u8; 32]); 2] = [
        (1, [7u8; 32]),
        (2, core::array::from_fn(|i| u8::try_from(i).expect("byte"))),
    ];
    let keyring = Keyring::from_keys(keys);
    let who = Identity {
        org: "0192f3a1-0000-7000-8000-000000000001",
        secret: "0192f3a1-0000-7000-8000-000000000002",
        revision: 3,
    };
    let values = BTreeMap::from([
        ("DATABASE_URL".to_owned(), "postgres://u:p@db/app".to_owned()),
        ("EMPTY".to_owned(), String::new()),
        (
            "UNICODE".to_owned(),
            "\u{633}\u{644}\u{627}\u{645} \u{2713} <&>".to_owned(),
        ),
    ]);
    let sealed = keyring.seal_values(who, &values).expect("seal");
    let fingerprints: Vec<Value> = keyring
        .fingerprints()
        .into_iter()
        .map(|(v, f)| json!({ "version": v, "sha256": STANDARD.encode(f) }))
        .collect();
    let key_list: Vec<Value> = keys
        .iter()
        .map(|(v, k)| json!({ "version": v, "key": STANDARD.encode(k) }))
        .collect();
    write(
        "secrets.json",
        &json!({
            "keys": key_list,
            "identity": { "org": who.org, "secret": who.secret, "revision": who.revision },
            "values": values,
            "sealed": {
                "ciphertext": STANDARD.encode(&sealed.ciphertext),
                "wrappedKey": STANDARD.encode(&sealed.wrapped_key),
                "keyVersion": sealed.key_version,
            },
            "fingerprints": fingerprints,
        }),
    );
}

fn write(name: &str, value: &Value) {
    let text = serde_json::to_string_pretty(value).expect("json") + "\n";
    std::fs::write(out(name), text).expect("write fixture");
}

#[test]
#[ignore = "writes fixtures for the Go port"]
fn write_compat_fixtures() {
    canonical_fixtures();
    float_fixtures();
    secret_fixtures();
}
