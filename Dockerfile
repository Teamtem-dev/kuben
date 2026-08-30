# syntax=docker/dockerfile:1.7
# Build Kuben from source into a distroless image. Compilation runs on the
# build platform and cross-compiles to static musl with zig, so arm64 images
# are not built under QEMU.
#   docker buildx build --platform linux/amd64,linux/arm64 -t kuben:dev .
# Releases use deploy/release.Dockerfile with the prebuilt, checksummed binaries.

FROM --platform=$BUILDPLATFORM node:22-alpine AS web
WORKDIR /src
RUN corepack enable
COPY package.json pnpm-lock.yaml pnpm-workspace.yaml .npmrc ./
COPY apps/web/package.json apps/web/
COPY packages/api-client/package.json packages/api-client/
RUN --mount=type=cache,id=pnpm-store,target=/root/.local/share/pnpm/store pnpm install --frozen-lockfile
COPY tsconfig.base.json ./
COPY packages/api-client packages/api-client
COPY apps/web apps/web
RUN pnpm -F @kuben/web build
