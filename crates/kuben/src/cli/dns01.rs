//! `kuben dns01-issuer` (M5.2): a cert-manager ClusterIssuer that answers
//! ACME DNS-01 challenges through Cloudflare, for wildcard and apex
//! certificates, and for hosts behind a proxy or without port 80.
//!
//! The API token goes into a Secret next to cert-manager; the issuer
//! references it. Both are applied server-side under Kuben's field manager,
//! so running the command again updates them.

use std::path::PathBuf;

use anyhow::Context as _;
use k8s_openapi::api::core::v1::Secret;
use kube::{
    Api,
    api::{ApiResource, DynamicObject, GroupVersionKind, Patch, PatchParams},
};
use kuben_core::config::Config;
use kuben_platform::registry::ClusterRegistry;
use serde_json::{Value, json};

const MANAGER: &str = "kuben";
const LETS_ENCRYPT: &str = "https://acme-v02.api.letsencrypt.org/directory";
const LETS_ENCRYPT_STAGING: &str = "https://acme-staging-v02.api.letsencrypt.org/directory";
const TOKEN_KEY: &str = "api-token";

/// Options of `kuben dns01-issuer`.
#[derive(Debug, clap::Args)]
pub struct Dns01Opts {
    /// The ClusterIssuer to create or update.
    #[arg(long, default_value = "letsencrypt-dns")]
    pub name: String,
    /// The ACME account's email address.
    #[arg(long)]
    pub email: String,
    /// A file holding a Cloudflare API token with Zone:DNS:Edit on the zones.
    #[arg(long)]
    pub token_file: PathBuf,
    /// The namespace cert-manager runs in (the token Secret goes there).
    #[arg(long, default_value = "cert-manager")]
    pub namespace: String,
    /// Use Let's Encrypt's staging server (untrusted certificates).
    #[arg(long)]
    pub staging: bool,
    /// Print the objects instead of applying them (the token is redacted).
    #[arg(long)]
    pub dry_run: bool,
}

/// The token Secret and the ClusterIssuer of `opts`.
#[must_use]
pub fn manifests(opts: &Dns01Opts, token: &str) -> (Value, Value) {
    let secret_name = format!("{}-cloudflare", opts.name);
    let labels = json!({ "app.kubernetes.io/managed-by": MANAGER });
    let secret = json!({
        "apiVersion": "v1",
        "kind": "Secret",
        "metadata": { "name": secret_name, "namespace": opts.namespace, "labels": labels },
        "type": "Opaque",
        "stringData": { TOKEN_KEY: token },
    });
    let issuer = json!({
        "apiVersion": "cert-manager.io/v1",
        "kind": "ClusterIssuer",
        "metadata": { "name": opts.name, "labels": labels },
        "spec": { "acme": {
            "email": opts.email,
            "server": if opts.staging { LETS_ENCRYPT_STAGING } else { LETS_ENCRYPT },
            "privateKeySecretRef": { "name": format!("{}-account", opts.name) },
            "solvers": [{ "dns01": { "cloudflare": {
                "apiTokenSecretRef": { "name": secret_name, "key": TOKEN_KEY },
            } } }],
        } },
    });
    (secret, issuer)
}

fn check(opts: &Dns01Opts) -> anyhow::Result<String> {
    anyhow::ensure!(
        opts.email.contains('@') && !opts.email.contains(char::is_whitespace),
        "--email must be an email address"
    );
    let token = std::fs::read_to_string(&opts.token_file)
        .with_context(|| format!("reading {}", opts.token_file.display()))?;
    let token = token.trim().to_owned();
    anyhow::ensure!(!token.is_empty(), "{} is empty", opts.token_file.display());
    Ok(token)
}

/// `kuben dns01-issuer`.
pub async fn run(cfg: Config, opts: Dns01Opts) -> anyhow::Result<()> {
    let token = check(&opts)?;
    if opts.dry_run {
        let (mut secret, issuer) = manifests(&opts, "<redacted>");
        secret["stringData"][TOKEN_KEY] = json!("<redacted>");
        println!("{}", serde_yaml_ng::to_string(&secret)?);
        println!("---\n{}", serde_yaml_ng::to_string(&issuer)?);
        return Ok(());
    }
    let client = ClusterRegistry::connect(&cfg.kube).await?.primary();
    let (secret, issuer) = manifests(&opts, &token);
    let params = PatchParams::apply(MANAGER).force();
    let secret: Secret = serde_json::from_value(secret)?;
    Api::<Secret>::namespaced(client.clone(), &opts.namespace)
        .patch(
            &format!("{}-cloudflare", opts.name),
            &params,
            &Patch::Apply(&secret),
        )
        .await
        .with_context(|| format!("writing the token Secret in {}", opts.namespace))?;
    let gvk = GroupVersionKind::gvk("cert-manager.io", "v1", "ClusterIssuer");
    let resource = ApiResource::from_gvk_with_plural(&gvk, "clusterissuers");
    let issuer: DynamicObject = serde_json::from_value(issuer)?;
    Api::<DynamicObject>::all_with(client, &resource)
        .patch(&opts.name, &params, &Patch::Apply(&issuer))
        .await
        .context("writing the ClusterIssuer (is cert-manager installed?)")?;
    println!(
        "ClusterIssuer {} is set up for DNS-01 through Cloudflare. Use it with \
         `kubectl patch kubenconfig kuben --type merge -p '{{\"spec\":{{\"clusterIssuer\":\"{}\"}}}}'`; \
         `kubectl get clusterissuer {}` shows when it is ready.",
        opts.name, opts.name, opts.name
    );
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    fn opts(staging: bool) -> Dns01Opts {
        Dns01Opts {
            name: "letsencrypt-dns".into(),
            email: "ops@example.com".into(),
            token_file: PathBuf::from("/nonexistent"),
            namespace: "cert-manager".into(),
            staging,
            dry_run: false,
        }
    }

    #[test]
    fn issuers_answer_dns01_through_cloudflare() {
        let (secret, issuer) = manifests(&opts(false), "tok");
        assert_eq!(secret["metadata"]["name"], "letsencrypt-dns-cloudflare");
        assert_eq!(secret["metadata"]["namespace"], "cert-manager");
        assert_eq!(secret["stringData"]["api-token"], "tok");
        let acme = &issuer["spec"]["acme"];
        assert_eq!(acme["server"], LETS_ENCRYPT);
        assert_eq!(
            acme["solvers"][0]["dns01"]["cloudflare"]["apiTokenSecretRef"],
            json!({ "name": "letsencrypt-dns-cloudflare", "key": "api-token" })
        );
        let (_, staging) = manifests(&opts(true), "tok");
        assert_eq!(staging["spec"]["acme"]["server"], LETS_ENCRYPT_STAGING);
        let issuer: DynamicObject = serde_json::from_value(issuer).expect("object");
        assert_eq!(issuer.metadata.name.as_deref(), Some("letsencrypt-dns"));
    }

    #[test]
    fn bad_input_is_refused() {
        let mut o = opts(false);
        o.email = "nobody".into();
        assert!(check(&o).is_err());
        let o = opts(false);
        assert!(check(&o).is_err(), "missing token file");
    }
}
