package desktop

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/GilmanLab/agentcompute/internal/compute"
)

type windowsSession struct {
	ref       compute.Ref
	session   *mcp.ClientSession
	stream    io.Closer
	version   string
	tools     []string
	closeOnce sync.Once
	closeErr  error
}

type windowsDialError struct {
	err error
}

func (e *windowsDialError) Error() string {
	return e.err.Error()
}

func (e *windowsDialError) Unwrap() error {
	return e.err
}

func windowsGuest(osName string) bool {
	return strings.HasPrefix(strings.ToLower(osName), "windows")
}

func sessionKey(ref compute.Ref) string {
	return ref.Sandbox + "/" + ref.Name
}

// Close drops every cached Windows MCP session.
func (d *Driver) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.closed = true
	var err error
	for key, sess := range d.sessions {
		err = errors.Join(err, sess.Close())
		delete(d.sessions, key)
	}
	return err
}

// CloseSandbox drops cached Windows MCP sessions for one sandbox.
func (d *Driver) CloseSandbox(name string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for key, sess := range d.sessions {
		if sess.ref.Sandbox == name {
			_ = sess.Close()
			delete(d.sessions, key)
		}
	}
}

func (s *windowsSession) Close() error {
	s.closeOnce.Do(func() {
		s.closeErr = errors.Join(s.session.Close(), s.stream.Close())
	})
	return s.closeErr
}

func (d *Driver) windowsInfo(ctx context.Context, ref compute.Ref, inst compute.Instance) (Info, error) {
	sess, err := d.ensureWindows(ctx, ref)
	if err != nil {
		if windowsUnavailable(err) {
			osName, osErr := d.imageOS(ctx, ref, inst.Image)
			return Info{OS: osName, Tools: []string{}}, osErr
		}
		return Info{}, err
	}
	ready, err := d.windowsReady(ctx, ref)
	if err != nil {
		return Info{}, err
	}
	osName, err := d.imageOS(ctx, ref, inst.Image)
	if err != nil {
		return Info{}, err
	}
	vnc, err := d.vncEndpoint(ctx, ref, inst)
	if err != nil {
		return Info{}, err
	}
	return Info{
		Ready:         ready,
		OS:            osName,
		DriverVersion: sess.version,
		Tools:         append([]string{}, sess.tools...),
		VNC:           vnc,
	}, nil
}

func (d *Driver) windowsReady(ctx context.Context, ref compute.Ref) (bool, error) {
	result, err := d.callWindowsTool(ctx, ref, "list_windows", map[string]any{})
	if err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		if _, expiryErr := d.compute.SandboxExpiry(ctx, ref.Sandbox); expiryErr != nil {
			return false, expiryErr
		}
		return false, nil
	}
	return !result.IsError, nil
}

func (d *Driver) callWindows(ctx context.Context, ref compute.Ref, tool, payload string) (CallResult, error) {
	path, err := randomGuestPNG("windows")
	if err != nil {
		return CallResult{}, err
	}
	missing := false
	defer func() {
		if !missing {
			d.removeGuestFile(ctx, ref, path)
		}
	}()
	args, err := callArguments(payload, path)
	if err != nil {
		return CallResult{}, err
	}
	result, err := d.callWindowsTool(ctx, ref, tool, args)
	if err != nil {
		return CallResult{}, err
	}
	classified := classifyMCP(result)
	shot, shotErr := d.pullScreenshot(ctx, ref, path, 0)
	switch {
	case shotErr == nil:
		classified.ScreenshotURL = shot.URL
	case errors.Is(shotErr, os.ErrNotExist):
		missing = true
	case classified.OK:
		return classified, shotErr
	}
	return classified, nil
}

func (d *Driver) screenshotWindows(
	ctx context.Context,
	ref compute.Ref,
	tool, payload string,
	pid, maxDimension int64,
) (Screenshot, error) {
	path, err := randomGuestPNG("windows")
	if err != nil {
		return Screenshot{}, err
	}
	defer d.removeGuestFile(ctx, ref, path)
	args, err := callArguments(payload, path)
	if err != nil {
		return Screenshot{}, err
	}
	result, err := d.callWindowsTool(ctx, ref, tool, args)
	if err != nil {
		return Screenshot{}, err
	}
	classified := classifyMCP(result)
	if !classified.OK {
		if classified.Summary == "" {
			return Screenshot{}, agentError("Driver screenshot failed")
		}
		return Screenshot{}, agentError(classified.Summary)
	}
	var dimensions struct {
		ScreenWidth  float64 `json:"screen_width"`
		WindowBounds struct {
			Width float64 `json:"width"`
		} `json:"window_bounds"`
	}
	if err = json.Unmarshal([]byte(classified.Result), &dimensions); err != nil {
		return Screenshot{}, agentError("Driver screenshot metadata is not JSON")
	}
	originalWidth := dimensions.ScreenWidth
	if pid != 0 {
		originalWidth = dimensions.WindowBounds.Width
	}
	if originalWidth <= 0 {
		return Screenshot{}, agentError("Driver screenshot omitted its coordinate width")
	}
	shot, err := d.pullScreenshot(ctx, ref, path, maxDimension)
	if errors.Is(err, os.ErrNotExist) {
		return Screenshot{}, agentError("Driver did not write a screenshot")
	}
	if err != nil {
		return Screenshot{}, err
	}
	shot.Scale = originalWidth / float64(shot.Width)
	return shot, nil
}

func (d *Driver) callWindowsTool(
	ctx context.Context,
	ref compute.Ref,
	tool string,
	args map[string]any,
) (*mcp.CallToolResult, error) {
	sess, err := d.ensureWindows(ctx, ref)
	if err != nil {
		if windowsUnavailable(err) {
			return nil, agentError("Driver daemon is not running")
		}
		return nil, err
	}
	params := &mcp.CallToolParams{Name: tool, Arguments: args}
	result, err := sess.session.CallTool(ctx, params)
	if err == nil {
		return result, nil
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	// A lost response does not prove a GUI action was not applied.
	// Drop the connection for the next call, but never replay the action.
	d.dropWindows(ref, sess)
	return nil, err
}

func (d *Driver) ensureWindows(ctx context.Context, ref compute.Ref) (*windowsSession, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, err := d.compute.SandboxExpiry(ctx, ref.Sandbox); err != nil {
		return nil, err
	}
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return nil, agentError("desktop driver is closed")
	}
	key := sessionKey(ref)
	if sess := d.sessions[key]; sess != nil {
		d.mu.Unlock()
		return sess, nil
	}
	d.mu.Unlock()

	sess, err := d.dialWindows(ctx, ref)
	if err != nil {
		return nil, err
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		_ = sess.Close()
		return nil, agentError("desktop driver is closed")
	}
	if existing := d.sessions[key]; existing != nil {
		_ = sess.Close()
		return existing, nil
	}
	d.sessions[key] = sess
	return sess, nil
}

func (d *Driver) dialWindows(ctx context.Context, ref compute.Ref) (*windowsSession, error) {
	stream, err := d.compute.OpenExec(ctx, compute.ExecRequest{
		Ref: ref,
		Argv: []string{
			windowsDriverProxy,
			"mcp",
			"--socket",
			windowsDriverSocket,
		},
		Cwd: windowsDriverHome,
	})
	if err != nil {
		return nil, err
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "agentcompute", Version: "1"}, nil)
	session, err := client.Connect(ctx, &mcp.IOTransport{
		Reader: io.NopCloser(stream),
		Writer: stream,
	}, nil)
	if err != nil {
		_ = stream.Close()
		return nil, &windowsDialError{err: err}
	}
	version := ""
	if res := session.InitializeResult(); res != nil && res.ServerInfo != nil {
		version = res.ServerInfo.Version
	}
	var tools []string
	for tool, listErr := range session.Tools(ctx, nil) {
		if listErr != nil {
			_ = session.Close()
			_ = stream.Close()
			return nil, &windowsDialError{err: listErr}
		}
		if tool.Name != "" {
			tools = append(tools, tool.Name)
		}
	}
	if version == "" || len(tools) == 0 {
		_ = session.Close()
		_ = stream.Close()
		return nil, agentError("Driver MCP must include version and tools")
	}
	return &windowsSession{
		ref:     ref,
		session: session,
		stream:  stream,
		version: version,
		tools:   tools,
	}, nil
}

func (d *Driver) dropWindows(ref compute.Ref, failed *windowsSession) {
	d.mu.Lock()
	defer d.mu.Unlock()
	key := sessionKey(ref)
	if sess := d.sessions[key]; sess != nil && sess == failed {
		_ = sess.Close()
		delete(d.sessions, key)
	}
}

func windowsUnavailable(err error) bool {
	var dial *windowsDialError
	if !errors.As(err, &dial) {
		return false
	}
	return !errors.Is(dial.err, context.Canceled) && !errors.Is(dial.err, context.DeadlineExceeded)
}

func callArguments(payload, screenshotPath string) (map[string]any, error) {
	decoder := json.NewDecoder(strings.NewReader(payload))
	decoder.UseNumber()
	var args map[string]any
	if err := decoder.Decode(&args); err != nil {
		return nil, agentError("args must be a JSON object")
	}
	if args == nil {
		args = map[string]any{}
	}
	args["screenshot_out_file"] = screenshotPath
	return args, nil
}

func classifyMCP(result *mcp.CallToolResult) CallResult {
	if result == nil {
		return CallResult{OK: false, Result: jsonNull}
	}
	text := mcpText(result.Content)
	if result.IsError {
		return CallResult{OK: false, Summary: text, Result: jsonNull}
	}
	if result.StructuredContent != nil {
		switch result.StructuredContent.(type) {
		case map[string]any, []any:
			encoded, err := marshalJSON(stripImages(result.StructuredContent))
			if err != nil {
				return CallResult{OK: false, Summary: text, Result: jsonNull}
			}
			return CallResult{OK: true, Summary: "structured content", Result: encoded}
		}
	}
	if isNativeOK(text) {
		return CallResult{OK: true, Summary: text, Result: jsonNull}
	}
	return classifyCall(0, text, "")
}

func mcpText(content []mcp.Content) string {
	var b strings.Builder
	for _, item := range content {
		text, ok := item.(*mcp.TextContent)
		if !ok {
			continue
		}
		b.WriteString(text.Text)
	}
	return strings.TrimSpace(b.String())
}
