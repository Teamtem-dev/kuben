//! Prints every Kuben CRD as a multi-document YAML stream.
//! Usage: `cargo run -p kuben-crd --bin crdgen > charts/kuben/crds/kuben.dev.yaml`

fn main() {
    let mut out = String::new();
    for crd in kuben_crd::all_crds() {
