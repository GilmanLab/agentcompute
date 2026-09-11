// Command agent-error probes an actionable error across the CodeMode worker.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/meigma/codemode"
	"github.com/meigma/codemode/authz"
	hostmcp "github.com/meigma/codemode/mcpserver"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type instanceGetInput struct {
	Sandbox string `json:"sandbox"`
	Name    string `json:"name"`
}

type instanceGetOutput struct {
	Name string `json:"name"`
}

func main() {
	codemode.ServeWorkerAndExit()
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	builder := codemode.New(codemode.Options{Authorizer: authz.AllowAll()})
	codemode.Register(builder, codemode.Capability[instanceGetInput, instanceGetOutput]{
		ID:      "instance.get",
		Name:    "instance.get",
		Summary: "Get a named instance in a sandbox.",
		Handler: func(_ context.Context, _ authz.Subject, in instanceGetInput) (instanceGetOutput, error) {
			return instanceGetOutput{}, &codemode.AgentError{Message: fmt.Sprintf("instance %q not found in sandbox %q", in.Name, in.Sandbox)}
		},
	})
	service, err := builder.Build()
	if err != nil {
		return err
	}
	server, err := hostmcp.New(service, hostmcp.StaticSubject(authz.Subject{ID: "spike"}), hostmcp.Options{
		Implementation: &mcp.Implementation{Name: "agentcompute-error-spike", Version: "spike"},
		Logger:         slog.New(slog.DiscardHandler),
	})
	if err != nil {
		return err
	}
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		return err
	}
	defer serverSession.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "spike-client", Version: "spike"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		return err
	}
	defer session.Close()
	for _, call := range []*mcp.CallToolParams{
		{Name: "search_api", Arguments: map[string]any{"query": "instance.get"}},
		{Name: "describe_api", Arguments: map[string]any{"name": "instance.get"}},
		{Name: "execute", Arguments: map[string]any{"source": "def main():\n    return instance.get(sandbox=\"y\", name=\"x\")\n"}},
	} {
		result, err := session.CallTool(ctx, call)
		if err != nil {
			return err
		}
		encoded, err := json.Marshal(result)
		if err != nil {
			return err
		}
		fmt.Printf("%s: %s\n", call.Name, encoded)
		if call.Name != "execute" {
			if result.IsError {
				return fmt.Errorf("%s failed", call.Name)
			}
			continue
		}
		if !result.IsError || len(result.Content) != 1 {
			return fmt.Errorf("expected one MCP tool error")
		}
		text, ok := result.Content[0].(*mcp.TextContent)
		if !ok || text.Text != `capability failed: instance "x" not found in sandbox "y"` {
			return fmt.Errorf("unexpected error content: %v", result.Content)
		}
	}
	return nil
}
