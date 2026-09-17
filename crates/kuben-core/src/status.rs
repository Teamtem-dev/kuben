//! The public state of a service (M5.3): what a status page says about an
//! app, from what Kuben observed, and nothing more.

use serde::Serialize;

/// A component's or a page's state, from best to worst.
#[derive(Clone, Copy, Debug, PartialEq, Eq, PartialOrd, Ord, Serialize)]
#[serde(rename_all = "camelCase")]
pub enum ServiceStatus {
    Operational,
    Degraded,
    MajorOutage,
}

/// What Kuben observed of one app.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub struct Observed {
    /// The app reports itself ready (`None`: no report yet).
    pub ready: Option<bool>,
    pub pods: u32,
    pub ready_pods: u32,
    /// An open critical incident concerns the app.
    pub critical_incident: bool,
    /// An open warning incident concerns the app.
    pub warning_incident: bool,
}

/// The public state of an app. Nothing observed is never "operational".
#[must_use]
pub fn component_status(o: Observed) -> ServiceStatus {
    if o.pods > 0 && o.ready_pods == 0 {
        return ServiceStatus::MajorOutage;
    }
    match o.ready {
        Some(true) if o.ready_pods >= o.pods && !o.critical_incident && !o.warning_incident => {
            ServiceStatus::Operational
        }
        Some(false) if o.pods == 0 => ServiceStatus::MajorOutage,
        _ => ServiceStatus::Degraded,
    }
}

/// The state of a page: its worst component; operational without any.
#[must_use]
pub fn page_status(components: &[ServiceStatus]) -> ServiceStatus {
    components
        .iter()
        .copied()
        .max()
        .unwrap_or(ServiceStatus::Operational)
}

#[cfg(test)]
mod tests {
    use super::*;

    const READY: Observed = Observed {
        ready: Some(true),
        pods: 2,
        ready_pods: 2,
        critical_incident: false,
        warning_incident: false,
    };

    #[test]
    fn components_are_judged_by_what_was_observed() {
        assert_eq!(component_status(READY), ServiceStatus::Operational);
        let half = Observed {
            ready_pods: 1,
            ..READY
        };
        assert_eq!(component_status(half), ServiceStatus::Degraded);
        let down = Observed {
            ready: Some(false),
            ready_pods: 0,
            ..READY
        };
        assert_eq!(component_status(down), ServiceStatus::MajorOutage);
        let gone = Observed {
            ready: Some(false),
            pods: 0,
            ready_pods: 0,
            ..READY
        };
        assert_eq!(component_status(gone), ServiceStatus::MajorOutage);
        let failing = Observed {
            critical_incident: true,
            ..READY
        };
        assert_eq!(component_status(failing), ServiceStatus::Degraded);
        assert_eq!(
            component_status(Observed::default()),
            ServiceStatus::Degraded,
            "nothing observed"
        );
    }

    #[test]
    fn a_page_is_its_worst_component() {
        use ServiceStatus::{Degraded, MajorOutage, Operational};
        assert_eq!(page_status(&[]), Operational);
        assert_eq!(page_status(&[Operational, Degraded]), Degraded);
        assert_eq!(page_status(&[MajorOutage, Degraded]), MajorOutage);
    }
}
