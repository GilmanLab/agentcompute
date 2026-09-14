// Package incus implements the compute.Backend seam over the Incus API.
package incus

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	incusclient "github.com/lxc/incus/v7/client"
	"github.com/lxc/incus/v7/shared/api"
	"github.com/lxc/incus/v7/shared/cliconfig"

	"github.com/GilmanLab/agentcompute/internal/compute"
)

const (
	metaPrefix        = "user.agentcompute."
	metaVersion       = metaPrefix + "version"
	metaCreatedAt     = metaPrefix + "created_at"
	metaExpiresAt     = metaPrefix + "expires_at"
	metaSubject       = metaPrefix + "subject"
	metaHost          = metaPrefix + "host"
	metaSandbox       = metaPrefix + "sandbox"
	metaName          = metaPrefix + "name"
	metaImage         = metaPrefix + "image"
	metaDesktop       = metaPrefix + "desktop"
	metaNetworkPrefix = metaPrefix + "network."

	versionValue          = "1"
	projectPrefix         = "ac-"
	imagesRemoteName      = "images"
	defaultHost           = "lab01"
	defaultLogicalNetwork = "default"
	defaultNICName        = "eth0"
	rootDeviceName        = "root"
	platformIncus         = "incus"
	kindContainer         = "container"
	kindVM                = "vm"
	networkKindBridge     = "bridge"
	configTrue            = "true"
	configFalse           = "false"
	configNone            = "none"
	configManaged         = "managed"
	configBlock           = "block"
	deviceTypeKey         = "type"
	deviceTypeNIC         = "nic"
	deviceNetworkKey      = "network"
	ipv4AddressKey        = "ipv4.address"
	ipv4NATKey            = "ipv4.nat"
	ipv4DHCPKey           = "ipv4.dhcp"
	dnsModeKey            = "dns.mode"
	addressAuto           = "auto"
	featuresNetworksKey   = "features.networks"
	bytesPerMiB           = 1024 * 1024
	bytesPerGiB           = 1024 * bytesPerMiB

	execSignalKill     = 9
	execTeardownBound  = 5 * time.Second
	runningPoll        = 200 * time.Millisecond
	etagAttempts       = 8
	physicalNameTries  = 8
	physicalNameBytes  = 4
	physicalNamePrefix = "ac"
)

var _ compute.Backend = (*Client)(nil)

// Options configures an Incus API client.
type Options struct {
	// Remote is an existing Incus client remote name from the local config.
	Remote string

	// URL is an explicit daemon URL. When set it overrides Remote.
	URL string

	// ClientCert is a PEM document or path to the client certificate.
	ClientCert string

	// ClientKey is a PEM document or path to the client key.
	ClientKey string

	// ServerCert is a PEM document or path to the remote server certificate.
	ServerCert string

	// Host is the slice-1 cluster member that owns bridge-backed sandboxes.
	Host string

	// Pool is the storage pool used for explicit root disks.
	Pool string

	// OVNUplink is the default-project physical network for sandbox OVN networks.
	OVNUplink string

	// OVNRanges is the external subnet authorization on the physical uplink.
	OVNRanges string
}

// Client is the Incus adapter used by compute.Service.
type Client struct {
	server    incusclient.InstanceServer
	cfg       *cliconfig.Config
	host      string
	pool      string
	ovnUplink string
	ovnRanges string
}

// New connects to Incus and returns a Client.
func New(ctx context.Context, opts Options) (*Client, error) {
	if opts.Host == "" {
		opts.Host = defaultHost
	}
	if opts.Pool == "" {
		return nil, errors.New("incus pool is required")
	}
	if opts.OVNUplink == "" {
		opts.OVNUplink = defaultOVNUplink
	}
	if opts.OVNRanges == "" {
		opts.OVNRanges = defaultOVNRanges
	}

	cfg, err := cliconfig.LoadConfig("")
	if err != nil {
		return nil, fmt.Errorf("load incus client config: %w", err)
	}
	if cfg.Remotes == nil {
		cfg.Remotes = map[string]cliconfig.Remote{}
	}
	if _, ok := cfg.Remotes[imagesRemoteName]; !ok {
		cfg.Remotes[imagesRemoteName] = cliconfig.ImagesRemote
	}

	server, err := connectServer(ctx, opts, cfg)
	if err != nil {
		return nil, mapError(err)
	}

	return &Client{
		server:    server,
		cfg:       cfg,
		host:      opts.Host,
		pool:      opts.Pool,
		ovnUplink: opts.OVNUplink,
		ovnRanges: opts.OVNRanges,
	}, nil
}

func connectServer(ctx context.Context, opts Options, cfg *cliconfig.Config) (incusclient.InstanceServer, error) {
	if opts.URL == "" {
		name := opts.Remote
		if name == "" {
			name = cfg.DefaultRemote
		}
		return cfg.GetInstanceServer(name)
	}
	args, err := connectionArgs(opts)
	if err != nil {
		return nil, err
	}
	return incusclient.ConnectIncusWithContext(ctx, opts.URL, args)
}

// Close releases background Incus client resources.
func (c *Client) Close() error {
	if c == nil || c.server == nil {
		return nil
	}
	c.server.Disconnect()
	return nil
}

// Scoped returns a request-scoped Incus client.
//
// Incus WithContext mutates the receiver, so Scoped always clones with
// UseProject then UseTarget before WithContext.
func (c *Client) Scoped(ctx context.Context, project, target string) incusclient.InstanceServer {
	return requestContext(ctx, c.server.UseProject(project).UseTarget(target))
}

func requestContext(ctx context.Context, scoped incusclient.InstanceServer) incusclient.InstanceServer {
	protocol, ok := scoped.(*incusclient.ProtocolIncus)
	if !ok {
		panic("Incus SDK returned an unsupported client protocol")
	}
	return protocol.WithContext(ctx)
}

// RemoteImage returns an image server for an existing client remote.
//
// The default images simplestreams remote is ensured in memory and is not
// written back to the on-disk Incus config.
func (c *Client) RemoteImage(ctx context.Context, name string) (incusclient.ImageServer, error) {
	if c.cfg == nil {
		return nil, errors.New("incus client config is not loaded")
	}

	remote := strings.TrimSpace(name)
	if remote == "" {
		remote = imagesRemoteName
	}
	if parsed, _, err := c.cfg.ParseRemote(remote + ":"); err == nil && parsed != "" {
		remote = parsed
	} else if strings.Contains(remote, ":") {
		if parsed, _, err := c.cfg.ParseRemote(remote); err == nil && parsed != "" {
			remote = parsed
		}
	}

	server, err := c.cfg.GetImageServer(remote)
	if err != nil {
		return nil, mapError(err)
	}
	if inst, ok := server.(incusclient.InstanceServer); ok {
		return requestContext(ctx, inst.UseTarget("")), nil
	}
	return server, nil
}

func connectionArgs(opts Options) (*incusclient.ConnectionArgs, error) {
	cert, err := credential(opts.ClientCert)
	if err != nil {
		return nil, fmt.Errorf("client certificate: %w", err)
	}
	key, err := credential(opts.ClientKey)
	if err != nil {
		return nil, fmt.Errorf("client key: %w", err)
	}
	serverCert, err := credential(opts.ServerCert)
	if err != nil {
		return nil, fmt.Errorf("server certificate: %w", err)
	}
	return &incusclient.ConnectionArgs{
		TLSClientCert: cert,
		TLSClientKey:  key,
		TLSServerCert: serverCert,
	}, nil
}

func credential(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	if strings.Contains(value, "-----BEGIN") {
		return value, nil
	}
	body, err := os.ReadFile(value)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

func mapError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, compute.ErrNotFound) || errors.Is(err, compute.ErrUnavailable) {
		return err
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if api.StatusErrorCheck(err, http.StatusNotFound) {
		return compute.ErrNotFound
	}
	if api.StatusErrorCheck(err, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout) {
		return fmt.Errorf("%w", compute.ErrUnavailable)
	}
	if unavailable(err) {
		return fmt.Errorf("%w", compute.ErrUnavailable)
	}
	return err
}

func unavailable(err error) bool {
	if _, ok := errors.AsType[net.Error](err); ok {
		return true
	}
	if _, ok := errors.AsType[*url.Error](err); ok {
		return true
	}
	if _, ok := errors.AsType[*net.OpError](err); ok {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unable to connect") ||
		strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "connection reset") ||
		strings.Contains(msg, "no such host") ||
		strings.Contains(msg, "i/o timeout") ||
		strings.Contains(msg, "network is unreachable")
}

func isConflict(err error) bool {
	return api.StatusErrorCheck(err, http.StatusConflict)
}

func isPrecondition(err error) bool {
	return api.StatusErrorCheck(err, http.StatusPreconditionFailed)
}

func waitOp(ctx context.Context, op incusclient.Operation) error {
	if op == nil {
		return nil
	}
	return mapError(op.WaitContext(ctx))
}

func projectName(sandbox string) string {
	return projectPrefix + sandbox
}

func sandboxFromProject(name string) (string, bool) {
	if !strings.HasPrefix(name, projectPrefix) {
		return "", false
	}
	sandbox := strings.TrimPrefix(name, projectPrefix)
	if sandbox == "" {
		return "", false
	}
	return sandbox, true
}

func networkKey(logical string) string {
	return metaNetworkPrefix + logical
}

func parseTime(value string) (time.Time, error) {
	if value == "" {
		return time.Time{}, errors.New("empty timestamp")
	}
	if t, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return t, nil
	}
	return time.Parse(time.RFC3339, value)
}

func isTrue(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case configTrue, "1", "yes", "on":
		return true
	default:
		return false
	}
}

func (c *Client) members(ctx context.Context) ([]string, error) {
	if c.server == nil || !c.server.IsClustered() {
		return []string{c.host}, nil
	}
	members, err := c.Scoped(ctx, api.ProjectDefaultName, "").GetClusterMembers()
	if err != nil {
		return nil, mapError(err)
	}
	names := make([]string, 0, len(members))
	for _, member := range members {
		if member.ServerName != "" {
			names = append(names, member.ServerName)
		}
	}
	if len(names) == 0 {
		return []string{c.host}, nil
	}
	return names, nil
}

func (c *Client) getProject(ctx context.Context, sandbox string) (*api.Project, string, error) {
	project, etag, err := c.Scoped(ctx, "", "").GetProject(projectName(sandbox))
	if err != nil {
		return nil, "", mapError(err)
	}
	if project.Config[metaVersion] != versionValue {
		return nil, "", compute.ErrNotFound
	}
	return project, etag, nil
}

func (c *Client) patchProject(ctx context.Context, sandbox string, fn func(*api.Project)) error {
	name := projectName(sandbox)
	var last error
	for range etagAttempts {
		if err := ctx.Err(); err != nil {
			return err
		}
		project, etag, err := c.getProject(ctx, sandbox)
		if err != nil {
			return err
		}
		fn(project)
		err = c.Scoped(ctx, "", "").UpdateProject(name, project.Writable(), etag)
		if err == nil {
			return nil
		}
		if !isPrecondition(err) {
			return mapError(err)
		}
		last = err
	}
	if last == nil {
		last = fmt.Errorf("update project %s", name)
	}
	return mapError(last)
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func splitCSV(value string) []string {
	if value == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func joinCSV(values []string) string {
	return strings.Join(uniqueStrings(values), ",")
}

func randomPhysicalName() (string, error) {
	buf := make([]byte, physicalNameBytes)
	if _, err := io.ReadFull(rand.Reader, buf); err != nil {
		return "", err
	}
	return physicalNamePrefix + hex.EncodeToString(buf), nil
}

func parseSandbox(project api.Project) (compute.Sandbox, bool) {
	if project.Config[metaVersion] != versionValue {
		return compute.Sandbox{}, false
	}
	name, ok := sandboxFromProject(project.Name)
	if !ok {
		return compute.Sandbox{}, false
	}
	created, _ := parseTime(project.Config[metaCreatedAt])
	expires, _ := parseTime(project.Config[metaExpiresAt])
	networkKind := networkKindBridge
	if isTrue(project.Config[featuresNetworksKey]) {
		networkKind = networkKindOVN
	}
	return compute.Sandbox{
		Name:        name,
		Platform:    platformIncus,
		Subject:     project.Config[metaSubject],
		Host:        project.Config[metaHost],
		NetworkKind: networkKind,
		CreatedAt:   created,
		ExpiresAt:   expires,
	}, true
}

func reservedNetworks(config map[string]string) map[string]string {
	out := make(map[string]string)
	for key, value := range config {
		if !strings.HasPrefix(key, metaNetworkPrefix) || value == "" {
			continue
		}
		logical := strings.TrimPrefix(key, metaNetworkPrefix)
		if logical == "" {
			continue
		}
		out[logical] = value
	}
	return out
}
