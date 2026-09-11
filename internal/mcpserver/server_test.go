package mcpserver

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/meigma/codemode"
	"github.com/meigma/codemode/authz"
	hostmcp "github.com/meigma/codemode/mcpserver"

	"github.com/GilmanLab/agentcompute/internal/compute"
)

const trustedSubjectID authz.SubjectID = "local"

type executeEnvelope[T any] struct {
	Result T `json:"result"`
}

type denyDeletePolicy struct{}

func (denyDeletePolicy) Authorize(_ context.Context, input authz.AuthorizationInput) error {
	if input.Subject.ID != trustedSubjectID {
		return authz.ErrDenied
	}
	if input.CapabilityName == capabilitySandboxDelete {
		return authz.ErrDenied
	}
	return nil
}

func TestServerEndToEnd(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	tc := newTestDeps(t)
	tc.sandbox.EXPECT().ListSandboxes(mock.Anything).Return([]compute.Sandbox{}, nil)
	session := newClientSession(t, Options{Deps: tc.deps})

	tools, err := session.ListTools(ctx, nil)
	require.NoError(t, err, "list tools")

	names := make([]string, 0, len(tools.Tools))
	for _, tool := range tools.Tools {
		names = append(names, tool.Name)
	}
	assert.ElementsMatch(t, []string{"search_api", "describe_api", "execute"}, names,
		"tools/list must expose exactly the CodeMode tool set")

	searched, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "search_api",
		Arguments: map[string]any{"query": capabilitySandboxList},
	})
	require.NoError(t, err, "search_api")
	requireSuccessfulTool(t, searched)
	var search codemode.SearchResponse
	decodeStructured(t, searched, &search)
	require.NotEmpty(t, search.Results, "search_api must find sandbox.list")
	assert.Equal(t, capabilitySandboxList, search.Results[0].Name)

	described, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "describe_api",
		Arguments: map[string]any{"name": capabilitySandboxList},
	})
	require.NoError(t, err, "describe_api")
	requireSuccessfulTool(t, described)
	var description codemode.Description
	decodeStructured(t, described, &description)
	assert.Equal(t, capabilitySandboxList, description.Name)
	assert.Equal(t, "sandbox.list()", description.Signature)

	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "execute",
		Arguments: map[string]any{"source": "def main():\n    return sandbox.list()\n"},
	})
	require.NoError(t, err, "execute")
	requireSuccessfulTool(t, result)

	var out executeEnvelope[sandboxListOut]
	decodeStructured(t, result, &out)
	require.NotNil(t, out.Result.Items, "empty list root must not be None")
	assert.Empty(t, out.Result.Items)
}

func TestServerAgentErrorReachesMCP(t *testing.T) {
	t.Parallel()

	tc := newTestDeps(t)
	tc.instance.EXPECT().
		GetInstance(mock.Anything, compute.Ref{Sandbox: "y", Name: "x"}).
		Return(compute.Instance{}, agentErrorf("instance %q not found in sandbox %q", "x", "y"))
	session := newClientSession(t, Options{Deps: tc.deps})

	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "execute",
		Arguments: map[string]any{
			"source": "def main():\n    return instance.get(sandbox=\"y\", name=\"x\")\n",
		},
	})
	require.NoError(t, err, "execute")
	requireToolError(t, result, `capability failed: instance "x" not found in sandbox "y"`)
}

func TestServerRejectsMissingSubject(t *testing.T) {
	t.Parallel()

	session := newClientSession(t, Options{Resolver: hostmcp.ContextSubject()})
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "execute",
		Arguments: map[string]any{"source": "def main():\n    return sandbox.list()\n"},
	})
	require.NoError(t, err, "missing subject must be a tool-level error")
	requireToolError(t, result, codemode.ErrUnauthenticated.Error())
}

func TestServerAuthorizesTrustedSubjectAndArguments(t *testing.T) {
	t.Parallel()

	tc := newTestDeps(t)
	tc.sandbox.EXPECT().ListSandboxes(mock.Anything).Return([]compute.Sandbox{}, nil)
	session := newClientSession(t, Options{
		Deps:    tc.deps,
		Runtime: codemode.Options{Authorizer: denyDeletePolicy{}},
	})

	allowed, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "execute",
		Arguments: map[string]any{"source": "def main():\n    return sandbox.list()\n"},
	})
	require.NoError(t, err, "execute")
	requireSuccessfulTool(t, allowed)

	denied, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "execute",
		Arguments: map[string]any{"source": "def main():\n    return sandbox.delete(name=\"demo\")\n"},
	})
	require.NoError(t, err, "denied execute must stay a tool-level error")
	requireToolError(t, denied, codemode.ErrPermissionDenied.Error())
}

func TestServerRequiresAuthorizer(t *testing.T) {
	t.Parallel()

	_, err := New(Options{
		Version:  "test",
		Logger:   slog.New(slog.DiscardHandler),
		Resolver: hostmcp.StaticSubject(authz.Subject{ID: trustedSubjectID}),
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, codemode.ErrInvalidRegistration)
}

func TestServerRequiresResolver(t *testing.T) {
	t.Parallel()

	_, err := New(Options{
		Version: "test",
		Logger:  slog.New(slog.DiscardHandler),
		Runtime: codemode.Options{Authorizer: authz.AllowAll()},
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, codemode.ErrInvalidRegistration)
}

type testDeps struct {
	sandbox  *MocksandboxService
	instance *MockinstanceService
	network  *MocknetworkService
	image    *MockimageService
	deps     Dependencies
}

func newTestDeps(t *testing.T) *testDeps {
	t.Helper()

	sandbox := NewMocksandboxService(t)
	instance := NewMockinstanceService(t)
	network := NewMocknetworkService(t)
	image := NewMockimageService(t)
	return &testDeps{
		sandbox:  sandbox,
		instance: instance,
		network:  network,
		image:    image,
		deps: Dependencies{
			Sandbox:  sandbox,
			Instance: instance,
			Network:  network,
			Image:    image,
		},
	}
}

func newClientSession(t *testing.T, options Options) *mcp.ClientSession {
	t.Helper()

	if options.Logger == nil {
		options.Logger = slog.New(slog.DiscardHandler)
	}
	if options.Version == "" {
		options.Version = "test"
	}
	if options.Resolver == nil {
		options.Resolver = hostmcp.StaticSubject(authz.Subject{ID: trustedSubjectID})
	}
	if options.Runtime.Authorizer == nil {
		options.Runtime.Authorizer = authz.AllowAll()
	}

	serverTransport, clientTransport := mcp.NewInMemoryTransports()

	srv, err := New(options)
	require.NoError(t, err, "construct server")
	serverSession, err := srv.Connect(context.Background(), serverTransport, nil)
	require.NoError(t, err, "server connect")
	t.Cleanup(func() { _ = serverSession.Close() })

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "test"}, nil)
	clientSession, err := client.Connect(context.Background(), clientTransport, nil)
	require.NoError(t, err, "client connect")
	t.Cleanup(func() { _ = clientSession.Close() })

	return clientSession
}

func decodeStructured(t *testing.T, result *mcp.CallToolResult, dest any) {
	t.Helper()

	raw, err := json.Marshal(result.StructuredContent)
	require.NoError(t, err, "marshal structured content")
	require.NoError(t, json.Unmarshal(raw, dest), "unmarshal structured content %q", raw)
}

func requireSuccessfulTool(t *testing.T, result *mcp.CallToolResult) {
	t.Helper()
	require.NotNil(t, result)
	require.False(t, result.IsError, "tool call failed, content: %+v", result.Content)
}

func requireToolError(t *testing.T, result *mcp.CallToolResult, expected string) {
	t.Helper()
	require.NotNil(t, result)
	require.True(t, result.IsError, "expected a tool-level error")
	require.Len(t, result.Content, 1)
	text, ok := result.Content[0].(*mcp.TextContent)
	require.True(t, ok, "tool error content must be text")
	assert.Equal(t, expected, text.Text)
}
