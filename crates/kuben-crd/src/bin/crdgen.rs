//! Writes every Kuben CRD as a multi-document YAML stream to the path given as
//! the only argument, or to stdout without one. `bun run gen` writes
//! `charts/kuben/crds/kuben.dev_all.yaml`.

use std::{ffi::OsString, fs, io::Write, path::PathBuf};

fn main() -> std::io::Result<()> {
    let mut out = String::new();
    for crd in kuben_crd::all_crds() {
        out.push_str("---\n");
        out.push_str(&serde_yaml_ng::to_string(&crd).expect("CRD serializes to YAML"));
    }
    emit(std::env::args_os().nth(1), &out)
}

/// Write-then-rename, so an interrupted run never leaves a truncated file.
fn emit(path: Option<OsString>, contents: &str) -> std::io::Result<()> {
    let Some(path) = path.map(PathBuf::from) else {
        return std::io::stdout().write_all(contents.as_bytes());
    };
    let tmp = path.with_extension("tmp");
    fs::write(&tmp, contents)?;
    fs::rename(&tmp, &path)
}
