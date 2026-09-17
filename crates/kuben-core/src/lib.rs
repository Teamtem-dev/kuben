//! Kuben core: domain types, configuration, errors and the traits every other
//! crate builds on. This crate deliberately performs **no IO** so it stays
//! cheap to compile and easy to test.

pub mod artifact;
pub mod authz;
pub mod capacity;
pub mod ci;
pub mod config;
pub mod domain;
pub mod error;
pub mod ids;
pub mod model;
pub mod ops;
pub mod perm;
pub mod policy;
pub mod preview;
pub mod scan;
pub mod source;
pub mod sso;
pub mod status;
pub mod support;
pub mod time;
pub mod traits;
pub mod upgrade;

pub use error::{Error, Result};
