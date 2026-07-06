//! Prints the OpenAPI document as pretty JSON.
//! Usage: `cargo run -p kuben-api --bin openapi > packages/api-client/openapi.json`

fn main() {
    let spec = kuben_api::openapi::spec();
    println!("{}", spec.to_pretty_json().expect("openapi serializes"));
}
