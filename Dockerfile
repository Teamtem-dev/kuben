# syntax=docker/dockerfile:1.7
# Build Kuben from source into a distroless image. Compilation runs on the
# build platform and cross-compiles to static musl with zig, so arm64 images
# are not built under QEMU.
#   docker buildx build --platform linux/amd64,linux/arm64 -t kuben:dev .
# Releases use deploy/release.Dockerfile with the prebuilt, checksummed binaries.

FROM --platform=$BUILDPLATFORM node:22-alpine AS web
WORKDIR /src
