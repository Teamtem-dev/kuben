package controller

import (
	"math"
	"strconv"
	"strings"
)

// parseDuration reads the human durations of CRD fields (crate::duration):
// `<integer><unit>` segments with the units s, m, h and d, e.g. `90s`,
// `15m`, `168h`, `14d`, `1h30m`. It returns whole seconds, and false for
// empty, malformed or overflowing input.
func parseDuration(input string) (uint64, bool) {
	s := strings.TrimSpace(input)
	if s == "" {
		return 0, false
	}
	var total uint64
	start := 0
	for i := range len(s) {
		c := s[i]
		if c >= '0' && c <= '9' {
			continue
		}
		var unit uint64
		switch c {
		case 's':
			unit = 1
		case 'm':
			unit = 60
		case 'h':
			unit = 3_600
		case 'd':
			unit = 86_400
		default:
			return 0, false
		}
		n, err := strconv.ParseUint(s[start:i], 10, 64)
		if err != nil {
			return 0, false
		}
		start = i + 1
		if n > math.MaxUint64/unit || total > math.MaxUint64-n*unit {
			return 0, false
		}
		total += n * unit
	}
	if start != len(s) {
		return 0, false // trailing number without unit
	}
	return total, true
}

// secondsToMillis is seconds in milliseconds, saturating at MaxInt64.
func secondsToMillis(seconds uint64) int64 {
	if seconds > math.MaxInt64/1000 {
		return math.MaxInt64
	}
	return int64(seconds) * 1000
}
