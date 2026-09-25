#!/usr/bin/env bash
# Static checks for the Go module in the working directory: what stands in
# for the guarantees the Rust compiler gave (go/CONVENTIONS.md). CI also runs
# golangci-lint with .golangci.yml; this script needs nothing but `go`.
set -euo pipefail
unformatted=$(go tool gofumpt -l .)
if [[ -n $unformatted ]]; then
  echo "not gofumpt-formatted:"; echo "$unformatted"; exit 1
fi
go vet ./...
go tool exhaustive -default-signifies-exhaustive=false -ignore-enum-types '^(reflect\.Kind|k8s\.io/api/core/v1\.ResourceName)$' ./...
go tool go-check-sumtype -default-signifies-exhaustive=false ./...
# NilAway checks Kuben's own packages only: inferring through client-go and
# the other dependencies takes more than 13 GiB (the analysis is killed on
# a 16 GiB machine), and their nilness is not ours to fix. The generated
# API server is excluded as generated code is.
go tool nilaway -test=true \
  -include-pkgs github.com/Teamtem-dev/kuben \
  -exclude-pkgs github.com/Teamtem-dev/kuben/go/hub/internal/api/gen \
  -exclude-file-docstrings "Code generated" ./...
