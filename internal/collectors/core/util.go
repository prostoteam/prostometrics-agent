package core

import (
	"strconv"
	"strings"
)

func parseUint(s string) (uint64, error) {
	return strconv.ParseUint(strings.TrimSpace(s), 10, 64)
}

func diffUint(a, b uint64) uint64 {
	if b >= a {
		return b - a
	}
	return 0
}
