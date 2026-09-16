//! `kuben setup --plan`: what a run would do, from the same read-only checks
//! the steps make, and nothing changed (plan §11.2: the plan comes before any
//! mutation). Resources a run would create are Kuben's; what exists already
//! keeps its recorded owner, or stays someone else's.

use std::path::Path;

use super::{
    BIN, CONFIG_FILE, DEFAULT_PORT, DataHome, K3S_KUBECONFIG, K3S_MARKER, STATE_DIR, SetupOpts, UNIT_FILE,
    USER, active_firewall, config_port, data_home, existing_kubeconfig, is_root, journal::Journal,
    packages_of, port_free, postgres_installed, psql_as_postgres, service_active, user_ids, written_by_setup,
};
use crate::cli::ui::Ui;

/// One line of the plan: what, and what a run would do about it.
#[derive(Debug, PartialEq, Eq)]
pub(super) struct Line {
    pub what: &'static str,
    pub action: String,
}

fn line(what: &'static str, action: impl Into<String>) -> Line {
    Line {
        what,
        action: action.into(),
    }
}

pub(super) fn show(ui: Ui, opts: &SetupOpts) {
    if !cfg!(target_os = "linux") {
        ui.note("`kuben setup` sets up a Linux server with systemd; nothing to plan on this machine.");
        return;
    }
    let journal = Journal::peek(&Path::new(STATE_DIR).join(super::journal::JOURNAL)).unwrap_or_default();
    ui.heading("kuben setup would:");
    for Line { what, action } in lines(opts, &journal) {
        ui.done(what, action);
    }
    if !is_root() {
        ui.note("Run as root for the checks that need it (PostgreSQL roles, firewall).");
    }
    if !journal.resources.is_empty() {
        ui.heading("Recorded owners (kuben uninstall --purge removes only Kuben's):");
        for r in &journal.resources {
            ui.done(&format!("{:?} {}", r.kind, r.name), format!("{:?}", r.owner));
        }
    }
    ui.note("Nothing was changed. Run `kuben setup` without --plan to apply it.");
}

fn lines(opts: &SetupOpts, journal: &Journal) -> Vec<Line> {
    let version = format!("v{}", crate::cli::VERSION);
    let mut plan = vec![
        line(
            "Binary",
            if Path::new(BIN).exists() {
                format!("replace {BIN} with {version} when it differs")
            } else {
                format!("install {version} to {BIN}")
            },
        ),
        line(
            "System user",
            if user_ids(USER).is_some() {
                format!("use the existing user {USER}")
            } else {
                format!("create the system user {USER} (home {STATE_DIR})")
            },
        ),
        cluster(opts),
    ];
    let configured = std::fs::read_to_string(CONFIG_FILE).ok();
    plan.push(database(configured.as_deref()));
    let port = opts
        .port
        .or_else(|| configured.as_deref().and_then(config_port))
        .unwrap_or(DEFAULT_PORT);
    plan.push(line(
        "Configuration",
        if configured.is_some() {
            format!("keep {CONFIG_FILE}")
        } else {
            format!("write {CONFIG_FILE} (PostgreSQL on this server, the kubeconfig copy)")
        },
    ));
    plan.push(line(
        "Port",
        if service_active() || port_free(port) {
            format!("{port}")
        } else {
            format!("{port} is in use: the next free port, or ask")
        },
    ));
    plan.push(line(
        "Service",
        match std::fs::read_to_string(UNIT_FILE) {
            Err(_) => "install and start kuben.service".to_owned(),
            Ok(unit) if !written_by_setup(&unit) => {
                format!("stop: {UNIT_FILE} was not written by kuben setup")
            }
            Ok(_) => "restart kuben.service only if its binary, unit or configuration changes".to_owned(),
        },
    ));
    plan.push(line(
        "Firewall",
        match active_firewall() {
            Some(firewall) if firewall.is_open(port) => format!("{} already open", firewall.rule(port)),
            Some(firewall) => format!("open {}", firewall.rule(port)),
            None => "nothing (no host firewall is active)".to_owned(),
        },
    ));
    if journal.unfinished().is_some() {
        plan.push(line("Last run", "did not finish: every step is checked again"));
    }
    plan
}

fn cluster(opts: &SetupOpts) -> Line {
    let action = if let Some(path) = &opts.kubeconfig {
        format!("use the cluster in {}", path.display())
    } else if Path::new(K3S_KUBECONFIG).exists() {
        if Path::new(K3S_MARKER).exists() {
            "use k3s (installed by kuben setup)".to_owned()
        } else {
            "use the k3s already installed (never removed by uninstall)".to_owned()
        }
    } else if let Some(existing) = existing_kubeconfig() {
        format!("use the cluster in {}", existing.display())
    } else if opts.no_k3s {
        "stop: no cluster found and --no-k3s given".to_owned()
    } else {
        "install k3s".to_owned()
    };
    line("Cluster", action)
}

fn database(configured: Option<&str>) -> Line {
    let action = match data_home(configured) {
        DataHome::External => format!("use the database in {CONFIG_FILE}"),
        DataHome::Sqlite => format!("stop: {CONFIG_FILE} keeps Kuben 1.x data in SQLite"),
        DataHome::Local if !postgres_installed() => {
            let os_release = std::fs::read_to_string("/etc/os-release").unwrap_or_default();
            match packages_of(&os_release) {
                Some(packages) => format!(
                    "install PostgreSQL ({}), then create the role and database kuben",
                    packages.describe()
                ),
                None => "stop: install PostgreSQL 14 or newer first".to_owned(),
            }
        }
        DataHome::Local if !is_root() => {
            "start PostgreSQL; create the role and database kuben if missing".to_owned()
        }
        DataHome::Local => {
            let exists = |sql: &str| psql_as_postgres(sql).is_ok_and(|out| out.trim() == "1");
            let role = exists("SELECT 1 FROM pg_roles WHERE rolname = 'kuben'");
            let db = exists("SELECT 1 FROM pg_database WHERE datname = 'kuben'");
            match (role, db) {
                (true, true) => {
                    "use the role and database kuben (kept by uninstall unless setup made them)".to_owned()
                }
                (false, false) => "create the role and database kuben".to_owned(),
                (true, false) => "create the database kuben for the existing role".to_owned(),
                (false, true) => "create the role kuben for the existing database".to_owned(),
            }
        }
    };
    line("PostgreSQL", action)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn a_plan_names_every_step_and_changes_nothing() {
        let opts = SetupOpts {
            port: Some(3999),
            kubeconfig: Some("/nonexistent/kubeconfig".into()),
            no_k3s: false,
            bind_local: false,
            yes: true,
            plan: true,
        };
        let plan = lines(&opts, &Journal::default());
        let whats: Vec<&str> = plan.iter().map(|l| l.what).collect();
        assert_eq!(
            whats,
            [
                "Binary",
                "System user",
                "Cluster",
                "PostgreSQL",
                "Configuration",
                "Port",
                "Service",
                "Firewall"
            ]
        );
        assert_eq!(plan[2].action, "use the cluster in /nonexistent/kubeconfig");
        assert!(plan[5].action.starts_with("3999"), "{}", plan[5].action);
    }
}
