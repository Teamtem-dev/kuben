//! Quota and scheduling admission (M4.5; plan §11, §16).
//!
//! A deployment is admitted on requested resources, never on current usage:
//! the peak an app may request (every process at its maximum replicas, plus
//! the one surge pod of a rolling update) plus what the other apps of the
//! environment may request must fit the environment's quota, and the whole
//! organization's must fit its quota. A pod larger than the largest
//! schedulable node will not run; whether it fits otherwise is an estimate.
//! A quota is never overridden; an estimate is only a warning.

use std::fmt;

/// Resources a set of pods requests.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub struct Demand {
    pub cpu_millis: u64,
    pub memory_bytes: u64,
    pub pods: u64,
}

impl Demand {
    /// `pods` pods of `cpu_millis` and `memory_bytes` each.
    #[must_use]
    pub const fn pods(count: u64, cpu_millis: u64, memory_bytes: u64) -> Self {
        Self {
            cpu_millis: cpu_millis.saturating_mul(count),
            memory_bytes: memory_bytes.saturating_mul(count),
            pods: count,
        }
    }

    #[must_use]
    pub const fn plus(self, other: Self) -> Self {
        Self {
            cpu_millis: self.cpu_millis.saturating_add(other.cpu_millis),
            memory_bytes: self.memory_bytes.saturating_add(other.memory_bytes),
            pods: self.pods.saturating_add(other.pods),
        }
    }
}

impl std::iter::Sum for Demand {
    fn sum<I: Iterator<Item = Self>>(iter: I) -> Self {
        iter.fold(Self::default(), Self::plus)
    }
}

/// Upper bounds; `None` is unlimited.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub struct Limits {
    pub cpu_millis: Option<u64>,
    pub memory_bytes: Option<u64>,
    pub pods: Option<u64>,
}

/// What a quota bounds.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Resource {
    Cpu,
    Memory,
    Pods,
}

impl fmt::Display for Resource {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(match self {
            Self::Cpu => "CPU",
            Self::Memory => "memory",
            Self::Pods => "pods",
        })
    }
}

/// A demand beyond a quota.
#[derive(Clone, Copy, Debug, PartialEq, Eq, thiserror::Error)]
#[error("{resource} requests would reach {}, over the quota of {}", show(*.resource, *.requested), show(*.resource, *.limit))]
pub struct Exceeded {
    pub resource: Resource,
    pub requested: u64,
    pub limit: u64,
}

/// `value` of `resource` for people.
#[must_use]
pub fn show(resource: Resource, value: u64) -> String {
    match resource {
        Resource::Cpu if value.is_multiple_of(1000) => format!("{} CPU", value / 1000),
        Resource::Cpu => format!("{value}m CPU"),
        Resource::Memory => {
            const MIB: u64 = 1 << 20;
            if value.is_multiple_of(1 << 30) {
                format!("{}Gi", value >> 30)
            } else {
                format!("{}Mi", value.div_ceil(MIB))
            }
        }
        Resource::Pods => format!("{value} pods"),
    }
}

impl Limits {
    /// Admit `demand` (everything, the new app included) under these limits.
    pub fn admit(&self, demand: Demand) -> Result<(), Exceeded> {
        for (resource, limit, requested) in [
            (Resource::Cpu, self.cpu_millis, demand.cpu_millis),
            (Resource::Memory, self.memory_bytes, demand.memory_bytes),
            (Resource::Pods, self.pods, demand.pods),
        ] {
            if let Some(limit) = limit
                && requested > limit
            {
                return Err(Exceeded {
                    resource,
                    requested,
                    limit,
                });
            }
        }
        Ok(())
    }

    #[must_use]
    pub const fn is_unlimited(&self) -> bool {
        self.cpu_millis.is_none() && self.memory_bytes.is_none() && self.pods.is_none()
    }
}

/// The largest schedulable node, as last discovered.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct NodeCapacity {
    pub cpu_millis: u64,
    pub memory_bytes: u64,
}

/// Whether the pods of a deployment are likely to be scheduled.
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum Estimate {
    FitsEstimate,
    /// A pod requests more than any schedulable node offers: it will not run.
    UnlikelyToSchedule(String),
    /// Not known (no node facts): never shown as safe.
    UnknownConstraints(String),
}

/// Estimate whether a pod of `cpu_millis` and `memory_bytes` fits on
/// `largest`.
#[must_use]
pub fn estimate(largest: Option<NodeCapacity>, cpu_millis: u64, memory_bytes: u64) -> Estimate {
    let Some(node) = largest else {
        return Estimate::UnknownConstraints("the cluster's nodes are not known yet".into());
    };
    if cpu_millis > node.cpu_millis {
        return Estimate::UnlikelyToSchedule(format!(
            "a pod requests {}, the largest node offers {}",
            show(Resource::Cpu, cpu_millis),
            show(Resource::Cpu, node.cpu_millis)
        ));
    }
    if memory_bytes > node.memory_bytes {
        return Estimate::UnlikelyToSchedule(format!(
            "a pod requests {} of memory, the largest node offers {}",
            show(Resource::Memory, memory_bytes),
            show(Resource::Memory, node.memory_bytes)
        ));
    }
    Estimate::FitsEstimate
}

/// A Kubernetes CPU quantity (`500m`, `2`, `0.5`) in millicores.
#[must_use]
pub fn cpu_millis(quantity: &str) -> Option<u64> {
    let q = quantity.trim();
    if let Some(millis) = q.strip_suffix('m') {
        return millis.parse().ok();
    }
    scaled(q, 1000)
}

/// A Kubernetes memory quantity (`512Mi`, `1Gi`, `1G`, `1048576`) in bytes.
#[must_use]
pub fn bytes(quantity: &str) -> Option<u64> {
    const UNITS: [(&str, u64); 12] = [
        ("Ki", 1 << 10),
        ("Mi", 1 << 20),
        ("Gi", 1 << 30),
        ("Ti", 1 << 40),
        ("Pi", 1 << 50),
        ("Ei", 1 << 60),
        ("k", 1_000),
        ("M", 1_000_000),
        ("G", 1_000_000_000),
        ("T", 1_000_000_000_000),
        ("P", 1_000_000_000_000_000),
        ("E", 1_000_000_000_000_000_000),
    ];
    let q = quantity.trim();
    for (suffix, factor) in UNITS {
        if let Some(number) = q.strip_suffix(suffix) {
            return scaled(number, factor);
        }
    }
    scaled(q, 1)
}

/// `number` (an integer or a decimal) times `factor`, rounded up.
fn scaled(number: &str, factor: u64) -> Option<u64> {
    let (whole, fraction) = number.split_once('.').unwrap_or((number, ""));
    if whole.is_empty() && fraction.is_empty() {
        return None;
    }
    let digits = |s: &str| s.bytes().all(|b| b.is_ascii_digit());
    if !digits(whole) || !digits(fraction) || fraction.len() > 9 {
        return None;
    }
    let whole: u64 = if whole.is_empty() { 0 } else { whole.parse().ok()? };
    let mut out = whole.checked_mul(factor)?;
    if !fraction.is_empty() {
        let scale = 10u64.pow(u32::try_from(fraction.len()).ok()?);
        let part: u64 = fraction.parse().ok()?;
        let extra = u128::from(part) * u128::from(factor);
        out = out.checked_add(u64::try_from(extra.div_ceil(u128::from(scale))).ok()?)?;
    }
    Some(out)
}

#[cfg(test)]
mod tests {
    use proptest::prelude::*;

    use super::*;

    #[test]
    fn quantities_parse_like_kubernetes() {
        for (q, millis) in [
            ("500m", 500),
            ("2", 2000),
            ("0.5", 500),
            ("1.25", 1250),
            (".1", 100),
        ] {
            assert_eq!(cpu_millis(q), Some(millis), "{q}");
        }
        for q in ["", "m", "abc", "1.2.3", "-1", "1x"] {
            assert_eq!(cpu_millis(q), None, "{q}");
        }
        for (q, b) in [
            ("512Mi", 512 << 20),
            ("1Gi", 1 << 30),
            ("1.5Gi", 3 << 29),
            ("1G", 1_000_000_000),
            ("100k", 100_000),
            ("1048576", 1 << 20),
        ] {
            assert_eq!(bytes(q), Some(b), "{q}");
        }
        for q in ["", "Mi", "1Zi", "one", "1.0000000001Gi"] {
            assert_eq!(bytes(q), None, "{q}");
        }
        assert_eq!(bytes("99999999999Ei"), None, "overflow");
    }

    #[test]
    fn quotas_bound_every_resource() {
        let limits = Limits {
            cpu_millis: Some(2000),
            memory_bytes: Some(4 << 30),
            pods: None,
        };
        let fits = Demand::pods(4, 500, 1 << 30);
        assert_eq!(limits.admit(fits), Ok(()));
        let too_much = fits.plus(Demand::pods(1, 100, 0));
        let refused = limits.admit(too_much).expect_err("over");
        assert_eq!(
            (refused.resource, refused.requested, refused.limit),
            (Resource::Cpu, 2100, 2000)
        );
        assert_eq!(
            refused.to_string(),
            "CPU requests would reach 2100m CPU, over the quota of 2 CPU"
        );
        let memory = limits.admit(Demand::pods(5, 0, 1 << 30)).expect_err("over");
        assert_eq!(
            memory.to_string(),
            "memory requests would reach 5Gi, over the quota of 4Gi"
        );
        let pods = Limits {
            pods: Some(3),
            ..Limits::default()
        };
        assert_eq!(
            pods.admit(Demand::pods(4, 0, 0)).map_err(|e| e.resource),
            Err(Resource::Pods)
        );
        assert!(Limits::default().is_unlimited());
        assert_eq!(
            Limits::default().admit(Demand::pods(u64::MAX, u64::MAX, u64::MAX)),
            Ok(())
        );
    }

    #[test]
    fn pods_larger_than_any_node_do_not_schedule() {
        let node = NodeCapacity {
            cpu_millis: 4000,
            memory_bytes: 8 << 30,
        };
        assert_eq!(estimate(Some(node), 4000, 8 << 30), Estimate::FitsEstimate);
        assert!(matches!(
            estimate(Some(node), 4001, 0),
            Estimate::UnlikelyToSchedule(_)
        ));
        assert!(matches!(
            estimate(Some(node), 1, (8 << 30) + 1),
            Estimate::UnlikelyToSchedule(why) if why.contains("memory")
        ));
        assert!(matches!(estimate(None, 1, 1), Estimate::UnknownConstraints(_)));
    }

    proptest! {
        #[test]
        fn demands_add_up_without_overflow(a in any::<u64>(), b in any::<u64>(), n in 0u64..1000) {
            let d = Demand::pods(n, a, b);
            let sum: Demand = [d, d].into_iter().sum();
            prop_assert!(sum.cpu_millis >= d.cpu_millis);
            prop_assert_eq!(sum.pods, n * 2);
        }

        #[test]
        fn millicores_round_trip(m in 0u64..10_000_000) {
            prop_assert_eq!(cpu_millis(&format!("{m}m")), Some(m));
            prop_assert_eq!(cpu_millis(&format!("{}", m * 1000)), Some(m * 1_000_000));
        }
    }
}
