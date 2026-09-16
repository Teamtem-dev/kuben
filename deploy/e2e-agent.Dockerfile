# The cluster agent for the end-to-end test (M2.8): the runner's debug build,
# in a base with the same C library, loaded into kind. The build context is a
# directory holding only that binary (see the kind job in ci.yml); releases
# ship the static binary in deploy/release.Dockerfile instead.
FROM ubuntu:24.04
COPY --chmod=0755 kuben-agent /kuben-agent
USER 65532:65532
ENTRYPOINT ["/kuben-agent"]
