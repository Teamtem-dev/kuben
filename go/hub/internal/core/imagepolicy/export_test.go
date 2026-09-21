package imagepolicy

// GlobMatches exposes globMatches to the external tests.
func GlobMatches(glob, tag string) bool { return globMatches(glob, tag) }

// Natural exposes natural to the external tests.
func Natural(a, b string) int { return natural(a, b) }

// ParseVersionReq exposes parseVersionReq to the external tests.
func ParseVersionReq(text string) (VersionReq, error) { return parseVersionReq(text) }
