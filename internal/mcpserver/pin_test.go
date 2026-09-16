package mcpserver

import (
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"

	"github.com/GilmanLab/agentcompute/internal/compute"
	"github.com/GilmanLab/agentcompute/internal/compute/mocks"
)

func TestPinAuthorizationReachesMCPWithoutMutations(t *testing.T) {
	t.Parallel()
	for _, source := range []string{
		"return sandbox.create(name=\"keep\", pinned=True)",
		"return sandbox.pin(name=\"keep\", pinned=True)",
		"return sandbox.pin(name=\"keep\", pinned=False)",
	} {
		t.Run(source, func(t *testing.T) {
			t.Parallel()
			backend := mocks.NewMockBackend(t)
			service, err := compute.New(backend, nil, compute.Options{PinIdentities: []string{"omp"}})
			require.NoError(t, err)
			session := newClientSession(t, Options{Deps: NewDependencies(service, nil)})
			result, err := session.CallTool(
				t.Context(),
				&mcp.CallToolParams{
					Name:      "execute",
					Arguments: map[string]any{"source": "def main():\n    " + source + "\n"},
				},
			)
			require.NoError(t, err)
			requireToolError(t, result, "capability failed: pinning requires an operator identity")
		})
	}
}
