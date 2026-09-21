// Package ascii has the ASCII-only text operations Rust's standard library
// offers on str (to_ascii_lowercase and friends): Go's strings package
// folds all of Unicode, which is a different comparison.
package ascii

// Lower lowercases the ASCII letters of s and leaves every other byte alone.
func Lower(s string) string {
	for i := 0; i < len(s); i++ {
		if c := s[i]; c >= 'A' && c <= 'Z' {
			b := []byte(s)
			for j := i; j < len(b); j++ {
				if b[j] >= 'A' && b[j] <= 'Z' {
					b[j] += 'a' - 'A'
				}
			}
			return string(b)
		}
	}
	return s
}
