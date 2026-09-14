//! Conditional writes (ADR-027): every write carries the `resourceVersion`
//! of the read it was decided on, or is a create that fails when the object
//! appeared meanwhile. A 409 means "read again and decide again".
//!
//! Objects are written with server-side apply as [`FIELD_MANAGER`]. Fields
//! other managers own and the materializer does not render stay theirs.

use std::fmt::Debug;

use kube::{
    Api, Resource,
    api::{ObjectMeta, Patch, PatchParams, PostParams},
};
use kuben_core::ids::OrgId;
use kuben_crd::labels;
use serde::{Serialize, de::DeserializeOwned};

use super::render::FIELD_MANAGER;

/// Conflicting writes tolerated for one object before the attempt is retried
/// later.
pub const MAX_CONFLICTS: usize = 5;

#[must_use]
pub fn is_conflict(err: &kube::Error) -> bool {
    matches!(err, kube::Error::Api(status) if status.code == 409)
}

/// The object belongs to `org`: Kuben manages it and labelled it with the
/// organization. An object anyone else created is never adopted.
#[must_use]
pub fn belongs_to(meta: &ObjectMeta, org: OrgId) -> bool {
    let Some(l) = &meta.labels else {
        return false;
    };
    l.get(labels::MANAGED_BY).is_some_and(|v| v == labels::MANAGER)
        && l.get(labels::ORG).is_some_and(|v| *v == org.to_string())
}

/// Write `desired` over `live`, the object as last read: server-side apply
/// carrying its `resourceVersion`, or a create when there was none.
pub async fn put<K>(api: &Api<K>, desired: &K, live: Option<&K>) -> Result<K, kube::Error>
where
    K: Resource + Clone + Serialize + DeserializeOwned + Debug,
{
    let Some(live) = live else {
        let params = PostParams {
            field_manager: Some(FIELD_MANAGER.into()),
            ..PostParams::default()
        };
        return api.create(&params, desired).await;
    };
    let name = desired.meta().name.clone().unwrap_or_default();
    let mut object = desired.clone();
    object
        .meta_mut()
        .resource_version
        .clone_from(&live.meta().resource_version);
    api.patch(
        &name,
        &PatchParams::apply(FIELD_MANAGER).force(),
        &Patch::Apply(&object),
    )
    .await
}

#[cfg(test)]
mod tests {
    use std::collections::BTreeMap;

    use super::*;

    fn labelled(pairs: &[(&str, &str)]) -> ObjectMeta {
        ObjectMeta {
            labels: Some(
                pairs
                    .iter()
                    .map(|(k, v)| ((*k).to_owned(), (*v).to_owned()))
                    .collect::<BTreeMap<_, _>>(),
            ),
            ..ObjectMeta::default()
        }
    }

    #[test]
    fn only_objects_kuben_made_for_the_same_organization_are_taken_over() {
        let org = OrgId::new();
        let ours = labelled(&[
            (labels::MANAGED_BY, labels::MANAGER),
            (labels::ORG, &org.to_string()),
        ]);
        assert!(belongs_to(&ours, org));
        assert!(!belongs_to(&ours, OrgId::new()), "another organization");
        assert!(
            !belongs_to(&labelled(&[(labels::ORG, &org.to_string())]), org),
            "not managed by Kuben"
        );
        assert!(!belongs_to(&ObjectMeta::default(), org));
    }
}
