package compute

import (
	"crypto/rand"
	"fmt"
)

const (
	maxNameLength     = 32
	nameGenerateTries = 64
	reservedDefault   = "default"
	reservedNone      = "none"
)

func validateName(name string) error {
	if !validName(name) {
		return agentErrorf("invalid name %q", name)
	}
	if name == reservedDefault || name == reservedNone {
		return agentErrorf("name %q is reserved", name)
	}
	return nil
}

func validName(name string) bool {
	n := len(name)
	if n < 1 || n > maxNameLength {
		return false
	}
	for i := range n {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c == '-' && i > 0 && i < n-1:
		default:
			return false
		}
	}
	return true
}

func generateName() (string, error) {
	adjectives := [...]string{
		"amber", "brisk", "coral", "dusky", "eager", "fern",
		"golden", "hardy", "ivory", "jade", "keen", "lucky",
	}
	nouns := [...]string{
		"acorn", "brook", "cedar", "daisy", "ember", "falcon",
		"grove", "heron", "inlet", "jasper", "kestrel", "lagoon",
	}

	var buf [2]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("generate name: %w", err)
	}
	return adjectives[int(buf[0])%len(adjectives)] + "-" + nouns[int(buf[1])%len(nouns)], nil
}
