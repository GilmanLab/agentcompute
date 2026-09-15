package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/spf13/cobra"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/meigma/codemode/authz"
	hostmcp "github.com/meigma/codemode/mcpserver"

	"github.com/GilmanLab/agentcompute/internal/desktop"
	"github.com/GilmanLab/agentcompute/internal/mcpserver"
	"github.com/GilmanLab/agentcompute/internal/templateinfo"
)

const (
	// httpCommandName is the name of the http subcommand, also used by its tests.
	httpCommandName = "http"
	// Flag (and viper key) names for the http subcommand. Shared between flag
	// registration and value retrieval so the two cannot drift.
	addrFlag           = "addr"
	credentialFileFlag = "auth-tokens-file" //nolint:gosec // G101: flag name, not credential material.
	insecureFlag       = "insecure"
	// httpShutdownTimeout bounds how long graceful shutdown waits for in-flight
	// requests to finish before the server is forced closed.
	httpShutdownTimeout = 10 * time.Second
	// httpReadHeaderTimeout bounds how long the server waits to read request
	// headers, mitigating Slowloris-style attacks.
	httpReadHeaderTimeout = 10 * time.Second
)

// httpDevelopmentSubjectID is the explicit development identity for allowed
// loopback and --insecure unauthenticated HTTP modes.
const httpDevelopmentSubjectID authz.SubjectID = "development"

// httpConfig carries the resolved http subcommand configuration into runHTTP.
type httpConfig struct {
	// build supplies the version reported to MCP clients.
	build BuildInfo
	// addr is the host:port to bind.
	addr string
	// authTokens holds validated credential digests and their non-secret identities.
	authTokens []bearerIdentity
	// insecure permits a non-loopback bind without authentication.
	insecure bool
	// logger receives the server's diagnostics and lifecycle events. It must
	// write to stderr; the HTTP transport has no stdout JSON-RPC constraint, but
	// keeping logs on stderr stays consistent with the stdio transport.
	logger *slog.Logger
	// deps are constructed once before serving any HTTP session.
	deps        mcpserver.Dependencies
	screenshots *desktop.Store
}

// newHTTPCommand builds the "http" subcommand, which serves the MCP server over
// the Streamable HTTP transport for networked clients.
//
// To produce a stdio-only repository, delete this file and its AddCommand call
// in root.go.
func newHTTPCommand(options Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   httpCommandName,
		Short: "Serve the MCP server over Streamable HTTP (networked transport)",
		Long: "Serve the MCP server over the Streamable HTTP transport.\n\n" +
			"Binds to a loopback address by default; exposing it to the network " +
			"is an explicit, security-relevant opt-in.",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			logger, err := resolveLogger(options.Viper, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			tokens, err := loadBearerTokens(options.Viper.GetString(credentialFileFlag))
			if err != nil {
				return err
			}
			if bindErr := checkBindSecurity(
				options.Viper.GetString(addrFlag),
				len(tokens) > 0,
				options.Viper.GetBool(insecureFlag),
			); bindErr != nil {
				return bindErr
			}
			rt, err := options.openRuntime(cmd.Context(), logger)
			if err != nil {
				return err
			}
			runErr := runHTTP(cmd.Context(), httpConfig{
				build:       options.Build,
				addr:        options.Viper.GetString(addrFlag),
				authTokens:  tokens,
				insecure:    options.Viper.GetBool(insecureFlag),
				logger:      logger,
				deps:        rt.deps,
				screenshots: rt.screenshots,
			})
			return errors.Join(runErr, rt.close())
		},
	}

	// --addr defaults to loopback, not 0.0.0.0: binding to all interfaces
	// exposes the server to the local network and is a deliberate opt-in.
	cmd.Flags().String(
		addrFlag,
		"localhost:8080",
		fmt.Sprintf("address to listen on (env %s_ADDR)", templateinfo.EnvPrefix()),
	)
	cmd.Flags().String(
		credentialFileFlag,
		"",
		fmt.Sprintf(
			"JSON credential file mapping identity names to bearer tokens (env %s_AUTH_TOKENS_FILE)",
			templateinfo.EnvPrefix(),
		),
	)
	// --insecure is the explicit opt-in to bind a non-loopback address without
	// authentication. Without it, runHTTP refuses such a configuration so a
	// published port or container cannot silently expose every tool.
	cmd.Flags().Bool(
		insecureFlag,
		false,
		fmt.Sprintf(
			"allow binding a non-loopback address without authentication (UNSAFE; env %s_INSECURE)",
			templateinfo.EnvPrefix(),
		),
	)

	return cmd
}

// runHTTP validates the bind configuration, binds the listener, and serves the
// MCP server on it until the context is cancelled.
func runHTTP(ctx context.Context, cfg httpConfig) error {
	if err := checkBindSecurity(cfg.addr, len(cfg.authTokens) > 0, cfg.insecure); err != nil {
		return err
	}

	// Bind separately from serving so configuration errors surface here, before
	// the shutdown machinery starts, and so tests can serve on an ephemeral
	// port (127.0.0.1:0).
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", cfg.addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.addr, err)
	}

	return serveHTTP(ctx, ln, cfg)
}

// serveHTTP serves the MCP server on the bound listener until the context is
// cancelled, then shuts down gracefully, draining in-flight requests for up to
// httpShutdownTimeout. The listener is closed by the time serveHTTP returns.
func serveHTTP(ctx context.Context, ln net.Listener, cfg httpConfig) error {
	logger := cfg.logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}

	// One CodeMode runtime and MCP server for the process. Identity is not
	// captured here: HTTP uses ContextSubject and per-request trusted context.
	mcpServer, err := newComputeServer(logger, cfg.build.Version, hostmcp.ContextSubject(), cfg.deps)
	if err != nil {
		closeErr := ln.Close()
		return errors.Join(err, closeErr)
	}
	mcpServer.AddReceivingMiddleware(installHTTPSubject(len(cfg.authTokens) > 0))

	handler := mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server {
			return mcpServer
		},
		&mcp.StreamableHTTPOptions{
			// Authenticated reverse proxies preserve the public Host header.
			// Bearer verification below remains mandatory before SDK dispatch.
			// Unauthenticated loopback servers retain DNS rebinding protection.
			DisableLocalhostProtection: len(cfg.authTokens) > 0,
		},
	)

	// MUST: the SDK does NOT enable Origin verification by default. Wrapping the
	// handler with the Go 1.25+ stdlib CrossOriginProtection rejects
	// cross-origin browser requests, mitigating CSRF and DNS-rebinding attacks.
	rootHandler := http.NewCrossOriginProtection().Handler(handler)

	// Authenticate before accepting MCP requests, including through a loopback proxy.
	if len(cfg.authTokens) > 0 {
		rootHandler = requireBearerTokens(cfg.authTokens)(rootHandler)
	}
	if cfg.screenshots != nil {
		mux := http.NewServeMux()
		mux.Handle(desktop.ScreenshotPath, cfg.screenshots)
		mux.Handle("/", rootHandler)
		rootHandler = mux
	}

	srv := &http.Server{
		Handler:           rootHandler,
		ReadHeaderTimeout: httpReadHeaderTimeout,
	}

	logger.InfoContext(ctx, "listening", "addr", ln.Addr().String())

	// Graceful shutdown: when the context is cancelled (e.g. SIGINT/SIGTERM),
	// stop accepting connections and let in-flight requests drain. The
	// goroutine exits either way (ctx cancelled, or serveDone closed when
	// Serve returns for another reason), so it never leaks.
	serveDone := make(chan struct{})
	shutdownErr := make(chan error, 1)
	// G118 (gosec): the goroutine derives Shutdown's context from
	// context.Background rather than ctx on purpose. ctx is already cancelled by
	// the time this case fires (that cancellation is what triggers shutdown), so
	// it cannot bound the drain; a fresh context gives in-flight requests time to
	// finish.
	//nolint:gosec // G118: ctx is cancelled by design; see comment above.
	go func() {
		select {
		case <-ctx.Done():
			logger.InfoContext(ctx, "shutting down", "drain_timeout", httpShutdownTimeout.String())
			shutdownCtx, cancel := context.WithTimeout(context.Background(), httpShutdownTimeout)
			defer cancel()
			shutdownErr <- srv.Shutdown(shutdownCtx)
		case <-serveDone:
			shutdownErr <- nil
		}
	}()

	err = srv.Serve(ln)
	close(serveDone)
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve http: %w", err)
	}
	if err := <-shutdownErr; err != nil {
		return fmt.Errorf("shutdown http server: %w", err)
	}

	logger.InfoContext(ctx, "shut down cleanly")

	return nil
}

// checkBindSecurity fails closed against a network-exposed, unauthenticated
// server. A non-loopback bind with no authentication would expose every tool to
// anyone who can reach the port — CrossOriginProtection only stops browser
// cross-origin requests, not direct clients such as curl. Such a configuration
// is allowed only when the operator explicitly opts in with --insecure.
func checkBindSecurity(addr string, authenticated, insecure bool) error {
	if isLoopbackHost(addr) || authenticated || insecure {
		return nil
	}

	return fmt.Errorf(
		"refusing to bind non-loopback address %q without authentication: "+
			"set --auth-tokens-file (env %s_AUTH_TOKENS_FILE) to require a bearer token, "+
			"or pass --insecure to expose all tools unauthenticated (UNSAFE)",
		addr,
		templateinfo.EnvPrefix(),
	)
}

// isLoopbackHost reports whether addr binds only the loopback interface.
//
// It treats "localhost" and any loopback IP literal as loopback. An empty host
// (for example ":8080") binds all interfaces and is not loopback, and a
// non-localhost hostname cannot be proven loopback without resolving it, so it
// is treated as non-loopback. The conservative direction is deliberate: when in
// doubt, the caller's fail-closed guard applies.
func isLoopbackHost(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		// No port present; treat the whole string as the host.
		host = addr
	}
	switch host {
	case "":
		return false
	case "localhost":
		return true
	default:
		if ip := net.ParseIP(host); ip != nil {
			return ip.IsLoopback()
		}
		return false
	}
}

// installHTTPSubject copies per-request identity into the MCP handler context.
// TokenInfo is present only after the bearer verifier succeeds. Unauthenticated
// loopback and --insecure modes receive the explicit development identity.
// When a token is configured and TokenInfo is missing, the subject is left
// unset so ContextSubject fails closed.
func installHTTPSubject(authenticated bool) mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			extra := req.GetExtra()
			if extra != nil && extra.TokenInfo != nil {
				ctx = authz.WithSubject(ctx, authz.Subject{ID: authz.SubjectID(extra.TokenInfo.UserID)})
				return next(ctx, method, req)
			}
			if authenticated {
				return next(authz.WithSubject(ctx, authz.Subject{}), method, req)
			}
			return next(authz.WithSubject(ctx, authz.Subject{ID: httpDevelopmentSubjectID}), method, req)
		}
	}
}
