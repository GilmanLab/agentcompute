package incus

import (
	"bytes"
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/GilmanLab/agentcompute/internal/compute"
	"github.com/GilmanLab/agentcompute/internal/windowsexec"
)

func windowsGuest(osName string) bool {
	return strings.HasPrefix(strings.ToLower(osName), "windows")
}

func instanceRunningStatus(status string) bool {
	return strings.EqualFold(status, "Running") || strings.EqualFold(status, "Ready")
}

func (c *Client) startedInstance(ctx context.Context, ref compute.Ref) (compute.Instance, error) {
	inst, err := c.GetInstance(ctx, ref)
	if err != nil {
		return compute.Instance{}, err
	}
	if err := c.configureWindowsNICs(ctx, inst); err != nil {
		return compute.Instance{}, err
	}
	if !windowsGuest(inst.OS) {
		return inst, nil
	}
	return c.GetInstance(ctx, ref)
}

func (c *Client) configureWindowsNICs(ctx context.Context, inst compute.Instance) error {
	if !windowsGuest(inst.OS) || !instanceRunningStatus(inst.Status) {
		return nil
	}
	if _, err := c.WaitInstance(ctx, compute.WaitRequest{
		Ref:   inst.Ref,
		Until: compute.WaitUntilAgent,
	}); err != nil {
		return fmt.Errorf("wait for windows agent on %q: %w", inst.Ref.Name, err)
	}
	inst, err := c.GetInstance(ctx, inst.Ref)
	if err != nil {
		return err
	}
	egress := false
	for _, nic := range inst.NICs {
		nicEgress, err := c.configureWindowsNIC(ctx, inst.Ref, nic)
		if err != nil {
			return err
		}
		egress = egress || nicEgress
	}
	if !egress {
		return nil
	}
	return c.activateWindowsEvaluation(ctx, inst.Ref)
}

// activateWindowsEvaluation completes normal Microsoft activation for an
// unactivated evaluation edition. A fresh clone's own first-boot attempt runs
// before the NIC MTU matches the network, so it fails and leaves the guest
// unlicensed. Guests without an egress network are skipped; anything else is
// activated or reported.
func (c *Client) activateWindowsEvaluation(ctx context.Context, ref compute.Ref) error {
	var stdout, stderr bytes.Buffer
	code, err := c.Exec(ctx, compute.ExecRequest{
		Ref:  ref,
		Argv: windowsexec.PowerShell(windowsEvaluationActivationScript),
	}, &stdout, &stderr)
	if err != nil {
		return fmt.Errorf("activate windows evaluation on %q: %w", ref.Name, err)
	}
	if code != 0 {
		return fmt.Errorf(
			"activate windows evaluation on %q: exit %d: %s",
			ref.Name,
			code,
			strings.TrimSpace(stdout.String()+"\n"+stderr.String()),
		)
	}
	return nil
}

func (c *Client) configureWindowsNIC(ctx context.Context, ref compute.Ref, nic compute.NIC) (bool, error) {
	mac := normalizeMAC(nic.MAC)
	if mac == "" {
		return false, fmt.Errorf("windows nic %q has no MAC", nic.Name)
	}
	if nic.Network == "" {
		return false, fmt.Errorf("windows nic %q is not on an owned network", nic.Name)
	}
	network, err := c.ownedNetwork(ctx, ref.Sandbox, nic.Network)
	if err != nil {
		return false, fmt.Errorf("windows nic %q network %q: %w", nic.Name, nic.Network, err)
	}
	mtu, err := parseNetworkMTU(network.Config)
	if err != nil {
		return false, fmt.Errorf("windows nic %q network %q: %w", nic.Name, nic.Network, err)
	}
	var stdout, stderr bytes.Buffer
	code, err := c.Exec(ctx, compute.ExecRequest{
		Ref:  ref,
		Argv: windowsexec.PowerShell(windowsIPv4MTUScript(mac, mtu)),
	}, &stdout, &stderr)
	if err != nil {
		return false, fmt.Errorf("configure windows nic %q mtu %d: %w", nic.Name, mtu, err)
	}
	if code != 0 {
		return false, fmt.Errorf(
			"configure windows nic %q mtu %d: exit %d: %s",
			nic.Name,
			mtu,
			code,
			strings.TrimSpace(stdout.String()+"\n"+stderr.String()),
		)
	}
	return isTrue(network.Config[ipv4NATKey]), nil
}

// windowsEvaluationActivationScript activates unlicensed evaluation editions
// and leaves every other licensing state untouched.
const windowsEvaluationActivationScript = "$ErrorActionPreference='Stop';" +
	"$filter=\"ApplicationID='55c92734-d682-4d71-983e-d6ec3f16059f' AND PartialProductKey IS NOT NULL\";" +
	"function Pending { @(Get-CimInstance SoftwareLicensingProduct -Filter $filter |" +
	" Where-Object { $_.Description -like '*TIMEBASED_EVAL*' -and $_.LicenseStatus -ne 1 }) };" +
	"$pending=Pending;" +
	"if ($pending.Count -eq 0) { exit 0 };" +
	"foreach ($product in $pending) { $null=Invoke-CimMethod -InputObject $product -MethodName Activate };" +
	"$after=Pending;" +
	"if ($after.Count -ne 0) { throw \"evaluation activation left $($after[0].Name) in license status $($after[0].LicenseStatus)\" }"

func windowsIPv4MTUScript(mac string, mtu int) string {
	return "$ErrorActionPreference='Stop';" +
		"$want='" + mac + "';" +
		"$nics=@(Get-NetAdapter | Where-Object { (($_.MacAddress -replace '[^0-9A-Fa-f]','').ToUpper()) -eq $want });" +
		"if ($nics.Count -ne 1) { throw \"no unique adapter for MAC $want\" };" +
		"Set-NetIPInterface -InterfaceIndex $nics[0].ifIndex -AddressFamily IPv4 -NlMtuBytes " +
		strconv.Itoa(mtu) +
		" -ErrorAction Stop"
}

func normalizeMAC(mac string) string {
	var b strings.Builder
	for i := range mac {
		c := mac[i]
		switch {
		case c >= '0' && c <= '9', c >= 'A' && c <= 'F':
			b.WriteByte(c)
		case c >= 'a' && c <= 'f':
			b.WriteByte(c - 'a' + 'A')
		}
	}
	return b.String()
}
