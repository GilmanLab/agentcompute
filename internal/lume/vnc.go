package lume

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/GilmanLab/agentcompute/internal/compute"
)

const (
	// lumeBin is the account-local Lume this backend drives. The global
	// /usr/local/bin/lume is 0.5.3, which always starts a per-VM VNC server
	// bound to every interface and offers no way to turn it off, so the
	// backend never touches it.
	lumeBin = "/Users/agentcompute/bin/lume"

	// vncPolicyDisabled is the run-policy value that starts a guest with no
	// VNC listener, publishes no credentials, and reports a null vncUrl.
	vncPolicyDisabled = "disabled"

	// vncPinFile is the single source of installation and provenance details.
	vncPinFile = "pins/lume.yaml"

	// vncCLIOption is how the pinned build renders the run flag in help
	// output. 0.5.3 already ships --vnc-port and --vnc-password, so the value
	// placeholder is what separates the policy flag from those.
	vncCLIOption = "--vnc <vnc>"

	// vncConflictMarker is the pinned daemon's own rejection of a disabled
	// policy combined with a display, lowercased for matching. It comes from
	// VMError.vncDisabledConflict, a LocalizedError, so the HTTP body carries
	// this sentence rather than an opaque bridged description.
	vncConflictMarker = "vnc is disabled for this run"

	// vncProbeVM prefixes the throwaway VM name used by the daemon probe.
	vncProbeVM = "ac-vnc-policy-probe-"
)

// requireVNCDisableSupport fails closed unless both halves of the deployment
// understand the disabled VNC run policy: the account-local CLI this backend
// shells out to for inventory, and the lume serve daemon behind the tunnel
// that actually starts guests. Checking only the binary on disk would pass on
// a host where launchd still serves the old 0.5.3 daemon.
func (c *Client) requireVNCDisableSupport(ctx context.Context) error {
	if err := c.requireVNCCLIOption(ctx); err != nil {
		return err
	}
	inventory, err := c.lumeList(ctx)
	if err != nil {
		return err
	}
	return c.requireVNCPolicyEnforced(ctx, inventory)
}

// requireVNCCLIOption checks the installed executable's policy capability.
func (c *Client) requireVNCCLIOption(ctx context.Context) error {
	script := `set -eu
if [ ! -x ` + quote(lumeBin) + ` ]; then
  echo 'not installed or not executable' >&2
  exit 1
fi
` + lumeBin + ` run --help
`
	out, err := c.host(ctx, script, nil)
	if err != nil {
		// An unreachable host is an outage, not an unpinned install; saying
		// otherwise sends the operator to reinstall a working Lume.
		if errors.Is(err, compute.ErrUnavailable) {
			return err
		}
		return vncUnsupported(fmt.Sprintf("%s run --help failed: %v", lumeBin, err))
	}
	if !strings.Contains(string(out), vncCLIOption) {
		return vncUnsupported(fmt.Sprintf("%s run has no %q option", lumeBin, vncCLIOption))
	}
	return nil
}

// requireVNCPolicyEnforced proves the running daemon parses the run policy
// instead of dropping it.
//
// The probe asks for a disabled policy together with a display, which the
// pinned daemon rejects while decoding the request — before it looks up,
// creates, or starts anything. Lume 0.5.3 has no vnc field at all, silently
// drops the unknown key, and answers 202, which is exactly the silent
// fail-open this guards against. The probe name is random, verified absent
// from inventory, and colon-free so 0.5.3's image-reference auto-pull path
// cannot fire either; on that daemon the accepted request can only resolve to
// "virtual machine not found" in its background task.
func (c *Client) requireVNCPolicyEnforced(ctx context.Context, inventory []lumeVM) error {
	name, err := vncProbeName(inventory)
	if err != nil {
		return err
	}
	body := map[string]any{"noDisplay": false, "vnc": vncPolicyDisabled}
	status, payload, err := c.apiRaw(ctx, http.MethodPost, "/lume/vms/"+name+"/run", body)
	if err != nil {
		return err
	}
	message := apiErrorMessage(payload)
	if status == http.StatusBadRequest && strings.Contains(strings.ToLower(message), vncConflictMarker) {
		return nil
	}
	return vncUnsupported(fmt.Sprintf(
		"lume serve answered the %q run-policy probe with HTTP %d %s",
		vncPolicyDisabled, status, strings.TrimSpace(message)))
}

func vncProbeName(inventory []lumeVM) (string, error) {
	for range 16 {
		id, err := randomID()
		if err != nil {
			return "", err
		}
		name := vncProbeVM + id
		if _, exists := findVM(inventory, name); !exists {
			return name, nil
		}
	}
	return "", errors.New("unable to allocate a vnc run-policy probe name")
}

func vncUnsupported(detail string) error {
	return fmt.Errorf(
		"lume on this host cannot start guests with VNC disabled: %s; install the build pinned in %s as %s and run lume serve from that executable",
		detail,
		vncPinFile,
		lumeBin,
	)
}

// requireNoVNCListener stops a guest that came up with a VNC endpoint anyway.
// Lume's listener binds every interface, so a running guest reporting one is
// an exposure, not a cosmetic mismatch.
func (c *Client) requireNoVNCListener(ctx context.Context, name string, vm lumeVM) error {
	if vm.vnc() == "" {
		return nil
	}
	failure := agentErrorf(
		"vm %q started a VNC listener despite the %q run policy; it was stopped again",
		name, vncPolicyDisabled)
	if err := c.stopNamed(ctx, name); err != nil {
		return errors.Join(failure, fmt.Errorf("stop %q: %w", name, err))
	}
	return failure
}
