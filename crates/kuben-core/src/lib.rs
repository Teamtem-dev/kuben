//! Kuben core: domain types, configuration, errors and the traits every other
//! crate builds on. This crate deliberately performs **no IO** so it stays
//! cheap to compile and easy to test.

pub mod authz;
pub mod config;
pub mod error;
