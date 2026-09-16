package lume

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/GilmanLab/agentcompute/internal/compute"
)

const testGuestNAT = "192.168.64.12"

type hostScriptHandler func(script string, stdin io.Reader, stdout, stderr io.Writer) (handled bool, err error)

func newTunneledClient(t *testing.T, api http.Handler) *Client {
	t.Helper()
	return newTunneledClientWithHost(t, api, nil)
}

func newTunneledClientWithHost(
	t *testing.T,
	api http.Handler,
	host func(script string, stdin io.Reader, stdout, stderr io.Writer) (handled bool, err error),
) *Client {
	t.Helper()
	return startTunnel(t, tunnelOpts{API: api, Host: host}).client
}

type tunnelOpts struct {
	API          http.Handler
	Host         hostScriptHandler
	GuestDial    string
	GuestKeyFile string
	GuestHostKey ssh.PublicKey
	Seed         string
	Image        string
}

type tunnelState struct {
	client *Client
	home   string
}

func startTunnel(t *testing.T, opts tunnelOpts) *tunnelState {
	t.Helper()

	if opts.API == nil {
		opts.API = http.NotFoundHandler()
	}
	httpServer := httptest.NewServer(opts.API)
	t.Cleanup(httpServer.Close)

	home := filepath.Join(t.TempDir(), "home")
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".agentcompute"), 0o700))
	require.NoError(t, os.MkdirAll(filepath.Join(home, "tmp"), 0o700))

	_, hostPriv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	_, clientPriv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	hostSigner, err := ssh.NewSignerFromKey(hostPriv)
	require.NoError(t, err)
	clientSigner, err := ssh.NewSignerFromKey(clientPriv)
	require.NoError(t, err)

	jumpCfg := &ssh.ServerConfig{
		PublicKeyCallback: func(conn ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if conn.User() != hostUser {
				return nil, fmt.Errorf("user %q", conn.User())
			}
			if !bytes.Equal(key.Marshal(), clientSigner.PublicKey().Marshal()) {
				return nil, errors.New("unexpected key")
			}
			return &ssh.Permissions{}, nil
		},
	}
	jumpCfg.AddHostKey(hostSigner)
	// OpenSSH offers multiple host keys even when known_hosts pins only one.
	ecdsaKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	ecdsaSigner, err := ssh.NewSignerFromKey(ecdsaKey)
	require.NoError(t, err)
	jumpCfg.AddHostKey(ecdsaSigner)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	jump := &sshTestServer{
		cfg:      jumpCfg,
		httpAddr: httpServer.Listener.Addr().String(),
		natHost:  testGuestNAT,
		natPort:  22,
		natDial:  opts.GuestDial,
		home:     home,
		user:     hostUser,
		hostFn:   opts.Host,
	}
	var acceptWG sync.WaitGroup
	acceptWG.Go(func() {
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			go jump.handle(conn)
		}
	})
	t.Cleanup(func() {
		_ = listener.Close()
		acceptWG.Wait()
	})

	dir := t.TempDir()
	identity := filepath.Join(dir, "id_ed25519")
	known := filepath.Join(dir, "known_hosts")
	guestKnown := filepath.Join(dir, "guest_known_hosts")
	require.NoError(t, os.WriteFile(identity, marshalKey(t, clientPriv), 0o600))
	line := knownhosts.Line([]string{listener.Addr().String()}, hostSigner.PublicKey())
	require.NoError(t, os.WriteFile(known, []byte(line+"\n"), 0o600))
	guestKnownBody := []byte{}
	if opts.GuestHostKey != nil && opts.Seed != "" {
		guestKnownBody = []byte(knownhosts.Line([]string{opts.Seed}, opts.GuestHostKey) + "\n")
	}
	require.NoError(t, os.WriteFile(guestKnown, guestKnownBody, 0o600))

	keys := map[string]string{}
	if opts.Image != "" && opts.GuestKeyFile != "" {
		keys[opts.Image] = opts.GuestKeyFile
	}

	client, err := New(t.Context(), Options{
		Host:                listener.Addr().String(),
		IdentityFile:        identity,
		KnownHostsFile:      known,
		GuestKnownHostsFile: guestKnown,
		GuestKeys:           keys,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	return &tunnelState{client: client, home: home}
}

type sshTestServer struct {
	cfg      *ssh.ServerConfig
	httpAddr string
	natHost  string
	natPort  uint32
	natDial  string
	home     string
	user     string
	hostFn   hostScriptHandler
	sftp     string
}

func (s *sshTestServer) handle(conn net.Conn) {
	server, chans, reqs, err := ssh.NewServerConn(conn, s.cfg)
	if err != nil {
		_ = conn.Close()
		return
	}
	defer server.Close()
	go ssh.DiscardRequests(reqs)
	for newCh := range chans {
		switch newCh.ChannelType() {
		case "session":
			ch, requests, acceptErr := newCh.Accept()
			if acceptErr == nil {
				go s.session(ch, requests)
			}
		case "direct-tcpip":
			go s.forward(newCh)
		default:
			_ = newCh.Reject(ssh.UnknownChannelType, newCh.ChannelType())
		}
	}
}

func (s *sshTestServer) forward(newCh ssh.NewChannel) {
	var spec struct {
		Host     string
		Port     uint32
		OrigHost string
		OrigPort uint32
	}
	if err := ssh.Unmarshal(newCh.ExtraData(), &spec); err != nil {
		_ = newCh.Reject(ssh.ConnectionFailed, "bad extra")
		return
	}
	target := ""
	switch {
	case spec.Host == "127.0.0.1" && spec.Port == 7777:
		target = s.httpAddr
	case s.natDial != "" && spec.Host == s.natHost && spec.Port == s.natPort:
		target = s.natDial
	}
	if target == "" {
		_ = newCh.Reject(ssh.Prohibited, "denied")
		return
	}
	backend, err := net.Dial("tcp", target)
	if err != nil {
		_ = newCh.Reject(ssh.ConnectionFailed, err.Error())
		return
	}
	ch, requests, err := newCh.Accept()
	if err != nil {
		_ = backend.Close()
		return
	}
	go ssh.DiscardRequests(requests)
	proxyConn(ch, backend)
}

func (s *sshTestServer) session(ch ssh.Channel, requests <-chan *ssh.Request) {
	defer ch.Close()
	var cmd *exec.Cmd
	defer func() { _ = killProcessGroup(cmd) }()
	for req := range requests {
		if req.Type == "signal" {
			_ = killProcessGroup(cmd)
			if req.WantReply {
				_ = req.Reply(true, nil)
			}
			continue
		}
		if cmd != nil || (req.Type != "exec" && req.Type != "subsystem") {
			if req.WantReply {
				_ = req.Reply(false, nil)
			}
			continue
		}
		var finished bool
		cmd, finished = s.startCommand(ch, req)
		if finished {
			return
		}
	}
}

func (s *sshTestServer) startCommand(ch ssh.Channel, req *ssh.Request) (*exec.Cmd, bool) {
	var payload struct{ Value string }
	if err := ssh.Unmarshal(req.Payload, &payload); err != nil {
		_ = req.Reply(false, nil)
		return nil, false
	}
	var cmd *exec.Cmd
	switch req.Type {
	case "subsystem":
		if payload.Value != "sftp" || s.sftp == "" {
			_ = req.Reply(false, nil)
			return nil, false
		}
		_ = req.Reply(true, nil)
		cmd = exec.Command(s.sftp, "-e")
	default:
		_ = req.Reply(true, nil)
		if s.hostFn != nil {
			handled, err := s.hostFn(payload.Value, ch, ch, ch.Stderr())
			if handled {
				sendSSHExit(ch, err)
				return nil, true
			}
		}
		// Match macOS zsh's unmatched-glob failure without requiring zsh in CI.
		cmd = exec.Command("bash", "--noprofile", "--norc", "-O", "failglob", "-c", payload.Value)
	}
	cmd.Env = isolatedEnv(s.home, s.user)
	cmd.Dir = s.home
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = execTeardownBound
	cmd.Stdin = ch
	cmd.Stdout = ch
	cmd.Stderr = ch.Stderr()
	if err := cmd.Start(); err != nil {
		sendSSHExit(ch, err)
		return nil, true
	}
	go func() {
		sendSSHExit(ch, cmd.Wait())
		_ = ch.Close()
	}()
	return cmd, false
}

func sendSSHExit(ch ssh.Channel, err error) {
	status := uint32(0)
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() >= 0 {
			status = uint32(exitErr.ExitCode())
		} else {
			status = 1
		}
	}
	_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(&struct{ Status uint32 }{status}))
}

func isolatedEnv(home, user string) []string {
	if user == "" {
		user = hostUser
	}
	return []string{
		"HOME=" + home,
		"TMPDIR=" + filepath.Join(home, "tmp"),
		"PATH=/usr/bin:/bin:/usr/sbin:/sbin:/usr/local/bin",
		"LANG=C",
		"USER=" + user,
		"LOGNAME=" + user,
	}
}

func proxyConn(ch ssh.Channel, backend net.Conn) {
	defer ch.Close()
	defer backend.Close()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(backend, ch)
		if cw, ok := backend.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(ch, backend)
		_ = ch.CloseWrite()
	}()
	wg.Wait()
}

func marshalKey(t *testing.T, key ed25519.PrivateKey) []byte {
	t.Helper()
	block, err := ssh.MarshalPrivateKey(key, "")
	require.NoError(t, err)
	return pem.EncodeToMemory(block)
}

type guestTransport struct {
	client *Client
	root   string
	ref    compute.Ref
}

func newGuestTransport(t *testing.T) *guestTransport {
	t.Helper()

	sftpServer := sftpServerPath(t)
	guestRoot := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(guestRoot, "tmp"), 0o700))

	_, guestHostPriv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	_, guestUserPriv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	guestHostSigner, err := ssh.NewSignerFromKey(guestHostPriv)
	require.NoError(t, err)
	guestUserSigner, err := ssh.NewSignerFromKey(guestUserPriv)
	require.NoError(t, err)

	guestCfg := &ssh.ServerConfig{
		PublicKeyCallback: func(conn ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if conn.User() != guestUser {
				return nil, fmt.Errorf("user %q", conn.User())
			}
			if !bytes.Equal(key.Marshal(), guestUserSigner.PublicKey().Marshal()) {
				return nil, errors.New("unexpected guest key")
			}
			return &ssh.Permissions{}, nil
		},
	}
	guestCfg.AddHostKey(guestHostSigner)

	guestLn, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	guestSrv := &sshTestServer{
		cfg:  guestCfg,
		home: guestRoot,
		user: guestUser,
		sftp: sftpServer,
	}
	var guestWG sync.WaitGroup
	guestWG.Go(func() {
		for {
			conn, err := guestLn.Accept()
			if err != nil {
				return
			}
			go guestSrv.handle(conn)
		}
	})
	t.Cleanup(func() {
		_ = guestLn.Close()
		guestWG.Wait()
	})

	guestKeyFile := filepath.Join(t.TempDir(), "guest_ed25519")
	require.NoError(t, os.WriteFile(guestKeyFile, marshalKey(t, guestUserPriv), 0o600))

	ref := compute.Ref{Sandbox: "demo", Name: "web"}
	vmName := instanceVMName(ref.Sandbox, ref.Name)
	seed := "ac-seed-macos-tahoe-desktop"
	image := "macos-tahoe-desktop"
	ip := testGuestNAT
	api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/lume/vms/"+vmName {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(lumeVM{
				Name:      vmName,
				OS:        "macOS",
				Status:    "running",
				IPAddress: &ip,
			})
			return
		}
		http.NotFound(w, r)
	})

	state := startTunnel(t, tunnelOpts{
		API:          api,
		GuestDial:    guestLn.Addr().String(),
		GuestKeyFile: guestKeyFile,
		GuestHostKey: guestHostSigner.PublicKey(),
		Seed:         seed,
		Image:        image,
	})

	now := time.Now()
	rec := &sandboxRecord{
		Version:   sidecarVersion,
		Name:      ref.Sandbox,
		Platform:  platformMac,
		Host:      "mac.example",
		CreatedAt: now,
		ExpiresAt: now.Add(time.Hour),
		Instances: map[string]*instanceRecord{
			ref.Name: {
				VM:       vmName,
				Image:    image,
				Seed:     seed,
				Desktop:  true,
				CPUs:     4,
				MemoryMB: 8192,
				DiskGB:   100,
			},
		},
	}
	require.NoError(t, state.client.writeJSON(t.Context(), sidecarRel(ref.Sandbox), rec))

	return &guestTransport{client: state.client, root: guestRoot, ref: ref}
}

func sftpServerPath(t *testing.T) string {
	t.Helper()
	for _, path := range []string{
		"/usr/libexec/sftp-server",
		"/usr/libexec/openssh/sftp-server",
		"/usr/lib/openssh/sftp-server",
	} {
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}
	t.Fatal("sftp-server not installed")
	return ""
}
