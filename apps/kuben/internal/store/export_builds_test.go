package store

// HasSlot is the statement the build tests run by hand, as the Rust tests
// did.
const HasSlot = hasSlot

// Bounded exposes bounded to the external tests.
func Bounded(detail string) string { return bounded(detail) }

// LogTail exposes logTail to the external tests.
func LogTail(log string, maxBytes int) string { return logTail(log, maxBytes) }
