# syntax=docker/dockerfile:1.7
# Release image: packages the static musl binaries built by release.yml.
# No compiler and no QEMU — each platform is a single COPY, so the multi-arch
# build takes seconds and ships exactly the bytes that were checksummed.
