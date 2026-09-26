# syntax=docker/dockerfile:1.7
# Build Kuben from source into a distroless image. Go cross-compiles on the
# build platform (CGO_ENABLED=0, static), so arm64 images are not built under
# QEMU.
# The context is the repository root:
#   docker buildx build -f deploy/Dockerfile --platform linux/amd64,linux/arm64 -t kuben:dev .
#   docker buildx build -f deploy/Dockerfile --build-arg VERSION=2.0.0-dev ...
# Releases use deploy/release.Dockerfile with the prebuilt, checksummed binaries.

FROM --platform=$BUILDPLATFORM oven/bun:1.4.2-alpine AS web
WORKDIR /src
ENV BUN_INSTALL_CACHE_DIR=/cache/bun
# Manifests first, so the install layer is reused until a dependency changes.
COPY package.json bun.lock bunfig.toml ./
COPY apps/console/package.json apps/console/
COPY packages/api-client/package.json packages/api-client/
COPY packages/typescript-config/package.json packages/typescript-config/
RUN --mount=type=cache,id=bun-install,target=/cache/bun bun install --frozen-lockfile
COPY packages/typescript-config packages/typescript-config
COPY packages/api-client packages/api-client
COPY apps/console apps/console
RUN cd apps/console && bun run build

# The Go version follows go.work.
FROM --platform=$BUILDPLATFORM golang:1.27-bookworm AS build
WORKDIR /src
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ENV CGO_ENABLED=0
# The Go workspace (go.work: the hub, the agent and the API module), as
# scripts/go-build.sh and goreleaser build it.
COPY go.work go.work.sum ./
COPY packages/api packages/api
COPY apps/kuben-agent apps/kuben-agent
COPY apps/kuben apps/kuben
# The console is embedded from apps/kuben/internal/httpapi/web/dist
# (scripts/go-build.sh).
COPY --from=web /src/apps/console/dist apps/kuben/internal/httpapi/web/dist
RUN find apps/kuben/internal/httpapi/web/dist -name '*.map' -delete
# The flags of scripts/go-build.sh and .goreleaser.yaml.
RUN --mount=type=cache,id=go-mod,target=/go/pkg/mod \
    --mount=type=cache,id=go-build,target=/root/.cache/go-build \
    cd apps/kuben && GOOS="$TARGETOS" GOARCH="$TARGETARCH" go build -trimpath \
      -ldflags "-s -w -X github.com/Teamtem-dev/kuben/apps/kuben/internal/version.Version=$VERSION" \
      -o /kuben ./cmd/kuben \
 && cd ../kuben-agent && GOOS="$TARGETOS" GOARCH="$TARGETARCH" go build -trimpath \
      -ldflags "-s -w -X main.version=$VERSION" \
      -o /kuben-agent ./cmd/kuben-agent

FROM gcr.io/distroless/static:nonroot
COPY --from=build /kuben /kuben
# The cluster agent (M2.8), started by the chart with `command: [/kuben-agent]`.
COPY --from=build /kuben-agent /kuben-agent
# No database default: Kuben keeps its data in PostgreSQL (ADR-025), given
# at run time as KUBEN_DATABASE__URL (the Helm chart sets it).
USER 65532:65532
EXPOSE 8080 9090
ENTRYPOINT ["/kuben"]
CMD ["serve", "--roles=all"]
