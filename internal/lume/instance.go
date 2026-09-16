package lume

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/GilmanLab/agentcompute/internal/compute"
)

const (
	stoppedAfterRun     = 12 * time.Second
	lifecyclePoll       = 500 * time.Millisecond
	errLimitOwned       = "macOS guest limit reached (2 per host); the owner's macOS VMs share this budget"
	errLimitOther       = "Apple's two-guest host-wide macOS VM limit is reached; another account is holding a slot"
	driverLaunchLabel   = "com.trycua.cua_driver_daemon"
	limitLogNeedle      = "The number of virtual machines exceeds the limit"
	permissionsJSONTool = "/usr/local/bin/cua-driver"
	bytesPerMiB         = 1 << 20
	bytesPerGiB         = 1 << 30
)

type lumeDisk struct {
	Allocated uint64 `json:"allocated"`
	Total     uint64 `json:"total"`
}

type lumeVM struct {
	Name       string   `json:"name"`
	OS         string   `json:"os"`
	CPUCount   int64    `json:"cpuCount"`
	MemorySize uint64   `json:"memorySize"`
	DiskSize   lumeDisk `json:"diskSize"`
	Status     string   `json:"status"`
	VNCURL     *string  `json:"vncUrl"`
	IPAddress  *string  `json:"ipAddress"`
}

type pendingInstance struct {
	client *Client
	ref    compute.Ref
	start  bool
}

type driverPermissions struct {
	Accessibility   bool `json:"accessibility"`
	ScreenRecording bool `json:"screen_recording"`
	Source          struct {
		Attribution string `json:"attribution"`
	} `json:"source"`
}

func instanceVMName(sandbox, name string) string {
	return "ac-" + sandbox + "-" + name
}

func snapshotVMName(sandbox, instance, snapshot, extra string) string {
	base := "ac-" + sandbox + "-" + instance + "-s-" + snapshot
	if extra == "" {
		return base
	}
	return base + "-" + extra
}

func macOSGuest(os string) bool {
	return strings.Contains(strings.ToLower(os), "mac")
}

func mapLumeStatus(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "running":
		return "Running"
	case "stopped":
		return "Stopped"
	default:
		return status
	}
}

func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, compute.ErrNotFound) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "not found") || strings.Contains(msg, "no such")
}

func countRunningMacOS(vms []lumeVM) int {
	n := 0
	for _, vm := range vms {
		if macOSGuest(vm.OS) && strings.EqualFold(vm.Status, "running") {
			n++
		}
	}
	return n
}

func rewriteVNCURL(raw, host string) string {
	if raw == "" || host == "" {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	hostname := u.Hostname()
	if hostname != "127.0.0.1" && hostname != "localhost" && hostname != "::1" {
		return raw
	}
	port := u.Port()
	if port != "" {
		u.Host = net.JoinHostPort(host, port)
	} else {
		u.Host = host
	}
	return u.String()
}

func (vm lumeVM) ip() string {
	if vm.IPAddress == nil {
		return ""
	}
	return strings.TrimSpace(*vm.IPAddress)
}

func (vm lumeVM) vnc() string {
	if vm.VNCURL == nil {
		return ""
	}
	return strings.TrimSpace(*vm.VNCURL)
}

func (vm lumeVM) running() bool {
	return strings.EqualFold(vm.Status, "running")
}

func (vm lumeVM) stopped() bool {
	return strings.EqualFold(vm.Status, "stopped")
}

func randomID() (string, error) {
	var buf [4]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf[:]), nil
}

func (c *Client) guestTarget(ctx context.Context, ref compute.Ref) (string, string, string, error) {
	if ref.Sandbox == "" || ref.Name == "" {
		return "", "", "", errors.New("instance reference is required")
	}
	rec, err := c.readSandbox(ctx, ref.Sandbox)
	if err != nil {
		return "", "", "", err
	}
	inst, err := rec.instance(ref.Name)
	if err != nil {
		return "", "", "", err
	}
	vm, err := c.getVM(ctx, inst.VM)
	if err != nil {
		return "", "", "", err
	}
	addr := vm.ip()
	if addr == "" {
		return "", "", "", fmt.Errorf("instance %q in sandbox %q has no NAT address", ref.Name, ref.Sandbox)
	}
	return addr, inst.Seed, inst.Image, nil
}

// Wait finishes guest readiness after clone (and run, when requested) have already completed.
func (p *pendingInstance) Wait(ctx context.Context) (compute.Instance, error) {
	if !p.start {
		return p.client.GetInstance(ctx, p.ref)
	}
	mapped, err := p.client.requireMappedInstance(ctx, p.ref)
	if err != nil {
		return compute.Instance{}, err
	}
	if err := p.client.waitAgentFacing(ctx, p.ref, mapped); err != nil {
		return compute.Instance{}, err
	}
	return p.client.GetInstance(ctx, p.ref)
}

// BeginCreateInstance clones the catalog seed, pins machineIdentifier, and records the mapping first.
func (c *Client) BeginCreateInstance(ctx context.Context, req compute.CreateInstance) (compute.PendingInstance, error) {
	if req.Ref.Sandbox == "" || req.Ref.Name == "" {
		return nil, errors.New("instance reference is required")
	}
	if err := validateResources(req); err != nil {
		return nil, err
	}
	rec, err := c.readSandbox(ctx, req.Ref.Sandbox)
	if err != nil {
		return nil, err
	}
	if _, existsErr := rec.instance(req.Ref.Name); existsErr == nil {
		return nil, agentErrorf("instance %q already exists in sandbox %q", req.Ref.Name, req.Ref.Sandbox)
	} else if !errors.Is(existsErr, compute.ErrNotFound) {
		return nil, existsErr
	}
	seed := strings.TrimSpace(req.Image.Seed)
	if seed == "" {
		return nil, agentErrorf("image %q has no Lume seed", req.Image.Name)
	}

	if req.Start {
		c.mu.Lock()
		err = c.cloneStartLocked(ctx, req, seed)
		c.mu.Unlock()
	} else {
		err = c.cloneStartLocked(ctx, req, seed)
	}
	if err != nil {
		return nil, err
	}
	return &pendingInstance{client: c, ref: req.Ref, start: req.Start}, nil
}

func (c *Client) cloneStartLocked(ctx context.Context, req compute.CreateInstance, seed string) error {
	inventory, err := c.lumeList(ctx)
	if err != nil {
		return err
	}
	if req.Start && countRunningMacOS(inventory) >= maxMacOSGuests {
		return agentError(errLimitOwned)
	}
	seedVM, ok := findVM(inventory, seed)
	if !ok {
		return agentErrorf("seed %q for image %q is not on the host", seed, req.Image.Name)
	}
	if err := rejectShrink(req, seedVM); err != nil {
		return err
	}
	vmName := instanceVMName(req.Ref.Sandbox, req.Ref.Name)
	if _, exists := findVM(inventory, vmName); exists {
		return agentErrorf("lume vm %q already exists", vmName)
	}

	inst := &instanceRecord{
		VM:        vmName,
		Image:     req.Image.Name,
		Seed:      seed,
		Desktop:   req.Image.Desktop,
		CPUs:      req.CPUs,
		MemoryMB:  req.MemoryMB,
		DiskGB:    req.DiskGB,
		Prepared:  false,
		Snapshots: map[string]*snapshotRecord{},
	}
	if err := c.updateSandbox(ctx, req.Ref.Sandbox, func(rec *sandboxRecord) error {
		if rec.Instances == nil {
			rec.Instances = map[string]*instanceRecord{}
		}
		rec.Instances[req.Ref.Name] = inst
		return nil
	}); err != nil {
		return err
	}

	if err := c.cloneAndPin(ctx, seed, vmName); err != nil {
		if delErr := c.deleteExactVM(ctx, vmName); delErr != nil && !errors.Is(delErr, compute.ErrNotFound) {
			return fmt.Errorf("clone %q: %w (clone retained as %q)", vmName, err, vmName)
		}
		_ = c.updateSandbox(ctx, req.Ref.Sandbox, func(rec *sandboxRecord) error {
			delete(rec.Instances, req.Ref.Name)
			return nil
		})
		return err
	}
	if err := c.applyResources(ctx, vmName, req, seedVM.DiskSize.Total); err != nil {
		return err
	}
	if err := c.markPrepared(ctx, req.Ref.Sandbox, req.Ref.Name); err != nil {
		return err
	}
	if !req.Start {
		return nil
	}
	return c.runLocked(ctx, vmName, inventory)
}

// ListInstances returns guests recorded for a sandbox that still exist on the host.
func (c *Client) ListInstances(ctx context.Context, sandbox string) ([]compute.Instance, error) {
	rec, err := c.readSandbox(ctx, sandbox)
	if err != nil {
		return nil, err
	}
	out := make([]compute.Instance, 0, len(rec.Instances))
	for name := range rec.Instances {
		inst, err := c.GetInstance(ctx, compute.Ref{Sandbox: sandbox, Name: name})
		if errors.Is(err, compute.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		out = append(out, inst)
	}
	slices.SortFunc(out, func(a, b compute.Instance) int {
		return strings.Compare(a.Ref.Name, b.Ref.Name)
	})
	return out, nil
}

// GetInstance returns observed Lume guest state for a mapped instance.
func (c *Client) GetInstance(ctx context.Context, ref compute.Ref) (compute.Instance, error) {
	if ref.Sandbox == "" || ref.Name == "" {
		return compute.Instance{}, errors.New("instance reference is required")
	}
	rec, err := c.readSandbox(ctx, ref.Sandbox)
	if err != nil {
		return compute.Instance{}, err
	}
	mapped, err := rec.instance(ref.Name)
	if err != nil {
		return compute.Instance{}, err
	}
	vm, err := c.getVM(ctx, mapped.VM)
	if err != nil {
		return compute.Instance{}, err
	}
	return c.mapInstance(ctx, ref, mapped, vm)
}

// DeleteInstance force-stops and deletes one guest and its snapshot VMs.
func (c *Client) DeleteInstance(ctx context.Context, ref compute.Ref) error {
	rec, err := c.readSandbox(ctx, ref.Sandbox)
	if err != nil {
		return err
	}
	mapped, err := rec.instance(ref.Name)
	if err != nil {
		return err
	}
	names := []string{mapped.VM}
	if mapped.Recovery != nil {
		names = append(names, mapped.Recovery.PreviousVM, mapped.Recovery.PendingVM)
	}
	for _, snap := range mapped.Snapshots {
		if snap != nil {
			names = append(names, snap.VM)
		}
	}
	actual, err := c.listActualVMs(ctx)
	if err != nil {
		return err
	}
	var failed error
	for _, name := range names {
		if name == "" || rec.protected(name) || !actual[name] {
			continue
		}
		if err := c.deleteExactVM(ctx, name); err != nil {
			failed = errors.Join(failed, fmt.Errorf("delete vm %q: %w", name, err))
		}
	}
	if failed != nil {
		return failed
	}
	return c.updateSandbox(ctx, ref.Sandbox, func(rec *sandboxRecord) error {
		delete(rec.Instances, ref.Name)
		return nil
	})
}

// StartInstance starts a guest, waits until SSH and the Driver are agent-facing, and returns Running.
func (c *Client) StartInstance(ctx context.Context, ref compute.Ref, _ bool) (compute.Instance, error) {
	mapped, err := c.requireMappedInstance(ctx, ref)
	if err != nil {
		return compute.Instance{}, err
	}
	c.mu.Lock()
	inventory, err := c.lumeList(ctx)
	if err != nil {
		c.mu.Unlock()
		return compute.Instance{}, err
	}
	if err = c.ensurePreparedLocked(ctx, ref, mapped, inventory); err != nil {
		c.mu.Unlock()
		return compute.Instance{}, err
	}
	err = c.runLocked(ctx, mapped.VM, inventory)
	c.mu.Unlock()
	if err != nil {
		return compute.Instance{}, err
	}
	if err := c.waitAgentFacing(ctx, ref, mapped); err != nil {
		return compute.Instance{}, err
	}
	return c.GetInstance(ctx, ref)
}

// StopInstance stops a guest and waits until it is stopped.
func (c *Client) StopInstance(ctx context.Context, ref compute.Ref, _ bool) (compute.Instance, error) {
	mapped, err := c.requireMappedInstance(ctx, ref)
	if err != nil {
		return compute.Instance{}, err
	}
	if err := c.stopVM(ctx, mapped.VM); err != nil {
		return compute.Instance{}, err
	}
	return c.GetInstance(ctx, ref)
}

// RestartInstance stops a guest if needed, then starts it with the same capacity rules as StartInstance.
func (c *Client) RestartInstance(ctx context.Context, ref compute.Ref, _ bool) (compute.Instance, error) {
	mapped, err := c.requireMappedInstance(ctx, ref)
	if err != nil {
		return compute.Instance{}, err
	}
	c.mu.Lock()
	inventory, err := c.lumeList(ctx)
	if err != nil {
		c.mu.Unlock()
		return compute.Instance{}, err
	}
	if err = c.ensurePreparedLocked(ctx, ref, mapped, inventory); err != nil {
		c.mu.Unlock()
		return compute.Instance{}, err
	}
	if vm, ok := findVM(inventory, mapped.VM); ok && vm.running() {
		if err = c.stopNamed(ctx, mapped.VM); err != nil {
			c.mu.Unlock()
			return compute.Instance{}, err
		}
		inventory = markStopped(inventory, mapped.VM)
	}
	err = c.runLocked(ctx, mapped.VM, inventory)
	c.mu.Unlock()
	if err != nil {
		return compute.Instance{}, err
	}
	if err := c.waitAgentFacing(ctx, ref, mapped); err != nil {
		return compute.Instance{}, err
	}
	return c.GetInstance(ctx, ref)
}

// WaitInstance polls until a readiness stage is reached.
func (c *Client) WaitInstance(ctx context.Context, req compute.WaitRequest) (compute.WaitResult, error) {
	mapped, err := c.requireMappedInstance(ctx, req.Ref)
	if err != nil {
		return compute.WaitResult{}, err
	}
	last := ""
	for {
		if err := ctx.Err(); err != nil {
			return compute.WaitResult{Status: last}, err
		}
		ok, status, waitErr := c.waitSatisfied(ctx, req, mapped)
		last = status
		if waitErr != nil {
			return compute.WaitResult{Status: status}, waitErr
		}
		if ok {
			return compute.WaitResult{Status: status}, nil
		}
		timer := time.NewTimer(lifecyclePoll)
		select {
		case <-ctx.Done():
			timer.Stop()
			return compute.WaitResult{Status: last}, ctx.Err()
		case <-timer.C:
		}
	}
}

// PublishInstance is unsupported on the Mac backend.
func (c *Client) PublishInstance(context.Context, compute.Ref, string) (string, error) {
	return "", unsupportedMac()
}

// GetSandboxImage is unsupported on the Mac backend.
func (c *Client) GetSandboxImage(context.Context, string, string) (compute.CatalogImage, error) {
	return compute.CatalogImage{}, unsupportedMac()
}

func (c *Client) waitSatisfied(
	ctx context.Context,
	req compute.WaitRequest,
	mapped *instanceRecord,
) (bool, string, error) {
	vm, err := c.getVM(ctx, mapped.VM)
	if err != nil {
		return false, "", err
	}
	status := mapLumeStatus(vm.Status)
	switch req.Until {
	case compute.WaitUntilRunning:
		return vm.running(), status, nil
	case compute.WaitUntilStopped:
		return vm.stopped(), status, nil
	case compute.WaitUntilNetwork:
		return vm.running() && vm.ip() != "", status, nil
	case compute.WaitUntilAgent:
		if !vm.running() || vm.ip() == "" {
			return false, status, nil
		}
		ok, execErr := c.guestReady(ctx, req.Ref)
		return ok, status, execErr
	case compute.WaitUntilDesktop:
		if !vm.running() || vm.ip() == "" {
			return false, status, nil
		}
		ready, execErr := c.guestReady(ctx, req.Ref)
		if execErr != nil || !ready {
			return false, status, execErr
		}
		ok, faceErr := c.agentFacing(ctx, req.Ref)
		return ok, status, faceErr
	default:
		return false, status, fmt.Errorf("unsupported wait stage %q", req.Until)
	}
}

func (c *Client) requireMappedInstance(ctx context.Context, ref compute.Ref) (*instanceRecord, error) {
	if ref.Sandbox == "" || ref.Name == "" {
		return nil, errors.New("instance reference is required")
	}
	rec, err := c.readSandbox(ctx, ref.Sandbox)
	if err != nil {
		return nil, err
	}
	return rec.instance(ref.Name)
}

func (c *Client) mapInstance(
	ctx context.Context,
	ref compute.Ref,
	mapped *instanceRecord,
	vm lumeVM,
) (compute.Instance, error) {
	snaps := make([]string, 0, len(mapped.Snapshots))
	for name := range mapped.Snapshots {
		snaps = append(snaps, name)
	}
	slices.Sort(snaps)
	nics := []compute.NIC{c.natNIC(ctx, mapped.VM, vm)}
	cpus := vm.CPUCount
	if cpus == 0 {
		cpus = mapped.CPUs
	}
	if vm.MemorySize > math.MaxInt64 || vm.DiskSize.Total > math.MaxInt64 {
		return compute.Instance{}, errors.New("lume VM capacity exceeds supported byte range")
	}
	memoryMB := int64(vm.MemorySize) / bytesPerMiB
	if memoryMB == 0 {
		memoryMB = mapped.MemoryMB
	}
	diskGB := mapped.DiskGB
	if diskGB == 0 && vm.DiskSize.Total > 0 {
		diskGB = int64(vm.DiskSize.Total) / bytesPerGiB
	}
	vnc := ""
	if vm.running() {
		vnc = rewriteVNCURL(vm.vnc(), c.opts.Host)
	}
	return compute.Instance{
		Ref:       ref,
		Image:     mapped.Image,
		OS:        osMacOS,
		Kind:      kindVM,
		Host:      c.opts.Host,
		Status:    mapLumeStatus(vm.Status),
		CPUs:      cpus,
		MemoryMB:  memoryMB,
		DiskGB:    diskGB,
		Desktop:   mapped.Desktop,
		NICs:      nics,
		Snapshots: snaps,
		VNCURL:    vnc,
	}, nil
}

func (c *Client) natNIC(ctx context.Context, vmName string, vm lumeVM) compute.NIC {
	addrs := []string{}
	if ip := vm.ip(); ip != "" {
		addrs = []string{ip}
	}
	return compute.NIC{
		Name:      guestNIC,
		GuestName: guestNIC,
		Network:   "default",
		MAC:       c.vmMAC(ctx, vmName),
		Addresses: addrs,
	}
}

func (c *Client) vmMAC(ctx context.Context, name string) string {
	script := `set -eu
/usr/bin/plutil -extract macAddress raw -o - "$HOME/.lume/"` + quote(name) + `/config.json 2>/dev/null || true
`
	out, err := c.host(ctx, script, nil)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func (c *Client) getVM(ctx context.Context, name string) (lumeVM, error) {
	var vm lumeVM
	err := c.api(ctx, http.MethodGet, "/lume/vms/"+name, nil, &vm)
	if err != nil {
		if isNotFound(err) {
			return lumeVM{}, compute.ErrNotFound
		}
		return lumeVM{}, err
	}
	if vm.Name == "" {
		vm.Name = name
	}
	return vm, nil
}

func (c *Client) lumeList(ctx context.Context) ([]lumeVM, error) {
	out, err := c.host(ctx, `set -eu
/usr/local/bin/lume ls --format json
`, nil)
	if err != nil {
		return nil, err
	}
	out = bytes.TrimSpace(out)
	if len(out) == 0 || string(out) == "[]" {
		return []lumeVM{}, nil
	}
	var vms []lumeVM
	if err := json.Unmarshal(out, &vms); err != nil {
		return nil, fmt.Errorf("lume ls: %w", err)
	}
	return vms, nil
}

func findVM(vms []lumeVM, name string) (lumeVM, bool) {
	for _, vm := range vms {
		if vm.Name == name {
			return vm, true
		}
	}
	return lumeVM{}, false
}

func markStopped(vms []lumeVM, name string) []lumeVM {
	out := slices.Clone(vms)
	for i := range out {
		if out[i].Name == name {
			out[i].Status = "stopped"
		}
	}
	return out
}

func validateResources(req compute.CreateInstance) error {
	if req.CPUs <= 0 || req.MemoryMB <= 0 || req.DiskGB <= 0 {
		return agentError("cpu, memory, and disk size must be positive")
	}
	return nil
}

func rejectShrink(req compute.CreateInstance, current lumeVM) error {
	if req.DiskGB <= 0 {
		return agentError("disk size must be positive")
	}
	wholeGB := current.DiskSize.Total / bytesPerGiB
	wantGB := uint64(req.DiskGB)
	if wantGB < wholeGB || (wantGB == wholeGB && current.DiskSize.Total%bytesPerGiB != 0) {
		return agentErrorf("disk size %dGiB would shrink the clone from %d bytes", req.DiskGB, current.DiskSize.Total)
	}
	return nil
}

func (c *Client) applyResources(
	ctx context.Context,
	vmName string,
	req compute.CreateInstance,
	diskBytes uint64,
) error {
	body := map[string]any{
		"cpu":    req.CPUs,
		"memory": fmt.Sprintf("%dMB", req.MemoryMB),
	}
	// Lume rejects equal-size disk requests as well as actual shrinking.
	if req.DiskGB > 0 && uint64(req.DiskGB) > diskBytes/bytesPerGiB {
		body["diskSize"] = fmt.Sprintf("%dGB", req.DiskGB)
	}
	return c.api(ctx, http.MethodPatch, "/lume/vms/"+vmName, body, nil)
}

func (c *Client) markPrepared(ctx context.Context, sandbox, name string) error {
	return c.updateSandbox(ctx, sandbox, func(rec *sandboxRecord) error {
		inst, err := rec.instance(name)
		if err != nil {
			return err
		}
		inst.Prepared = true
		return nil
	})
}

func (c *Client) ensurePreparedLocked(
	ctx context.Context,
	ref compute.Ref,
	mapped *instanceRecord,
	inventory []lumeVM,
) error {
	if mapped.Prepared {
		if _, ok := findVM(inventory, mapped.VM); !ok {
			return agentErrorf("instance %q is not prepared; delete it and recreate", ref.Name)
		}
		return nil
	}
	current, ok := findVM(inventory, mapped.VM)
	if !ok {
		return agentErrorf("instance %q is not prepared; delete it and recreate", ref.Name)
	}
	if mapped.Seed == "" {
		return agentErrorf("instance %q is not prepared; delete it and recreate", ref.Name)
	}
	req := compute.CreateInstance{CPUs: mapped.CPUs, MemoryMB: mapped.MemoryMB, DiskGB: mapped.DiskGB}
	if err := validateResources(req); err != nil {
		return err
	}
	if err := rejectShrink(req, current); err != nil {
		return err
	}
	if err := c.pinMachineIdentifier(ctx, mapped.VM, mapped.Seed); err != nil {
		return err
	}
	if err := c.applyResources(ctx, mapped.VM, req, current.DiskSize.Total); err != nil {
		return err
	}
	if err := c.markPrepared(ctx, ref.Sandbox, ref.Name); err != nil {
		return err
	}
	mapped.Prepared = true
	return nil
}

func (c *Client) listActualVMs(ctx context.Context) (map[string]bool, error) {
	vms, err := c.lumeList(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(vms))
	for _, vm := range vms {
		if vm.Name != "" {
			out[vm.Name] = true
		}
	}
	return out, nil
}

func (c *Client) uniqueVMName(ctx context.Context, base string) (string, error) {
	actual, err := c.listActualVMs(ctx)
	if err != nil {
		return "", err
	}
	if !actual[base] {
		return base, nil
	}
	for range 16 {
		id, err := randomID()
		if err != nil {
			return "", err
		}
		name := base + "-" + id
		if !actual[name] {
			return name, nil
		}
	}
	return "", fmt.Errorf("unable to allocate vm name %q", base)
}

func (c *Client) cloneAndPin(ctx context.Context, source, dest string) error {
	req := map[string]string{"name": source, "newName": dest}
	if err := c.api(ctx, http.MethodPost, "/lume/vms/clone", req, nil); err != nil {
		return err
	}
	return c.pinMachineIdentifier(ctx, dest, source)
}

func (c *Client) pinMachineIdentifier(ctx context.Context, cloneName, sourceName string) error {
	script := `set -eu
store="$HOME/.lume"
seed="$store"/` + quote(sourceName) + `/config.json
clone="$store"/` + quote(cloneName) + `/config.json
identifier=$(/usr/bin/plutil -extract machineIdentifier raw -o - "$seed")
tmp=$(/usr/bin/mktemp "$(dirname "$clone")/.config.XXXXXX")
trap 'rm -f "$tmp"' EXIT
cp "$clone" "$tmp"
/usr/bin/plutil -replace machineIdentifier -string "$identifier" "$tmp"
got=$(/usr/bin/plutil -extract machineIdentifier raw -o - "$tmp")
[ "$got" = "$identifier" ]
mv -f "$tmp" "$clone"
`
	_, err := c.host(ctx, script, nil)
	if err != nil {
		return fmt.Errorf("pin machineIdentifier on %s: %w", cloneName, err)
	}
	return nil
}

func (c *Client) deleteExactVM(ctx context.Context, name string) error {
	if name == "" {
		return nil
	}
	vm, err := c.getVM(ctx, name)
	if errors.Is(err, compute.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if vm.running() {
		if err := c.stopNamed(ctx, name); err != nil {
			return err
		}
	}
	if err := c.api(ctx, http.MethodDelete, "/lume/vms/"+name, nil, nil); err != nil && !isNotFound(err) {
		return err
	}
	return nil
}

func (c *Client) stopNamed(ctx context.Context, name string) error {
	if err := c.api(ctx, http.MethodPost, "/lume/vms/"+name+"/stop", nil, nil); err != nil {
		return err
	}
	return c.waitVMStatus(ctx, name, "stopped")
}

func (c *Client) stopVM(ctx context.Context, name string) error {
	vm, err := c.getVM(ctx, name)
	if err != nil {
		return err
	}
	if vm.stopped() {
		return nil
	}
	return c.stopNamed(ctx, name)
}

func (c *Client) waitVMStatus(ctx context.Context, name, want string) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		vm, err := c.getVM(ctx, name)
		if err != nil {
			return err
		}
		if strings.EqualFold(vm.Status, want) {
			return nil
		}
		timer := time.NewTimer(lifecyclePoll)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (c *Client) runLocked(ctx context.Context, name string, inventory []lumeVM) error {
	if vm, ok := findVM(inventory, name); ok && vm.running() {
		return nil
	}
	if countRunningMacOS(inventory) >= maxMacOSGuests {
		return agentError(errLimitOwned)
	}
	cursor, err := c.serveLogCursor(ctx)
	if err != nil {
		return err
	}
	body := map[string]any{"noDisplay": true}
	if err := c.api(ctx, http.MethodPost, "/lume/vms/"+name+"/run", body, nil); err != nil {
		return err
	}
	deadline := time.Now().Add(stoppedAfterRun)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		vm, err := c.getVM(ctx, name)
		if err != nil {
			return err
		}
		if vm.running() {
			return nil
		}
		if time.Now().After(deadline) {
			return c.stoppedRunError(ctx, name, cursor)
		}
		if err := sleepPoll(ctx); err != nil {
			return err
		}
	}
}

func (c *Client) stoppedRunError(ctx context.Context, name string, cursor serveLogCursor) error {
	found, err := c.limitInAppendedLogs(ctx, cursor)
	if err != nil {
		return fmt.Errorf("vm %q stayed stopped after run; lume-serve logs: %w", name, err)
	}
	if found {
		return agentError(errLimitOther)
	}
	return fmt.Errorf("vm %q stayed stopped after run", name)
}

type serveLogCursor struct {
	outSize int64
	errSize int64
}

func (c *Client) serveLogCursor(ctx context.Context) (serveLogCursor, error) {
	out, err := c.host(ctx, `set -eu
out="$HOME/lume-serve.out.log"
err="$HOME/lume-serve.err.log"
osize=-1
esize=-1
if [ -f "$out" ]; then osize=$(/usr/bin/stat -f%z "$out"); fi
if [ -f "$err" ]; then esize=$(/usr/bin/stat -f%z "$err"); fi
printf '%s %s\n' "$osize" "$esize"
`, nil)
	if err != nil {
		return serveLogCursor{}, fmt.Errorf("stat lume-serve logs: %w", err)
	}
	var cur serveLogCursor
	if _, err := fmt.Sscanf(strings.TrimSpace(string(out)), "%d %d", &cur.outSize, &cur.errSize); err != nil {
		return serveLogCursor{}, fmt.Errorf("parse lume-serve log sizes %q: %w", strings.TrimSpace(string(out)), err)
	}
	return cur, nil
}

func (c *Client) limitInAppendedLogs(ctx context.Context, cur serveLogCursor) (bool, error) {
	outData, err := c.readServeLogAppend(ctx, "out", cur.outSize)
	if err != nil {
		return false, err
	}
	errData, err := c.readServeLogAppend(ctx, "err", cur.errSize)
	if err != nil {
		return false, err
	}
	if cur.outSize < 0 && cur.errSize < 0 && len(outData) == 0 && len(errData) == 0 {
		return false, errors.New("lume-serve.out.log and lume-serve.err.log were absent")
	}
	return strings.Contains(string(outData)+string(errData), limitLogNeedle), nil
}

func (c *Client) readServeLogAppend(ctx context.Context, which string, offset int64) ([]byte, error) {
	if which != "out" && which != "err" {
		return nil, fmt.Errorf("unknown lume-serve log %q", which)
	}
	if offset < 0 {
		offset = 0
	}
	script := fmt.Sprintf(`set -eu
file="$HOME/lume-serve.%s.log"
offset=%d
if [ ! -f "$file" ]; then
  if [ "$offset" -le 0 ]; then exit 0; fi
  echo 'lume-serve %s log missing after run' >&2
  exit 1
fi
size=$(/usr/bin/stat -f%%z "$file")
if [ "$size" -lt "$offset" ]; then
  cat "$file"
  exit 0
fi
/usr/bin/tail -c +$((offset+1)) "$file"
`, which, offset, which)
	out, err := c.host(ctx, script, nil)
	if err != nil {
		return nil, fmt.Errorf("read lume-serve.%s.log: %w", which, err)
	}
	return out, nil
}

func (c *Client) waitAgentFacing(ctx context.Context, ref compute.Ref, mapped *instanceRecord) error {
	if err := pollReady(ctx, func() (bool, error) {
		vm, err := c.getVM(ctx, mapped.VM)
		return vm.running() && vm.ip() != "", err
	}); err != nil {
		return err
	}
	for _, probe := range []func(context.Context, compute.Ref) (bool, error){
		c.guestReady,
		c.desktopDomainReady,
		c.kickstartDriver,
		c.driverGrantsOK,
	} {
		if err := pollReady(ctx, func() (bool, error) { return probe(ctx, ref) }); err != nil {
			return err
		}
	}
	return nil
}

func pollReady(ctx context.Context, probe func() (bool, error)) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		ready, err := probe()
		if err != nil || ready {
			return err
		}
		if err := sleepPoll(ctx); err != nil {
			return err
		}
	}
}

func (c *Client) desktopDomainReady(ctx context.Context, ref compute.Ref) (bool, error) {
	ready, err := c.desktopSessionOK(ctx, ref)
	if err != nil || !ready {
		return false, err
	}
	return c.guiDomainOK(ctx, ref)
}

func (c *Client) kickstartDriver(ctx context.Context, ref compute.Ref) (bool, error) {
	code, err := c.guestScript(ctx, ref, "launchctl kickstart gui/$(id -u)/"+driverLaunchLabel)
	if err != nil {
		if isNotFound(err) || ctx.Err() != nil {
			return false, fmt.Errorf("kickstart driver: %w", err)
		}
		return false, nil
	}
	return code == 0, nil
}

func sleepPoll(ctx context.Context) error {
	timer := time.NewTimer(lifecyclePoll)
	select {
	case <-ctx.Done():
		timer.Stop()
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (c *Client) guiDomainOK(ctx context.Context, ref compute.Ref) (bool, error) {
	code, err := c.guestScript(ctx, ref, `launchctl print "gui/$(id -u)" >/dev/null`)
	if err != nil {
		if ctx.Err() != nil || isNotFound(err) {
			return false, err
		}
		return false, nil
	}
	return code == 0, nil
}

func (c *Client) guestReady(ctx context.Context, ref compute.Ref) (bool, error) {
	code, err := c.guestScript(ctx, ref, "/usr/bin/true")
	if err != nil {
		if ctx.Err() != nil {
			return false, err
		}
		if isNotFound(err) {
			return false, err
		}
		return false, nil
	}
	return code == 0, nil
}

func (c *Client) agentFacing(ctx context.Context, ref compute.Ref) (bool, error) {
	sessionOK, err := c.desktopSessionOK(ctx, ref)
	if err != nil || !sessionOK {
		return false, err
	}
	return c.driverGrantsOK(ctx, ref)
}

func (c *Client) desktopSessionOK(ctx context.Context, ref compute.Ref) (bool, error) {
	script := `[ "$(stat -f%Su /dev/console)" = "lume" ] || exit 1
pgrep -x Finder >/dev/null || exit 1
pgrep -x Dock >/dev/null || exit 1
if pgrep -f "Setup Assistant.app/Contents/MacOS" >/dev/null; then exit 1; fi
`
	code, err := c.guestScript(ctx, ref, script)
	if err != nil {
		if ctx.Err() != nil || isNotFound(err) {
			return false, err
		}
		return false, nil
	}
	return code == 0, nil
}

func (c *Client) driverGrantsOK(ctx context.Context, ref compute.Ref) (bool, error) {
	var stdout bytes.Buffer
	code, err := c.execCapture(ctx, ref, permissionsJSONTool+" permissions status --json", &stdout)
	if err != nil {
		if ctx.Err() != nil || isNotFound(err) {
			return false, err
		}
		return false, nil
	}
	if code != 0 {
		return false, nil
	}
	raw := extractJSON(stdout.Bytes())
	if len(raw) == 0 {
		return false, nil
	}
	var status driverPermissions
	valid := json.Unmarshal(raw, &status) == nil
	return valid && status.Accessibility && status.ScreenRecording && status.Source.Attribution == "driver-daemon", nil
}

func (c *Client) guestScript(ctx context.Context, ref compute.Ref, script string) (int64, error) {
	return c.execCapture(ctx, ref, script, io.Discard)
}

func (c *Client) execCapture(ctx context.Context, ref compute.Ref, script string, stdout io.Writer) (int64, error) {
	if stdout == nil {
		stdout = io.Discard
	}
	return c.Exec(ctx, compute.ExecRequest{
		Ref:  ref,
		Argv: []string{"/bin/bash", "-lc", script},
	}, stdout, io.Discard)
}

func extractJSON(raw []byte) []byte {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil
	}
	start := bytes.IndexByte(raw, '{')
	end := bytes.LastIndexByte(raw, '}')
	if start < 0 || end < start {
		return nil
	}
	return raw[start : end+1]
}
