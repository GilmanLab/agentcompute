package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
)

func (rt *runtime) runStdio(
	ctx context.Context,
	logger *slog.Logger,
	build BuildInfo,
	in io.Reader,
	out io.Writer,
) error {
	if rt.screenshots == nil {
		return runStdio(ctx, logger, build, in, out, rt.deps)
	}
	lifecycle, cancel := context.WithCancel(ctx)
	defer cancel()
	var lc net.ListenConfig
	listener, err := lc.Listen(lifecycle, "tcp", rt.screenshotListen)
	if err != nil {
		return fmt.Errorf("listen for screenshots on %s: %w", rt.screenshotListen, err)
	}
	server := &http.Server{Handler: rt.screenshots, ReadHeaderTimeout: httpReadHeaderTimeout}
	served := make(chan error, 1)
	go func() {
		err := server.Serve(listener)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		served <- err
		cancel()
	}()
	logger.InfoContext(ctx, "serving screenshots", "addr", listener.Addr().String())
	runErr := runStdio(lifecycle, logger, build, in, out, rt.deps)
	shutdownCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), httpShutdownTimeout)
	defer stop()
	shutdownErr := server.Shutdown(shutdownCtx)
	if shutdownErr != nil {
		shutdownErr = errors.Join(shutdownErr, server.Close())
	}
	return errors.Join(runErr, shutdownErr, <-served)
}
