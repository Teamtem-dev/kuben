# Kuben task runner. `just` lists the recipes by group; CI runs the same commands.
set shell := ["bash", "-euo", "pipefail", "-c"]

export CARGO_TERM_COLOR := "always"

# List all recipes, grouped
default:
    @just --list --unsorted

# One-time setup: Rust toolchain (rust-toolchain.toml) and JS dependencies
[group('setup')]
setup:
