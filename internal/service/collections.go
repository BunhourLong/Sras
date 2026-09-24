package service

import (
	"fmt"
	"regexp"
)

// collectionPattern allows [a-z0-9_-]{1,64}, which also rejects / and NUL.
var collectionPattern = regexp.MustCompile(`^[a-z0-9_-]{1,64}$`)

// validateCollection checks a collection name before it is used in a key.
func validateCollection(name string) error {
	if !collectionPattern.MatchString(name) {
		return fmt.Errorf("%q: %w", name, ErrInvalidCollection)
	}
	return nil
}
