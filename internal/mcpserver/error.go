package mcpserver

import (
	"fmt"

	"github.com/meigma/codemode"
)

func agentError(message string) error {
	return &codemode.AgentError{Message: message}
}

func agentErrorf(format string, args ...any) error {
	return &codemode.AgentError{Message: fmt.Sprintf(format, args...)}
}
