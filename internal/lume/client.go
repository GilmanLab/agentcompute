// Package lume implements the compute.Backend seam over Lume on a Mac host.
package lume

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/GilmanLab/agentcompute/internal/compute"
)

const (
	hostUser          = "agentcompute"
	guestUser         = "lume"
	guestUID          = "501"
	jumpHostAlias     = "ac-host"
	guestHostAlias    = "ac-guest"
	lumeAPIAddr       = "127.0.0.1:7777"
	lumeAPIBase       = "http://127.0.0.1:7777"
	defaultSSHPort    = "22"
	hostOutputLimit   = 1 << 20
	apiBodyLimit      = 8 << 20
	sshDialTimeout    = 30 * time.Second
	execTeardownBound = 5 * time.Second
)

// Options configures a Lume client.
type Options struct {
	// Host is the Mac hostname or IP, without a user. A host:port value
	// selects a non-default SSH port.
	Host string

	// IdentityFile is the server-to-host private key path.
	IdentityFile string

	// KnownHostsFile verifies the Mac host key.
	KnownHostsFile string

	// GuestKnownHostsFile verifies guest keys by seed HostKeyAlias.
	GuestKnownHostsFile string

	// GuestKeys maps catalog image name to a server-side guest private key path.
	GuestKeys map[string]string
}

// Client is the Lume adapter used by compute.Service.
type Client struct {
	opts Options
	mu   sync.Mutex

	ssh     *ssh.Client
	http    *http.Client
	tmpDir  string
	sshPath string
	closed  atomic.Bool
}

// New dials the Mac host over SSH and returns a Client.
func New(ctx context.Context, opts Options) (*Client, error) {
	if opts.Host == "" {
		return nil, errors.New("lume host is required")
	}
	if opts.IdentityFile == "" {
		return nil, errors.New("lume identity file is required")
	}
	if opts.KnownHostsFile == "" {
		return nil, errors.New("lume known hosts file is required")
	}
	if opts.GuestKnownHostsFile == "" {
		return nil, errors.New("lume guest known hosts file is required")
	}

	cleaned, err := cleanOptions(opts)
	if err != nil {
		return nil, err
	}

	keyBytes, err := os.ReadFile(cleaned.IdentityFile)
	if err != nil {
		return nil, fmt.Errorf("read identity file: %w", err)
	}
	signer, err := ssh.ParsePrivateKey(keyBytes)
	if err != nil {
		return nil, fmt.Errorf("parse identity file: %w", err)
	}
	hostKeyCallback, err := knownhosts.New(cleaned.KnownHostsFile)
	if err != nil {
		return nil, fmt.Errorf("load known hosts: %w", err)
	}

	addr := sshDialAddr(cleaned.Host)
	dialer := net.Dialer{Timeout: sshDialTimeout}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("%w: ssh dial: %w", compute.ErrUnavailable, err)
	}
	sshConfig := &ssh.ClientConfig{
		User:            hostUser,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: hostKeyCallback,
		Timeout:         sshDialTimeout,
	}
	clientConn, chans, reqs, err := ssh.NewClientConn(conn, addr, sshConfig)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("ssh handshake: %w", err)
	}
	sshClient := ssh.NewClient(clientConn, chans, reqs)

	tmpDir, err := os.MkdirTemp("", "agentcompute-lume-")
	if err != nil {
		_ = sshClient.Close()
		return nil, fmt.Errorf("create ssh config directory: %w", err)
	}

	sshPath, err := exec.LookPath("ssh")
	if err != nil {
		_ = sshClient.Close()
		_ = os.RemoveAll(tmpDir)
		return nil, fmt.Errorf("find ssh: %w", err)
	}

	client := &Client{
		opts:    cleaned,
		ssh:     sshClient,
		tmpDir:  tmpDir,
		sshPath: sshPath,
	}
	client.http = &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				if client.closed.Load() {
					return nil, errors.New("lume client is closed")
				}
				return sshClient.DialContext(ctx, "tcp", lumeAPIAddr)
			},
			ForceAttemptHTTP2: false,
			IdleConnTimeout:   sshDialTimeout,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return client, nil
}

// Close closes the SSH tunnel and removes generated SSH config files.
func (c *Client) Close() error {
	if c == nil {
		return nil
	}
	if !c.closed.CompareAndSwap(false, true) {
		return nil
	}
	var errs []error
	if c.http != nil {
		c.http.CloseIdleConnections()
	}
	if c.ssh != nil {
		errs = append(errs, c.ssh.Close())
	}
	if c.tmpDir != "" {
		errs = append(errs, os.RemoveAll(c.tmpDir))
	}
	return errors.Join(errs...)
}

func (c *Client) api(ctx context.Context, method, path string, request any, result any) error {
	if err := c.errClosed(); err != nil {
		return err
	}
	if path == "" {
		return errors.New("lume api path is required")
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}

	var body io.Reader
	if request != nil {
		payload, err := json.Marshal(request)
		if err != nil {
			return fmt.Errorf("lume api %s %s: encode: %w", method, path, err)
		}
		body = bytes.NewReader(payload)
	}

	req, err := http.NewRequestWithContext(ctx, method, lumeAPIBase+path, body)
	if err != nil {
		return fmt.Errorf("lume api %s %s: %w", method, path, err)
	}
	if request != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if unavailable(err) {
			return fmt.Errorf("lume api %s %s: %w", method, path, compute.ErrUnavailable)
		}
		return fmt.Errorf("lume api %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	payload, err := io.ReadAll(io.LimitReader(resp.Body, apiBodyLimit+1))
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("lume api %s %s: read: %w", method, path, err)
	}
	if int64(len(payload)) > apiBodyLimit {
		return fmt.Errorf("lume api %s %s: response too large", method, path)
	}

	return decodeAPIResponse(method, path, resp.StatusCode, payload, result)
}

func decodeAPIResponse(method, path string, status int, payload []byte, result any) error {
	if status < http.StatusOK || status >= http.StatusMultipleChoices {
		return apiStatusError(method, path, status, payload)
	}
	if result == nil || len(bytes.TrimSpace(payload)) == 0 {
		return nil
	}
	if err := json.Unmarshal(payload, result); err != nil {
		return fmt.Errorf("lume api %s %s: decode: %w", method, path, err)
	}
	return nil
}

func (c *Client) host(ctx context.Context, script string, stdin io.Reader) ([]byte, error) {
	if err := c.errClosed(); err != nil {
		return nil, err
	}
	if script == "" {
		return nil, errors.New("host script is required")
	}

	session, err := c.ssh.NewSession()
	if err != nil {
		if unavailable(err) {
			return nil, fmt.Errorf("%w: host session: %w", compute.ErrUnavailable, err)
		}
		return nil, fmt.Errorf("host session: %w", err)
	}
	defer session.Close()

	if stdin != nil {
		session.Stdin = stdin
	}
	stdout := &capWriter{limit: hostOutputLimit}
	stderr := &capWriter{limit: hostOutputLimit}
	session.Stdout = stdout
	session.Stderr = stderr

	runDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = session.Signal(ssh.SIGKILL)
			_ = session.Close()
		case <-runDone:
		}
	}()
	err = session.Run(script)
	close(runDone)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if stdout.overflow || stderr.overflow {
		return nil, errors.New("host command output exceeds limit")
	}
	if err != nil {
		msg := strings.TrimSpace(stderr.buf.String())
		if msg != "" {
			return stdout.buf.Bytes(), fmt.Errorf("host command: %w: %s", err, truncate(msg))
		}
		return stdout.buf.Bytes(), fmt.Errorf("host command: %w", err)
	}
	return stdout.buf.Bytes(), nil
}

func quote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

func (c *Client) guestKey(image string) (string, error) {
	if image == "" {
		return "", errors.New("guest image is required")
	}
	path, ok := c.opts.GuestKeys[image]
	if !ok || path == "" {
		return "", fmt.Errorf("no guest key for image %q", image)
	}
	return path, nil
}

func (c *Client) writeGuestConfig(address, seed, image string) (string, func(), error) {
	if c.tmpDir == "" {
		return "", nil, errors.New("ssh config directory is not initialized")
	}
	if address == "" {
		return "", nil, errors.New("guest address is required")
	}
	if seed == "" {
		return "", nil, errors.New("guest seed is required")
	}
	if strings.ContainsAny(seed, "\n\r") {
		return "", nil, errors.New("guest seed is invalid")
	}
	key, err := c.guestKey(image)
	if err != nil {
		return "", nil, err
	}
	if _, err = os.Stat(key); err != nil {
		return "", nil, fmt.Errorf("guest key for image %q: %w", image, err)
	}

	hostName, hostPort := splitHostPort(c.opts.Host)
	guestName, guestPort := splitHostPort(address)
	content := guestSSHConfig(sshConfigParams{
		hostName:            hostName,
		hostPort:            hostPort,
		identityFile:        c.opts.IdentityFile,
		knownHostsFile:      c.opts.KnownHostsFile,
		guestName:           guestName,
		guestPort:           guestPort,
		guestKey:            key,
		guestKnownHostsFile: c.opts.GuestKnownHostsFile,
		seed:                seed,
	})

	file, err := os.CreateTemp(c.tmpDir, "ssh-*.conf")
	if err != nil {
		return "", nil, fmt.Errorf("create ssh config: %w", err)
	}
	path := file.Name()
	cleanup := func() { _ = os.Remove(path) }
	if _, err := file.WriteString(content); err != nil {
		_ = file.Close()
		cleanup()
		return "", nil, fmt.Errorf("write ssh config: %w", err)
	}
	if err := file.Close(); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("close ssh config: %w", err)
	}
	return path, cleanup, nil
}

func (c *Client) guestCmd(
	ctx context.Context,
	bin, address, seed, image string,
	args ...string,
) (*exec.Cmd, func(), error) {
	if err := c.errClosed(); err != nil {
		return nil, nil, err
	}
	if bin == "" {
		return nil, nil, errors.New("guest ssh binary is not available")
	}
	cfg, cleanup, err := c.writeGuestConfig(address, seed, image)
	if err != nil {
		return nil, nil, err
	}
	cmdArgs := append([]string{"-F", cfg}, args...)
	cmd := commandWithCancel(ctx, bin, cmdArgs...)
	cmd.Env = sshCleanEnv()
	return cmd, cleanup, nil
}

func (c *Client) errClosed() error {
	if c == nil || c.closed.Load() {
		return errors.New("lume client is closed")
	}
	return nil
}

type sshConfigParams struct {
	hostName            string
	hostPort            string
	identityFile        string
	knownHostsFile      string
	guestName           string
	guestPort           string
	guestKey            string
	guestKnownHostsFile string
	seed                string
}

func guestSSHConfig(p sshConfigParams) string {
	var b strings.Builder
	writeHost := func(alias, hostName, port, user, identity, knownHosts, hostKeyAlias, proxyJump string) {
		fmt.Fprintf(&b, "Host %s\n", alias)
		fmt.Fprintf(&b, "  HostName %s\n", sshConfigQuote(hostName))
		fmt.Fprintf(&b, "  Port %s\n", sshConfigQuote(port))
		fmt.Fprintf(&b, "  User %s\n", sshConfigQuote(user))
		fmt.Fprintf(&b, "  IdentityFile %s\n", sshConfigQuote(identity))
		fmt.Fprintf(&b, "  UserKnownHostsFile %s\n", sshConfigQuote(knownHosts))
		if hostKeyAlias != "" {
			fmt.Fprintf(&b, "  HostKeyAlias %s\n", sshConfigQuote(hostKeyAlias))
		}
		if proxyJump != "" {
			fmt.Fprintf(&b, "  ProxyJump %s\n", proxyJump)
		}
		b.WriteString("  IdentitiesOnly yes\n")
		b.WriteString("  IdentityAgent none\n")
		b.WriteString("  GlobalKnownHostsFile /dev/null\n")
		b.WriteString("  StrictHostKeyChecking yes\n")
		b.WriteString("  UpdateHostKeys no\n")
		b.WriteString("  HashKnownHosts no\n")
		b.WriteString("  ForwardAgent no\n")
		b.WriteString("  ForwardX11 no\n")
		b.WriteString("  BatchMode yes\n")
		b.WriteString("  PasswordAuthentication no\n")
		b.WriteString("  KbdInteractiveAuthentication no\n")
		b.WriteString("  PreferredAuthentications publickey\n")
		b.WriteString("  RequestTTY no\n")
		b.WriteString("  AddKeysToAgent no\n")
		b.WriteString("  ClearAllForwardings yes\n")
		b.WriteString("  CanonicalizeHostname no\n")
		b.WriteByte('\n')
	}
	writeHost(jumpHostAlias, p.hostName, p.hostPort, hostUser, p.identityFile, p.knownHostsFile, "", "")
	writeHost(
		guestHostAlias,
		p.guestName,
		p.guestPort,
		guestUser,
		p.guestKey,
		p.guestKnownHostsFile,
		p.seed,
		jumpHostAlias,
	)
	return b.String()
}

func sshConfigQuote(s string) string {
	return `"` + strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), `"`, `\"`) + `"`
}

func cleanOptions(opts Options) (Options, error) {
	identity, err := filepath.Abs(opts.IdentityFile)
	if err != nil {
		return Options{}, fmt.Errorf("identity file: %w", err)
	}
	knownHosts, err := filepath.Abs(opts.KnownHostsFile)
	if err != nil {
		return Options{}, fmt.Errorf("known hosts file: %w", err)
	}
	guestKnownHosts, err := filepath.Abs(opts.GuestKnownHostsFile)
	if err != nil {
		return Options{}, fmt.Errorf("guest known hosts file: %w", err)
	}
	keys := make(map[string]string, len(opts.GuestKeys))
	for name, path := range opts.GuestKeys {
		if path == "" {
			continue
		}
		abs, err := filepath.Abs(path)
		if err != nil {
			return Options{}, fmt.Errorf("guest key %q: %w", name, err)
		}
		keys[name] = abs
	}
	opts.IdentityFile = identity
	opts.KnownHostsFile = knownHosts
	opts.GuestKnownHostsFile = guestKnownHosts
	opts.GuestKeys = keys
	return opts, nil
}

func sshDialAddr(host string) string {
	if _, _, err := net.SplitHostPort(host); err == nil {
		return host
	}
	return net.JoinHostPort(host, defaultSSHPort)
}

func splitHostPort(host string) (string, string) {
	if h, p, err := net.SplitHostPort(host); err == nil {
		return h, p
	}
	return host, defaultSSHPort
}

func apiStatusError(method, path string, status int, payload []byte) error {
	msg := apiErrorMessage(payload)
	switch status {
	case http.StatusNotFound:
		if msg == "" {
			return fmt.Errorf("lume api %s %s: %w", method, path, compute.ErrNotFound)
		}
		return fmt.Errorf("lume api %s %s: %w: %s", method, path, compute.ErrNotFound, msg)
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		if msg == "" {
			return fmt.Errorf("lume api %s %s: %w", method, path, compute.ErrUnavailable)
		}
		return fmt.Errorf("lume api %s %s: %w: %s", method, path, compute.ErrUnavailable, msg)
	default:
		if msg == "" {
			return fmt.Errorf("lume api %s %s: %d", method, path, status)
		}
		return fmt.Errorf("lume api %s %s: %d %s", method, path, status, msg)
	}
}

func apiErrorMessage(payload []byte) string {
	payload = bytes.TrimSpace(payload)
	if len(payload) == 0 {
		return ""
	}
	var body struct {
		Message string `json:"message"`
		Error   string `json:"error"`
	}
	if json.Unmarshal(payload, &body) == nil {
		if body.Message != "" {
			return truncate(body.Message)
		}
		if body.Error != "" {
			return truncate(body.Error)
		}
	}
	return truncate(string(payload))
}

func unavailable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, compute.ErrUnavailable) {
		return true
	}
	if _, ok := errors.AsType[net.Error](err); ok {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "connection reset") ||
		strings.Contains(msg, "no such host") ||
		strings.Contains(msg, "i/o timeout") ||
		strings.Contains(msg, "network is unreachable") ||
		strings.Contains(msg, "unable to connect")
}

func truncate(s string) string {
	const diagnosticLimit = 512
	if len(s) <= diagnosticLimit {
		return s
	}
	return s[:diagnosticLimit]
}

func sshCleanEnv() []string {
	env := os.Environ()
	out := make([]string, 0, len(env))
	for _, entry := range env {
		key, _, _ := strings.Cut(entry, "=")
		switch key {
		case "SSH_AUTH_SOCK", "SSH_AGENT_PID", "SSH_ASKPASS", "DISPLAY":
			continue
		}
		out = append(out, entry)
	}
	return out
}

type capWriter struct {
	buf      bytes.Buffer
	limit    int
	overflow bool
}

func (w *capWriter) Write(p []byte) (int, error) {
	if w.overflow {
		return len(p), nil
	}
	remain := w.limit - w.buf.Len()
	if remain <= 0 {
		w.overflow = true
		return len(p), nil
	}
	if len(p) > remain {
		_, _ = w.buf.Write(p[:remain])
		w.overflow = true
		return len(p), nil
	}
	return w.buf.Write(p)
}
