package server

import (
	"strconv"
	"strings"
)

// ParseNumber parses INDI's number spellings, plain decimal or sexagesimal
// "D[: ]M[: ]S", reporting false on anything else.
func ParseNumber(s string) (float64, bool) {
	s = strings.TrimSpace(s)
	if v, err := strconv.ParseFloat(s, 64); err == nil {
		return v, true
	}
	fields := strings.FieldsFunc(s, func(r rune) bool { return r == ':' || r == ' ' })
	if len(fields) < 2 || len(fields) > 3 {
		return 0, false
	}
	v, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0, false
	}
	// The sign belongs to the whole value: "-0:30" is -0.5, not +0.5.
	neg := strings.HasPrefix(fields[0], "-")
	if v < 0 {
		v = -v
	}
	for i, scale := range [2]float64{1.0 / 60, 1.0 / 3600} {
		if i+1 >= len(fields) {
			break
		}
		f, err := strconv.ParseFloat(fields[i+1], 64)
		if err != nil {
			return 0, false
		}
		v += f * scale
	}
	if neg {
		v = -v
	}
	return v, true
}
