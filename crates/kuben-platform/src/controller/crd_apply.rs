//! Apply Kuben's CRDs with server-side apply at boot (ADR-017). Helm never
//! upgrades `crds/`, so the binary owns its own schema lifecycle.

use k8s_openapi::apiextensions_apiserver::pkg::apis::apiextensions::v1::CustomResourceDefinition;
use kube::{
    Api, Client,
    api::{Patch, PatchParams},
};
use kuben_crd::FIELD_MANAGER;

/// Idempotently apply every CRD. Safe to call on every start.
pub async fn ensure(client: Client) -> anyhow::Result<()> {
    let api = Api::<CustomResourceDefinition>::all(client);
    let params = PatchParams::apply(FIELD_MANAGER).force();
