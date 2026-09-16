package cli

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
	"unicode"

	"github.com/modelcontextprotocol/go-sdk/auth"
)

type bearerIdentity struct {
	name   string
	digest [sha256.Size]byte
}

// loadBearerTokens loads the credential once; rotation takes effect on restart.
func loadBearerTokens(path string) ([]bearerIdentity, error) {
	if path == "" {
		return nil, nil
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open bearer credential file: %w", err)
	}
	defer file.Close()

	decoder := json.NewDecoder(file)
	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('{') {
		return nil, errors.New("bearer credential file must be a JSON object of identity names to tokens")
	}
	var identities []bearerIdentity
	for decoder.More() {
		key, tokenErr := decoder.Token()
		if tokenErr != nil {
			return nil, errors.New("invalid bearer credential entry")
		}
		name, ok := key.(string)
		var token string
		if !ok || decoder.Decode(&token) != nil || invalidCredentialText(name) || invalidCredentialText(token) {
			return nil, errors.New(
				"bearer identity names and tokens must be nonempty strings without whitespace or control characters",
			)
		}
		digest := sha256.Sum256([]byte(token))
		for _, identity := range identities {
			if identity.name == name || subtle.ConstantTimeCompare(identity.digest[:], digest[:]) == 1 {
				return nil, errors.New("bearer credential file contains a duplicate identity or token")
			}
		}
		identities = append(identities, bearerIdentity{name: name, digest: digest})
	}
	closing, err := decoder.Token()
	if err != nil || closing != json.Delim('}') || len(identities) == 0 {
		return nil, errors.New("bearer credential file must contain at least one identity")
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("unexpected data after bearer credential object")
	}
	return identities, nil
}

func invalidCredentialText(value string) bool {
	return value == "" || strings.ContainsFunc(value, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r)
	})
}

func requireBearerTokens(identities []bearerIdentity) func(http.Handler) http.Handler {
	verifier := func(_ context.Context, presented string, _ *http.Request) (*auth.TokenInfo, error) {
		digest := sha256.Sum256([]byte(presented))
		var subject string
		for _, identity := range identities {
			if subtle.ConstantTimeCompare(digest[:], identity.digest[:]) == 1 {
				subject = identity.name
			}
		}
		if subject == "" {
			return nil, auth.ErrInvalidToken
		}
		// The SDK requires an expiration. This bounds its session token metadata;
		// static credentials themselves are rechecked on every request until rotation.
		return &auth.TokenInfo{UserID: subject, Scopes: []string{"mcp"}, Expiration: time.Now().Add(time.Hour)}, nil
	}
	return auth.RequireBearerToken(verifier, &auth.RequireBearerTokenOptions{Scopes: []string{"mcp"}})
}
