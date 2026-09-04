# syntax=docker/dockerfile:1.7
# Release image: packages the static musl binaries built by release.yml.
# No compiler and no QEMU — each platform is a single COPY, so the multi-arch
# build takes seconds and ships exactly the bytes that were checksummed.
FROM gcr.io/distroless/static:nonroot
ARG TARGETOS
ARG TARGETARCH
COPY --chmod=0755 image/${TARGETOS}-${TARGETARCH}/kuben /kuben
# Numeric UID so Kubernetes `runAsNonRoot: true` can verify it.
USER 65532:65532
EXPOSE 8080 9090
ENTRYPOINT ["/kuben"]
CMD ["serve", "--roles=all"]
