package api

// Internals of secrets.go and registries.go under test.
var (
	CheckValues    = checkValues
	LegacySelector = legacySelector
	RegistryName   = registryName
	CheckLogin     = checkLogin
)

// Bounds of secrets and logins.
const (
	MaxSecretBytes = maxSecretBytes
	MaxSecretKeys  = maxSecretKeys
	MaxLoginField  = maxLoginField
)
