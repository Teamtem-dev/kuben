fn main() {
    // `sqlx::migrate!` embeds the migration files at compile time; rebuild
    // when one is added or changed, even if no Rust file changed.
    println!("cargo:rerun-if-changed=migrations");
}
