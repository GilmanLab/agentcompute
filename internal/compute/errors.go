package compute

import (
	"errors"
	"fmt"

	"github.com/meigma/codemode"
)

var (
	// ErrNotFound is the adapter sentinel for a missing backend resource.
	ErrNotFound = errors.New("not found")

	// ErrUnavailable is the adapter sentinel for an unreachable backend.
	ErrUnavailable = errors.New("backend unavailable")
)

func agentError(message string) error {
	return &codemode.AgentError{Message: message}
}

func agentErrorf(format string, args ...any) error {
	return &codemode.AgentError{Message: fmt.Sprintf(format, args...)}
}

func unsupportedOnMac() error {
	return agentError("unsupported on platform mac")
}

func sandboxNotFound(name string) error {
	return agentErrorf("sandbox %q not found", name)
}

func instanceNotFound(ref Ref) error {
	return agentErrorf("instance %q not found in sandbox %q", ref.Name, ref.Sandbox)
}

func networkNotFound(name, sandbox string) error {
	return agentErrorf("network %q not found in sandbox %q", name, sandbox)
}

func imageNotFound(name string) error {
	return agentErrorf("image %q not found", name)
}

func sandboxExpired(name string) error {
	return agentErrorf("sandbox %q has expired", name)
}

func isAlreadyExists(err error, name string) bool {
	var agent *codemode.AgentError
	if !errors.As(err, &agent) || agent == nil {
		return false
	}
	return agent.Message == fmt.Sprintf("sandbox %q already exists", name)
}
