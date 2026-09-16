package desktop

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"golang.org/x/image/draw"

	"github.com/meigma/codemode"

	"github.com/GilmanLab/agentcompute/internal/compute"
)

const (
	driverBin             = "/usr/local/bin/cua-driver"
	driverSocket          = "/run/user/1000/cua-driver.sock"
	driverHome            = "/home/automation"
	driverRuntime         = "/run/user/1000"
	driverUID             = "1000"
	macDriverUser         = "lume"
	macDriverHome         = "/Users/lume"
	driverCommandOverhead = 3
	windowsDriverProxy    = `C:\ProgramData\agentcompute\cua-driver\cua-driver-proxy.exe`
	windowsDriverSocket   = `\\.\pipe\cua-driver`
	windowsDriverHome     = `C:\ProgramData\agentcompute`
	vncPort               = "5900"
	vncTargetPort         = int64(5900)
	nativeOKText          = "[OK]"
	jsonNull              = "null"
	emptyJSONObject       = "{}"
	dataImagePrefix       = "data:image/"
	guestCleanupTimeout   = 10 * time.Second
	guestCleanupQueueSize = 8
	minImagePayloadLen    = 512
	imagePayloadSampleLen = 1024
)

type guestCleanup struct {
	ctx  context.Context
	ref  compute.Ref
	path string
}

// Driver proxies native Cua Driver CLI calls inside Linux and Windows guests.
type Driver struct {
	compute     *compute.Service
	store       *Store
	mu          sync.Mutex
	sessions    map[string]*windowsSession
	closed      bool
	cleanup     chan guestCleanup
	cleanupDone chan struct{}
}

// Info reports Driver catalog metadata, readiness, and a human VNC endpoint.
type Info struct {
	// Ready is true when the guest Driver daemon answers status.
	Ready bool
	// OS is the catalog operating system family for the instance image.
	OS string
	// DriverVersion is the version string from guest dump-docs or MCP initialize.
	DriverVersion string
	// Tools are native tool names from guest dump-docs or MCP ListTools, in discovery order.
	Tools []string
	// VNC is an existing TCP forward to port 5900, or a guest address if none exists.
	VNC string
}

// CallResult is a native tool response with screenshots reduced to URLs.
type CallResult struct {
	// OK is true only for structured JSON or native [OK] text. It does not mean an effect occurred.
	OK bool
	// Summary is a short classification or the exact native diagnostic.
	Summary string
	// Result is stripped native structured content as a JSON string.
	Result string
	// ScreenshotURL is a published PNG URL when the guest wrote a screenshot file.
	ScreenshotURL string
}

// NewDriver returns a Driver that execs through compute and publishes PNGs to store.
func NewDriver(service *compute.Service, store *Store) *Driver {
	return &Driver{compute: service, store: store, sessions: make(map[string]*windowsSession)}
}

// Info discovers dump-docs or MCP tools, probes daemon status, and reports an existing VNC endpoint.
func (d *Driver) Info(ctx context.Context, ref compute.Ref) (Info, error) {
	inst, err := d.compute.GetInstance(ctx, ref)
	if err != nil {
		return Info{}, err
	}
	if !strings.EqualFold(inst.Status, "Running") && !strings.EqualFold(inst.Status, "Ready") {
		osName, osErr := d.imageOS(ctx, ref, inst.Image)
		return Info{OS: osName, Tools: []string{}}, osErr
	}
	if windowsGuest(inst.OS) {
		return d.windowsInfo(ctx, ref, inst)
	}
	version, tools, err := d.discoverDocs(ctx, ref)
	if err != nil {
		return Info{}, err
	}
	ready, err := d.Ready(ctx, ref)
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
		DriverVersion: version,
		Tools:         tools,
		VNC:           vnc,
	}, nil
}

// Enable discovers dump-docs then reports whether the guest daemon answers.
func (d *Driver) Enable(ctx context.Context, ref compute.Ref) (bool, error) {
	inst, err := d.compute.GetInstance(ctx, ref)
	if err != nil {
		return false, err
	}
	if windowsGuest(inst.OS) {
		return d.Ready(ctx, ref)
	}
	if _, _, err := d.discoverDocs(ctx, ref); err != nil {
		return false, err
	}
	return d.Ready(ctx, ref)
}

// Ready reports whether the guest Driver daemon answers status.
//
// A stopped or absent daemon returns false and a nil error so waiters can poll.
// Missing or expired sandboxes, missing instances, and context cancellation
// are returned as errors.
func (d *Driver) Ready(ctx context.Context, ref compute.Ref) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if _, err := d.compute.SandboxExpiry(ctx, ref.Sandbox); err != nil {
		return false, err
	}
	inst, err := d.compute.GetInstance(ctx, ref)
	if err != nil {
		return false, err
	}
	if !strings.EqualFold(inst.Status, "Running") && !strings.EqualFold(inst.Status, "Ready") {
		return false, nil
	}
	if windowsGuest(inst.OS) {
		return d.windowsReady(ctx, ref)
	}
	result, err := d.execDriver(ctx, ref, []string{"status"})
	if err != nil {
		return false, err
	}
	if result.TimedOut {
		return false, nil
	}
	return result.ExitCode == 0, nil
}

// Call invokes a native Driver tool with a JSON object payload.
func (d *Driver) Call(ctx context.Context, ref compute.Ref, tool, args string) (CallResult, error) {
	if strings.TrimSpace(tool) == "" || strings.HasPrefix(tool, "-") {
		return CallResult{}, agentError("tool must be a native Driver tool name, not a CLI option")
	}
	payload, err := normalizeArgs(args)
	if err != nil {
		return CallResult{}, err
	}
	if d.cachedWindows(ref) {
		return d.callWindows(ctx, ref, tool, payload)
	}
	inst, err := d.compute.GetInstance(ctx, ref)
	if err != nil {
		return CallResult{}, err
	}
	if windowsGuest(inst.OS) {
		return d.callWindows(ctx, ref, tool, payload)
	}
	path, err := randomGuestPNG(inst.OS)
	if err != nil {
		return CallResult{}, err
	}
	missing := false
	defer func() {
		if !missing {
			d.removeGuestFile(ctx, ref, path)
		}
	}()

	result, err := d.execDriver(ctx, ref, callArgv(tool, payload, path))
	if err != nil {
		return CallResult{}, err
	}
	if result.TimedOut {
		return CallResult{}, agentError("Driver call timed out")
	}
	classified := classifyCall(result.ExitCode, result.Stdout, result.Stderr)
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

// Screenshot captures the desktop or a pid/window_id pair without an accessibility tree.
//
// Zero pid and window_id select get_desktop_state. get_desktop_state has no
// max_dimension input, so requested bounds are applied by host-side resize
// only when the guest PNG's long edge exceeds max_dimension. Window captures
// pass max_dimension through to get_window_state and still honor the bound
// after the file is pulled. Scale maps returned pixels back to the native
// window or screen coordinate width, including any resize performed in-guest.
func (d *Driver) Screenshot(
	ctx context.Context,
	ref compute.Ref,
	pid, windowID, maxDimension int64,
) (Screenshot, error) {
	if err := validateScreenshotArgs(pid, windowID, maxDimension); err != nil {
		return Screenshot{}, err
	}
	tool := "get_desktop_state"
	payload := emptyJSONObject
	if pid != 0 {
		tool = "get_window_state"
		body := map[string]any{
			"pid":                        pid,
			"window_id":                  windowID,
			"include_accessibility_tree": false,
		}
		if maxDimension > 0 {
			body["max_dimension"] = maxDimension
		}
		encoded, err := marshalJSON(body)
		if err != nil {
			return Screenshot{}, err
		}
		payload = encoded
	}
	if d.cachedWindows(ref) {
		return d.screenshotWindows(ctx, ref, tool, payload, pid, maxDimension)
	}
	inst, err := d.compute.GetInstance(ctx, ref)
	if err != nil {
		return Screenshot{}, err
	}
	if windowsGuest(inst.OS) {
		return d.screenshotWindows(ctx, ref, tool, payload, pid, maxDimension)
	}
	path, err := randomGuestPNG(inst.OS)
	if err != nil {
		return Screenshot{}, err
	}
	defer d.removeGuestFile(ctx, ref, path)

	result, err := d.execDriver(ctx, ref, callArgv(tool, payload, path))
	if err != nil {
		return Screenshot{}, err
	}
	if result.TimedOut {
		return Screenshot{}, agentError("Driver screenshot timed out")
	}
	classified := classifyCall(result.ExitCode, result.Stdout, result.Stderr)
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

func (d *Driver) discoverDocs(ctx context.Context, ref compute.Ref) (string, []string, error) {
	result, err := d.execDriver(ctx, ref, []string{"dump-docs", "--type", "mcp"})
	if err != nil {
		return "", nil, err
	}
	if result.TimedOut {
		return "", nil, agentError("Driver dump-docs timed out")
	}
	if result.ExitCode != 0 {
		return "", nil, agentError(nonzeroDiagnostic(result.ExitCode, result.Stdout, result.Stderr, "Driver dump-docs"))
	}
	return parseDumpDocs([]byte(result.Stdout))
}

func (d *Driver) imageOS(ctx context.Context, ref compute.Ref, imageName string) (string, error) {
	image, err := d.compute.ResolveImage(ctx, ref.Sandbox, imageName)
	if err != nil {
		if imageMissing(err) {
			return "", nil
		}
		return "", err
	}
	return image.OS, nil
}

func (d *Driver) vncEndpoint(ctx context.Context, ref compute.Ref, inst compute.Instance) (string, error) {
	if inst.VNCURL != "" {
		return inst.VNCURL, nil
	}
	if macGuest(inst.OS) {
		return "", nil
	}
	fwd, err := d.compute.InstanceForward(ctx, ref, vncTargetPort, "tcp")
	if err != nil {
		return "", err
	}
	if fwd.Address != "" {
		port := fwd.Port
		if port <= 0 {
			port = vncTargetPort
		}
		return net.JoinHostPort(fwd.Address, strconv.FormatInt(port, 10)), nil
	}
	for _, nic := range inst.NICs {
		for _, addr := range nic.Addresses {
			if addr != "" {
				return net.JoinHostPort(addr, vncPort), nil
			}
		}
	}
	return "", nil
}

func (d *Driver) execDriver(
	ctx context.Context,
	ref compute.Ref,
	args []string,
) (compute.ExecResult, error) {
	inst, err := d.compute.GetInstance(ctx, ref)
	if err != nil {
		return compute.ExecResult{}, err
	}
	req := compute.ExecRequest{Ref: ref}
	if macGuest(inst.OS) {
		req.User = macDriverUser
		req.Cwd = macDriverHome
		req.Env = map[string]string{"HOME": macDriverHome}
		req.Argv = make([]string, 0, len(args)+1)
		req.Argv = append(req.Argv, driverBin)
		req.Argv = append(req.Argv, args...)
		return d.compute.ExecJSON(ctx, req)
	}
	req.User = driverUID
	req.Cwd = driverHome
	req.Env = driverEnv()
	req.Argv = make([]string, 0, len(args)+driverCommandOverhead)
	req.Argv = append(req.Argv, driverBin)
	req.Argv = append(req.Argv, args...)
	if args[0] != "dump-docs" {
		req.Argv = append(req.Argv, "--socket", driverSocket)
	}
	return d.compute.ExecJSON(ctx, req)
}

// queueGuestCleanup keeps file deletion off the completed screenshot's response path.
// A full queue applies backpressure rather than leaking files or spawning workers.
func (d *Driver) queueGuestCleanup(ctx context.Context, ref compute.Ref, path string) {
	if path == "" {
		return
	}
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		d.removeGuestFile(ctx, ref, path)
		return
	}
	if d.cleanup == nil {
		d.cleanup = make(chan guestCleanup, guestCleanupQueueSize)
		d.cleanupDone = make(chan struct{})
		go d.cleanGuestFiles(d.cleanup, d.cleanupDone)
	}
	select {
	case d.cleanup <- guestCleanup{ctx: ctx, ref: ref, path: path}:
		d.mu.Unlock()
	default:
		d.mu.Unlock()
		d.removeGuestFile(ctx, ref, path)
	}
}

func (d *Driver) cleanGuestFiles(queue <-chan guestCleanup, done chan<- struct{}) {
	defer close(done)
	for file := range queue {
		d.removeGuestFile(file.ctx, file.ref, file.path)
	}
}

func (d *Driver) removeGuestFile(ctx context.Context, ref compute.Ref, path string) {
	if path == "" {
		return
	}
	rmCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), guestCleanupTimeout)
	defer cancel()
	_ = d.compute.DeleteFile(rmCtx, ref, path)
}

func (d *Driver) pullScreenshot(
	ctx context.Context,
	ref compute.Ref,
	path string,
	maxDimension int64,
) (Screenshot, error) {
	body, expiry, err := d.compute.ReadBinaryFile(ctx, ref, path)
	if err != nil {
		return Screenshot{}, err
	}
	defer body.Close()
	raw, err := io.ReadAll(io.LimitReader(body, MaxImageBytes+1))
	if err != nil {
		return Screenshot{}, fmt.Errorf("read screenshot: %w", err)
	}
	if int64(len(raw)) > MaxImageBytes {
		return Screenshot{}, agentError("screenshot exceeds 16 MiB")
	}
	if maxDimension <= 0 {
		return d.store.Publish(ref.Sandbox, expiry, bytes.NewReader(raw))
	}
	return publishBoundedPNG(d.store, ref.Sandbox, expiry, raw, maxDimension)
}

func callArgv(tool, payload, screenshotPath string) []string {
	return []string{
		"call",
		"--screenshot-out-file", screenshotPath,
		tool, payload,
	}
}

func driverEnv() map[string]string {
	return map[string]string{
		"HOME":            driverHome,
		"XDG_RUNTIME_DIR": driverRuntime,
	}
}

func randomGuestPNG(osName string) (string, error) {
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return "", fmt.Errorf("generate screenshot path: %w", err)
	}
	if strings.HasPrefix(strings.ToLower(osName), "windows") {
		return `C:\ProgramData\agentcompute\cua-` + hex.EncodeToString(token[:]) + ".png", nil
	}
	return "/tmp/cua-" + hex.EncodeToString(token[:]) + ".png", nil
}

func validateScreenshotArgs(pid, windowID, maxDimension int64) error {
	if pid < 0 || windowID < 0 {
		return agentError("pid and window_id must be non-negative")
	}
	if (pid == 0) != (windowID == 0) {
		return agentError("pid and window_id must both be set or both omitted")
	}
	if maxDimension < 0 {
		return agentError("max_dimension must be a positive integer")
	}
	return nil
}

func normalizeArgs(args string) (string, error) {
	trimmed := strings.TrimSpace(args)
	if trimmed == "" {
		return emptyJSONObject, nil
	}
	if trimmed[0] != '{' || !json.Valid([]byte(trimmed)) {
		return "", agentError("args must be a JSON object")
	}
	return trimmed, nil
}

func parseDumpDocs(raw []byte) (string, []string, error) {
	raw = bytes.TrimSpace(raw)
	var docs struct {
		Version string `json:"version"`
		Tools   []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(raw, &docs); err != nil {
		return "", nil, agentError("Driver dump-docs did not return JSON")
	}
	tools := make([]string, 0, len(docs.Tools))
	for _, tool := range docs.Tools {
		if tool.Name != "" {
			tools = append(tools, tool.Name)
		}
	}
	if docs.Version == "" || len(tools) == 0 {
		return "", nil, agentError("Driver dump-docs must include version and tools")
	}
	return docs.Version, tools, nil
}

// classifyCall interprets one-shot CLI stdout. Native run_call drops MCP isError
// and prints structuredContent when present, otherwise text, so exit 0 is not an
// effect claim. Ambiguous non-JSON text fails closed except native [OK].
func classifyCall(exitCode int64, stdout, stderr string) CallResult {
	out := strings.TrimSpace(stdout)
	errText := strings.TrimSpace(stderr)
	if exitCode != 0 {
		summary := errText
		if summary == "" {
			summary = out
		}
		return CallResult{OK: false, Summary: summary, Result: jsonNull}
	}
	if out == "" {
		return CallResult{OK: false, Summary: errText, Result: jsonNull}
	}
	if isNativeOK(out) {
		return CallResult{OK: true, Summary: out, Result: jsonNull}
	}
	decoder := json.NewDecoder(strings.NewReader(out))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return CallResult{OK: false, Summary: out, Result: jsonNull}
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return CallResult{OK: false, Summary: out, Result: jsonNull}
	}
	switch value.(type) {
	case map[string]any, []any:
		encoded, err := marshalJSON(stripImages(value))
		if err != nil {
			return CallResult{OK: false, Summary: out, Result: jsonNull}
		}
		return CallResult{OK: true, Summary: "structured content", Result: encoded}
	default:
		return CallResult{OK: false, Summary: out, Result: jsonNull}
	}
}

func isNativeOK(text string) bool {
	if text == nativeOKText {
		return true
	}
	if strings.HasPrefix(text, nativeOKText) && len(text) > len(nativeOKText) {
		return unicode.IsSpace(rune(text[len(nativeOKText)]))
	}
	return false
}

func stripImages(v any) any {
	switch t := v.(type) {
	case map[string]any:
		stripImageMap(t)
		return t
	case []any:
		for i, child := range t {
			t[i] = stripImages(child)
		}
		return t
	case string:
		if strings.HasPrefix(t, dataImagePrefix) {
			return ""
		}
		return t
	default:
		return v
	}
}

func stripImageMap(m map[string]any) {
	if typeName, _ := m["type"].(string); strings.EqualFold(typeName, "image") {
		delete(m, "data")
		if src, ok := m["source"].(map[string]any); ok {
			delete(src, "data")
		}
	}
	for key, child := range m {
		if shouldStripImageValue(key, child) {
			delete(m, key)
			continue
		}
		m[key] = stripImages(child)
	}
}

func shouldStripImageValue(key string, child any) bool {
	s, ok := child.(string)
	if !ok {
		return false
	}
	return strings.HasPrefix(s, dataImagePrefix) || (stripImageKey(key) && looksLikeImagePayload(s))
}

func stripImageKey(key string) bool {
	switch strings.ToLower(key) {
	case "screenshot", "image", "image_data", pngFormat, "png_base64",
		"jpeg", "jpeg_base64", "screenshot_png_b64", "screenshot_base64",
		"image_base64", "screenshot_data":
		return true
	default:
		return false
	}
}

func looksLikeImagePayload(s string) bool {
	if strings.HasPrefix(s, dataImagePrefix) {
		return true
	}
	if len(s) < minImagePayloadLen {
		return false
	}
	sample := s
	if len(sample) > imagePayloadSampleLen {
		sample = sample[:imagePayloadSampleLen]
	}
	for _, r := range sample {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '+' || r == '/' || r == '=' || r == '\n' || r == '\r' {
			continue
		}
		return false
	}
	return true
}

func publishBoundedPNG(
	store *Store,
	sandbox string,
	expiry time.Time,
	raw []byte,
	maxDimension int64,
) (Screenshot, error) {
	config, format, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil || format != pngFormat {
		return store.Publish(sandbox, expiry, bytes.NewReader(raw))
	}
	if config.Width <= 0 || config.Height <= 0 || int64(config.Width) > maxImagePixels/int64(config.Height) {
		return Screenshot{}, agentError("screenshot exceeds pixel bounds")
	}
	if maxDimension <= 0 || int64(max(config.Width, config.Height)) <= maxDimension {
		return store.Publish(sandbox, expiry, bytes.NewReader(raw))
	}
	src, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return store.Publish(sandbox, expiry, bytes.NewReader(raw))
	}
	origW := src.Bounds().Dx()
	bounded := boundImage(src, int(maxDimension))
	payload := raw
	if bounded != src {
		var buf bytes.Buffer
		if err = png.Encode(&buf, bounded); err != nil {
			return Screenshot{}, agentError("encode screenshot PNG")
		}
		payload = buf.Bytes()
	}
	shot, err := store.Publish(sandbox, expiry, bytes.NewReader(payload))
	if err != nil {
		return Screenshot{}, err
	}
	if shot.Width > 0 && origW > 0 {
		shot.Scale = float64(origW) / float64(shot.Width)
	}
	return shot, nil
}

func boundImage(src image.Image, maxDimension int) image.Image {
	b := src.Bounds()
	w, h := boundsForDimension(b.Dx(), b.Dy(), maxDimension)
	if w == b.Dx() && h == b.Dy() {
		return src
	}
	dst := image.NewNRGBA(image.Rect(0, 0, w, h))
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, b, draw.Src, nil)
	return dst
}

func boundsForDimension(width, height, maxDimension int) (int, int) {
	if maxDimension <= 0 || width <= 0 || height <= 0 {
		return width, height
	}
	long := max(width, height)
	if long <= maxDimension {
		return width, height
	}
	return max(width*maxDimension/long, 1), max(height*maxDimension/long, 1)
}

func marshalJSON(v any) (string, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return "", err
	}
	return strings.TrimSpace(buf.String()), nil
}

func nonzeroDiagnostic(code int64, stdout, stderr, op string) string {
	summary := strings.TrimSpace(stderr)
	if summary == "" {
		summary = strings.TrimSpace(stdout)
	}
	if summary == "" {
		return fmt.Sprintf("%s exited %d", op, code)
	}
	return summary
}

func imageMissing(err error) bool {
	var agent *codemode.AgentError
	if !errors.As(err, &agent) || agent == nil {
		return false
	}
	return strings.HasPrefix(agent.Message, "image ") && strings.HasSuffix(agent.Message, " not found")
}

func agentError(message string) error {
	return &codemode.AgentError{Message: message}
}
