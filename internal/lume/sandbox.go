package lume

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/meigma/codemode"

	"github.com/GilmanLab/agentcompute/internal/compute"
)

const (
	sidecarVersion = 1
	sidecarRoot    = "$HOME/.agentcompute"
	platformMac    = "mac"
	kindVM         = "vm"
	osMacOS        = "macos"
	guestNIC       = "en0"
	maxMacOSGuests = 2
)

var _ compute.Backend = (*Client)(nil)

type sandboxRecord struct {
	Version     int                        `json:"version"`
	Name        string                     `json:"name"`
	Platform    string                     `json:"platform"`
	Subject     string                     `json:"subject"`
	Host        string                     `json:"host"`
	NetworkKind string                     `json:"network_kind,omitempty"`
	CreatedAt   time.Time                  `json:"created_at"`
	ExpiresAt   time.Time                  `json:"expires_at"`
	Instances   map[string]*instanceRecord `json:"instances"`
}

type instanceRecord struct {
	VM        string                     `json:"vm"`
	Image     string                     `json:"image"`
	Seed      string                     `json:"seed"`
	Desktop   bool                       `json:"desktop"`
	CPUs      int64                      `json:"cpus"`
	MemoryMB  int64                      `json:"memory_mb"`
	DiskGB    int64                      `json:"disk_gb"`
	Prepared  bool                       `json:"prepared"`
	Snapshots map[string]*snapshotRecord `json:"snapshots"`
	Recovery  *recoveryRecord            `json:"recovery,omitempty"`
}

type snapshotRecord struct {
	VM        string    `json:"vm"`
	CreatedAt time.Time `json:"created_at"`
}

type recoveryRecord struct {
	PreviousVM string `json:"previous_vm,omitempty"`
	PendingVM  string `json:"pending_vm,omitempty"`
}

func agentError(message string) error {
	return &codemode.AgentError{Message: message}
}

func agentErrorf(format string, args ...any) error {
	return &codemode.AgentError{Message: fmt.Sprintf(format, args...)}
}

func unsupportedMac() error {
	return agentError("unsupported on platform mac")
}

func sidecarRel(name string) string {
	return "sandboxes/" + name + ".json"
}

func validSidecarName(name string) error {
	if name == "" || strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") {
		return fmt.Errorf("invalid sandbox name %q", name)
	}
	return nil
}

func (c *Client) writeJSON(ctx context.Context, rel string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	script := `set -eu
dest="$HOME/.agentcompute/"` + quote(rel) + `
mkdir -p "$(dirname "$dest")"
tmp=$(/usr/bin/mktemp "$(dirname "$dest")/.tmp.XXXXXX")
trap 'rm -f "$tmp"' EXIT
cat > "$tmp"
mv -f "$tmp" "$dest"
`
	_, err = c.host(ctx, script, bytes.NewReader(data))
	return err
}

func (c *Client) readJSON(ctx context.Context, rel string, v any) error {
	script := `set -eu
dest="$HOME/.agentcompute/"` + quote(rel) + `
if [ ! -f "$dest" ]; then exit 0; fi
cat "$dest"
`
	out, err := c.host(ctx, script, nil)
	if err != nil {
		return err
	}
	out = bytes.TrimSpace(out)
	if len(out) == 0 {
		return compute.ErrNotFound
	}
	return json.Unmarshal(out, v)
}

func (c *Client) removeJSON(ctx context.Context, rel string) error {
	script := `set -eu
dest="$HOME/.agentcompute/"` + quote(rel) + `
rm -f "$dest"
`
	_, err := c.host(ctx, script, nil)
	return err
}

func (c *Client) writeSandbox(ctx context.Context, rec *sandboxRecord) error {
	if err := validSidecarName(rec.Name); err != nil {
		return err
	}
	rec.Version = sidecarVersion
	if rec.Instances == nil {
		rec.Instances = map[string]*instanceRecord{}
	}
	return c.writeJSON(ctx, sidecarRel(rec.Name), rec)
}

func (c *Client) readSandbox(ctx context.Context, name string) (*sandboxRecord, error) {
	if err := validSidecarName(name); err != nil {
		return nil, err
	}
	var rec sandboxRecord
	if err := c.readJSON(ctx, sidecarRel(name), &rec); err != nil {
		return nil, err
	}
	if rec.Version != sidecarVersion {
		return nil, fmt.Errorf("sandbox %q sidecar version %d is unsupported", name, rec.Version)
	}
	if rec.Instances == nil {
		rec.Instances = map[string]*instanceRecord{}
	}
	return &rec, nil
}

func (c *Client) updateSandbox(ctx context.Context, name string, fn func(*sandboxRecord) error) error {
	rec, err := c.readSandbox(ctx, name)
	if err != nil {
		return err
	}
	if err := fn(rec); err != nil {
		return err
	}
	return c.writeSandbox(ctx, rec)
}

func (rec *sandboxRecord) sandbox() compute.Sandbox {
	platform := rec.Platform
	if platform == "" {
		platform = platformMac
	}
	return compute.Sandbox{
		Name:        rec.Name,
		Platform:    platform,
		Subject:     rec.Subject,
		Host:        rec.Host,
		NetworkKind: rec.NetworkKind,
		CreatedAt:   rec.CreatedAt,
		ExpiresAt:   rec.ExpiresAt,
	}
}

func (rec *sandboxRecord) instance(name string) (*instanceRecord, error) {
	inst, ok := rec.Instances[name]
	if !ok || inst == nil {
		return nil, compute.ErrNotFound
	}
	if inst.Snapshots == nil {
		inst.Snapshots = map[string]*snapshotRecord{}
	}
	return inst, nil
}

func (rec *sandboxRecord) mappedVMs() []string {
	names := make([]string, 0)
	seen := map[string]struct{}{}
	add := func(name string) {
		if name == "" {
			return
		}
		if _, ok := seen[name]; ok {
			return
		}
		seen[name] = struct{}{}
		names = append(names, name)
	}
	for _, inst := range rec.Instances {
		if inst == nil {
			continue
		}
		add(inst.VM)
		if inst.Recovery != nil {
			add(inst.Recovery.PreviousVM)
			add(inst.Recovery.PendingVM)
		}
		for _, snap := range inst.Snapshots {
			if snap != nil {
				add(snap.VM)
			}
		}
	}
	return names
}

func (rec *sandboxRecord) protected(name string) bool {
	if name == "" {
		return true
	}
	for _, inst := range rec.Instances {
		if inst != nil && inst.Seed == name {
			return true
		}
	}
	return false
}

// CreateSandbox persists a versioned sandbox sidecar under the agentcompute home.
func (c *Client) CreateSandbox(ctx context.Context, sandbox compute.Sandbox) error {
	if err := validSidecarName(sandbox.Name); err != nil {
		return err
	}
	if _, err := c.readSandbox(ctx, sandbox.Name); err == nil {
		return agentErrorf("sandbox %q already exists", sandbox.Name)
	} else if !errors.Is(err, compute.ErrNotFound) {
		return err
	}
	created := sandbox.CreatedAt.UTC()
	if created.IsZero() {
		created = time.Now().UTC()
	}
	expires := sandbox.ExpiresAt.UTC()
	host := sandbox.Host
	if host == "" {
		host = c.opts.Host
	}
	platform := sandbox.Platform
	if platform == "" {
		platform = platformMac
	}
	return c.writeSandbox(ctx, &sandboxRecord{
		Name:        sandbox.Name,
		Platform:    platform,
		Subject:     sandbox.Subject,
		Host:        host,
		NetworkKind: sandbox.NetworkKind,
		CreatedAt:   created,
		ExpiresAt:   expires,
		Instances:   map[string]*instanceRecord{},
	})
}

// ListSandboxes returns sandboxes from host sidecars.
func (c *Client) ListSandboxes(ctx context.Context) ([]compute.Sandbox, error) {
	script := `set -eu
dir=` + sidecarRoot + `/sandboxes
mkdir -p "$dir"
printf '['
first=1
for f in "$dir"/*.json; do
  [ -f "$f" ] || continue
  if [ "$first" -eq 1 ]; then
    first=0
  else
    printf ','
  fi
  cat "$f"
done
printf ']'
`
	out, err := c.host(ctx, script, nil)
	if err != nil {
		return nil, err
	}
	var recs []sandboxRecord
	if err := json.Unmarshal(out, &recs); err != nil {
		return nil, fmt.Errorf("list sandbox sidecars: %w", err)
	}
	outBoxes := make([]compute.Sandbox, 0, len(recs))
	for i := range recs {
		if recs[i].Version != sidecarVersion {
			continue
		}
		box := recs[i].sandbox()
		if box.Host == "" {
			box.Host = c.opts.Host
		}
		outBoxes = append(outBoxes, box)
	}
	return outBoxes, nil
}

// GetSandbox returns one sandbox sidecar.
func (c *Client) GetSandbox(ctx context.Context, name string) (compute.Sandbox, error) {
	rec, err := c.readSandbox(ctx, name)
	if err != nil {
		return compute.Sandbox{}, err
	}
	box := rec.sandbox()
	if box.Host == "" {
		box.Host = c.opts.Host
	}
	return box, nil
}

// ExtendSandbox writes a new expiry into the sandbox sidecar.
func (c *Client) ExtendSandbox(ctx context.Context, name string, expires time.Time) (compute.Sandbox, error) {
	err := c.updateSandbox(ctx, name, func(rec *sandboxRecord) error {
		rec.ExpiresAt = expires.UTC()
		return nil
	})
	if err != nil {
		return compute.Sandbox{}, err
	}
	return c.GetSandbox(ctx, name)
}

// DeleteSandbox deletes exact mapped VMs then the sidecar. Prefix deletes are never used.
func (c *Client) DeleteSandbox(ctx context.Context, name string) error {
	rec, err := c.readSandbox(ctx, name)
	if err != nil {
		return err
	}
	actual, err := c.listActualVMs(ctx)
	if err != nil {
		return err
	}
	var failed error
	for _, vmName := range rec.mappedVMs() {
		if rec.protected(vmName) {
			continue
		}
		if !actual[vmName] {
			continue
		}
		if err := c.deleteExactVM(ctx, vmName); err != nil {
			failed = errors.Join(failed, fmt.Errorf("delete vm %q: %w", vmName, err))
		}
	}
	if failed != nil {
		return failed
	}
	return c.removeJSON(ctx, sidecarRel(name))
}
