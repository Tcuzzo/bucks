package brokers

import (
	"errors"
	"sort"
)

// isNotFound reports whether err signals the broker has no record of the order.
func isNotFound(err error) bool {
	return errors.Is(err, ErrOrderNotFound)
}

// sortStrings sorts a slice of symbols in place for deterministic output.
func sortStrings(s []string) {
	sort.Strings(s)
}
