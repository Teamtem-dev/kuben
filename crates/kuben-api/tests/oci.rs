//! Image tags resolved against real registries (ADR-032, option A).
//!
//! Needs the network: the tests are ignored by default and run with
//! `cargo test -- --ignored` in the kind job of CI.

use kuben_api::oci::{ImageResolver, RegistryResolver, ResolveError};

#[tokio::test]
#[ignore = "needs the network; run with --ignored"]
async fn docker_hub_tags_resolve_to_digests() {
    let registry = RegistryResolver::new();
    let busybox = registry.resolve("busybox:1.36").await.expect("busybox");
    assert_eq!(busybox.repository, "docker.io/library/busybox");
    assert!(busybox.digest.as_str().starts_with("sha256:"));
    assert_eq!(busybox.given, "busybox:1.36");

    let nginx = registry
        .resolve("nginxinc/nginx-unprivileged:1.27-alpine")
        .await
        .expect("nginx");
    assert_eq!(nginx.repository, "docker.io/nginxinc/nginx-unprivileged");

    assert!(
        matches!(
            registry.resolve("busybox:no-such-tag-for-kuben").await,
            Err(ResolveError::NotFound(_))
        ),
        "an unknown tag"
    );
}

#[tokio::test]
#[ignore = "needs the network; run with --ignored"]
async fn ghcr_tags_resolve_with_an_anonymous_token() {
    let podinfo = RegistryResolver::new()
        .resolve("ghcr.io/stefanprodan/podinfo:6.7.0")
        .await
        .expect("podinfo");
    assert_eq!(podinfo.repository, "ghcr.io/stefanprodan/podinfo");
    assert!(podinfo.digest.as_str().starts_with("sha256:"));
}
