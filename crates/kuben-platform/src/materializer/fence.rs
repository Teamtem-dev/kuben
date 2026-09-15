//! The generation fence (ADR-027) for materialized App objects.
//!
//! A worker writes the App object of its run only while the run's generation
//! is the target's current one, as SQL says after the live object was read;
//! the write carries the `resourceVersion` of that read. A stale worker
//! therefore never lowers the generation of a live object: either SQL already
//! shows the newer generation, or the newer write changed the
//! `resourceVersion` and the stale write fails with 409 and starts over.
//!
//! The generation annotation of the live object is only a report: a value
//! higher than any generation SQL accepted was not written by Kuben.

use kube::api::ObjectMeta;
use kuben_core::ops::Generation;

use super::render::annotations;

/// Whether a run may write its target's App object.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Fence {
    /// Write: the run's generation is the target's current one.
    Write,
    /// Write, replacing an annotation that claims a generation SQL never
    /// accepted.
    Forged { live: u64 },
    /// Do not write: the target is at another generation, owned by a newer run.
    Superseded { current: u64 },
}

impl Fence {
    #[must_use]
    pub const fn may_write(self) -> bool {
        !matches!(self, Self::Superseded { .. })
    }
}

/// Decide for the run of generation `ours`, the target's `current` generation
/// read from SQL after `live` was read.
#[must_use]
pub fn fence(ours: Generation, current: Generation, live: &ObjectMeta) -> Fence {
    if current.0 != ours.0 {
        return Fence::Superseded { current: current.0 };
    }
    match live_generation(live) {
        Some(live) if live > ours.0 => Fence::Forged { live },
        _ => Fence::Write,
    }
}

/// The generation annotation of a live object, when it has a readable one.
#[must_use]
pub fn live_generation(meta: &ObjectMeta) -> Option<u64> {
    meta.annotations
        .as_ref()?
        .get(annotations::GENERATION)?
        .parse()
        .ok()
}

#[cfg(test)]
mod tests {
    use std::collections::BTreeMap;

    use super::*;

    fn annotated(value: &str) -> ObjectMeta {
        ObjectMeta {
            annotations: Some(BTreeMap::from([(
                annotations::GENERATION.to_owned(),
                value.to_owned(),
            )])),
            ..ObjectMeta::default()
        }
    }

    #[test]
    fn only_the_current_generation_writes() {
        let (g2, g3) = (Generation(2), Generation(3));
        assert_eq!(
            fence(g2, g3, &ObjectMeta::default()),
            Fence::Superseded { current: 3 }
        );
        assert!(!fence(g2, g3, &annotated("1")).may_write());
        assert_eq!(fence(g3, g3, &ObjectMeta::default()), Fence::Write, "adoption");
        assert_eq!(fence(g3, g3, &annotated("2")), Fence::Write);
        assert_eq!(fence(g3, g3, &annotated("3")), Fence::Write, "a retried write");
        assert_eq!(fence(g3, g3, &annotated("oops")), Fence::Write);
    }

    #[test]
    fn a_generation_sql_never_accepted_is_forged() {
        let g3 = Generation(3);
        let forged = fence(g3, g3, &annotated("99"));
        assert_eq!(forged, Fence::Forged { live: 99 });
        assert!(forged.may_write());
        assert_eq!(live_generation(&annotated("99")), Some(99));
        assert_eq!(live_generation(&ObjectMeta::default()), None);
    }
}
