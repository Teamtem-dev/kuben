// Package core is Kuben's domain: types, rules and state machines. Packages
// under core perform no IO and import nothing from store, platform or api,
// so they stay cheap to test and safe to reuse everywhere.
package core
