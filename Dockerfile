# syntax=docker/dockerfile:1.7
# Build Kuben from source into a distroless image. Compilation runs on the
# build platform and cross-compiles to static musl with zig, so arm64 images
# are not built under QEMU.
#   docker buildx build --platform linux/amd64,linux/arm64 -t kuben:dev .
# Releases use deploy/release.Dockerfile with the prebuilt, checksummed binaries.

FROM --platform=$BUILDPLATFORM oven/bun:1.4.2-alpine AS web
WORKDIR /src
ENV BUN_INSTALL_CACHE_DIR=/cache/bun
# Manifests first, so the install layer is reused until a dependency changes.
COPY package.json bun.lock bunfig.toml ./
COPY apps/console/package.json apps/console/
COPY packages/api-client/package.json packages/api-client/
RUN --mount=type=cache,id=bun-install,target=/cache/bun bun install --frozen-lockfile
COPY tsconfig.base.json ./
COPY packages/api-client packages/api-client
COPY apps/console apps/console
RUN cd apps/console && bun run build

FROM --platform=$BUILDPLATFORM rust:1-bookworm AS chef
WORKDIR /src
# The toolchain file decides the toolchain, so add targets after copying it.
COPY rust-toolchain.toml ./
RUN apt-get update \
 && apt-get install -y --no-install-recommends python3-pip \
 && rm -rf /var/lib/apt/lists/* \
 && pip3 install --no-cache-dir --break-system-packages ziglang \
 && rustup target add x86_64-unknown-linux-musl aarch64-unknown-linux-musl \
 && cargo install --locked cargo-chef cargo-zigbuild

FROM chef AS plan
COPY . .
RUN cargo chef prepare --recipe-path recipe.json

FROM chef AS build
ARG TARGETARCH
RUN case "$TARGETARCH" in \
      amd64) echo x86_64-unknown-linux-musl ;; \
      arm64) echo aarch64-unknown-linux-musl ;; \
      *) echo "unsupported TARGETARCH: $TARGETARCH" >&2; exit 1 ;; \
    esac >/target
COPY --from=plan /src/recipe.json .
RUN --mount=type=cache,id=cargo-registry,target=/usr/local/cargo/registry \
    cargo chef cook --release --zigbuild --target "$(cat /target)" --recipe-path recipe.json
COPY . .
COPY --from=web /src/apps/console/dist apps/console/dist
RUN --mount=type=cache,id=cargo-registry,target=/usr/local/cargo/registry \
    cargo zigbuild -p kuben --release --locked --features embed-ui --target "$(cat /target)" \
 && cp "target/$(cat /target)/release/kuben" /kuben

FROM gcr.io/distroless/static:nonroot
COPY --from=build /kuben /kuben
USER 65532:65532
EXPOSE 8080 9090
ENTRYPOINT ["/kuben"]
CMD ["serve", "--roles=all"]
