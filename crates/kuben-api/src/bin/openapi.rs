//! Writes the OpenAPI document as pretty JSON to the path given as the only
//! argument, or to stdout without one. `bun run gen` writes
//! `packages/api-client/openapi.json`.

use std::{ffi::OsString, fs, io::Write, path::PathBuf};

fn main() -> std::io::Result<()> {
    let mut json = kuben_api::openapi::spec()
        .to_pretty_json()
        .expect("openapi serializes");
    json.push('\n');
    emit(std::env::args_os().nth(1), &json)
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
