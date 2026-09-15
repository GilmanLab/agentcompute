package compute

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"net/netip"
	"strconv"
	"strings"
)

const (
	aclDirectionIngress = "ingress"
	aclDirectionEgress  = "egress"
	aclActionAllow      = "allow"
	aclActionDrop       = "drop"
	aclActionReject     = "reject"
	aclProtocolTCP      = "tcp"
	aclProtocolUDP      = "udp"
	aclProtocolICMP     = "icmp"

	// BaselineEgressMgmt is the immutable egress drop toward the management VLAN.
	BaselineEgressMgmt = "baseline-egress-mgmt"
	// BaselineEgressOOB is the immutable egress drop toward the OOB VLAN.
	BaselineEgressOOB = "baseline-egress-oob"

	// mappedIPv4PrefixBits is the bit offset of the embedded IPv4 address in an IPv4-mapped IPv6 prefix.
	mappedIPv4PrefixBits    = 96
	protectedIPv4PrefixBits = 24
	maxLossPercent          = 100
)

// ACLRule is one agent-facing network ACL entry.
type ACLRule struct {
	// ID is the agent-facing rule identifier returned by AddACLRule.
	ID string
	// Direction is ingress or egress.
	Direction string
	// Action is allow, drop, or reject.
	Action string
	// Protocol is tcp, udp, icmp, or empty for any.
	Protocol string
	// Src is an optional CIDR or address.
	Src string
	// Dst is an optional CIDR or address.
	Dst string
	// Port is an optional destination port or range.
	Port string
}

// Forward is a listen address allocated from the uplink OVN range.
type Forward struct {
	// Address is the listen address on the uplink range.
	Address string
	// Port is the listen port.
	Port int64
	// Protocol is tcp or udp.
	Protocol string
	// Network is the agent-facing network name.
	Network string
	// Instance is the target instance name.
	Instance string
}

// Impairment is in-guest tc netem configuration for one NIC.
type Impairment struct {
	// LatencyMS is added delay in milliseconds.
	LatencyMS int64
	// JitterMS is delay variation and requires LatencyMS.
	JitterMS int64
	// LossPercent is packet loss percent.
	LossPercent float64
	// RateMbit is a rate limit in Mbit/s.
	RateMbit int64
	// Clear removes existing impairment on the NIC.
	Clear bool
}

func sandboxNetworkKind(box Sandbox) string {
	if box.NetworkKind == "" || box.NetworkKind == kindBridge {
		return kindBridge
	}
	return box.NetworkKind
}

func isBridgeSandbox(box Sandbox) bool {
	return sandboxNetworkKind(box) == kindBridge
}

func isBaselineRule(id string) bool {
	return id == BaselineEgressMgmt || id == BaselineEgressOOB
}

func (s *Service) rejectIfMac(ctx context.Context, name string) error {
	if !s.hasMac || !validName(name) {
		return nil
	}
	box, err := s.backend.GetSandbox(ctx, name)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		return s.backendError(ctx, "get sandbox", err)
	}
	if box.Platform == platformMac {
		return unsupportedOnMac()
	}
	return nil
}

// ListNetworks returns agent-facing networks in a sandbox.
func (s *Service) ListNetworks(ctx context.Context, sandbox string) ([]Network, error) {
	if err := s.rejectIfMac(ctx, sandbox); err != nil {
		return nil, err
	}
	if err := validateName(sandbox); err != nil {
		return nil, err
	}
	if _, err := s.backend.GetSandbox(ctx, sandbox); err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, sandboxNotFound(sandbox)
		}
		return nil, s.backendError(ctx, "get sandbox", err)
	}
	networks, err := s.backend.ListNetworks(ctx, sandbox)
	if err != nil {
		return nil, s.backendError(ctx, "list networks", err)
	}
	return nonNil(networks), nil
}

// GetNetwork returns one agent-facing network.
func (s *Service) GetNetwork(ctx context.Context, sandbox, name string) (Network, error) {
	if err := s.rejectIfMac(ctx, sandbox); err != nil {
		return Network{}, err
	}
	if err := validateName(sandbox); err != nil {
		return Network{}, err
	}
	if name != reservedDefault {
		if err := validateName(name); err != nil {
			return Network{}, err
		}
	}
	if _, err := s.backend.GetSandbox(ctx, sandbox); err != nil {
		if errors.Is(err, ErrNotFound) {
			return Network{}, sandboxNotFound(sandbox)
		}
		return Network{}, s.backendError(ctx, "get sandbox", err)
	}
	return s.backendNetwork(ctx, sandbox, name)
}

func (s *Service) backendNetwork(ctx context.Context, sandbox, name string) (Network, error) {
	network, err := s.backend.GetNetwork(ctx, sandbox, name)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return Network{}, networkNotFound(name, sandbox)
		}
		return Network{}, s.backendError(ctx, "get network", err)
	}
	return network, nil
}

// DeleteNetwork deletes a network that has no attached NICs.
func (s *Service) DeleteNetwork(ctx context.Context, sandbox, name string) error {
	if err := s.rejectIfMac(ctx, sandbox); err != nil {
		return err
	}
	if name == reservedDefault {
		return agentErrorf("name %q is reserved", name)
	}
	if err := validateName(name); err != nil {
		return err
	}
	return s.withLiveSandbox(ctx, sandbox, func(Sandbox) error {
		instances, err := s.backend.ListInstances(ctx, sandbox)
		if err != nil {
			return s.backendError(ctx, "list instances", err)
		}
		for _, instance := range instances {
			for _, nic := range instance.NICs {
				if nic.Network == name {
					return agentErrorf("network %q still has attached NICs in sandbox %q", name, sandbox)
				}
			}
		}
		if err := s.backend.DeleteNetwork(ctx, sandbox, name); err != nil {
			if errors.Is(err, ErrNotFound) {
				return networkNotFound(name, sandbox)
			}
			return s.backendError(ctx, "delete network", err)
		}
		return nil
	})
}

// DetachNIC removes a NIC under the sandbox gate.
func (s *Service) DetachNIC(ctx context.Context, ref Ref, nic string) error {
	if err := s.rejectIfMac(ctx, ref.Sandbox); err != nil {
		return err
	}
	if err := validateRef(ref); err != nil {
		return err
	}
	if err := validateName(nic); err != nil {
		return err
	}
	return s.withLiveSandbox(ctx, ref.Sandbox, func(Sandbox) error {
		if err := s.backend.DetachNIC(ctx, ref, nic); err != nil {
			if errors.Is(err, ErrNotFound) {
				return agentErrorf("nic %q not found on instance %q in sandbox %q", nic, ref.Name, ref.Sandbox)
			}
			return s.backendError(ctx, "detach nic", err)
		}
		return nil
	})
}

// PeerNetworks routes between two OVN networks in a sandbox.
func (s *Service) PeerNetworks(ctx context.Context, sandbox, network, peer string) error {
	if err := s.rejectIfMac(ctx, sandbox); err != nil {
		return err
	}
	if err := validateNetworkName(network); err != nil {
		return err
	}
	if err := validateNetworkName(peer); err != nil {
		return err
	}
	if network == peer {
		return agentError("peer network must be different")
	}
	return s.withLiveSandbox(ctx, sandbox, func(box Sandbox) error {
		if isBridgeSandbox(box) {
			return agentError("network peering requires OVN networks")
		}
		left, err := s.backendNetwork(ctx, sandbox, network)
		if err != nil {
			return err
		}
		right, err := s.backendNetwork(ctx, sandbox, peer)
		if err != nil {
			return err
		}
		if left.Kind != kindOVN || right.Kind != kindOVN {
			return agentError("network peering requires OVN networks")
		}
		if err := s.backend.PeerNetworks(ctx, sandbox, network, peer); err != nil {
			return s.backendError(ctx, "peer networks", err)
		}
		return nil
	})
}

// AddACLRule appends a network-scoped rule without allowing baseline overrides.
func (s *Service) AddACLRule(ctx context.Context, sandbox, network string, rule ACLRule) (ACLRule, error) {
	if err := s.rejectIfMac(ctx, sandbox); err != nil {
		return ACLRule{}, err
	}
	if err := validateNetworkName(network); err != nil {
		return ACLRule{}, err
	}
	if err := validateACLRule(rule); err != nil {
		return ACLRule{}, err
	}
	var created ACLRule
	err := s.withLiveSandbox(ctx, sandbox, func(box Sandbox) error {
		if isBridgeSandbox(box) {
			return agentError("network ACLs require OVN networks")
		}
		if _, err := s.backend.GetNetwork(ctx, sandbox, network); err != nil {
			if errors.Is(err, ErrNotFound) {
				return networkNotFound(network, sandbox)
			}
			return s.backendError(ctx, "get network", err)
		}
		var addErr error
		created, addErr = s.backend.AddACLRule(ctx, sandbox, network, rule)
		if addErr != nil {
			if errors.Is(addErr, ErrNotFound) {
				return networkNotFound(network, sandbox)
			}
			return s.backendError(ctx, "add acl", addErr)
		}
		return nil
	})
	if err != nil {
		return ACLRule{}, err
	}
	return created, nil
}

// RemoveACLRule deletes an agent ACL rule; baseline IDs are not removable.
func (s *Service) RemoveACLRule(ctx context.Context, sandbox, network, rule string) error {
	if err := s.rejectIfMac(ctx, sandbox); err != nil {
		return err
	}
	if err := validateNetworkName(network); err != nil {
		return err
	}
	if rule == "" {
		return agentError("rule is required")
	}
	if isBaselineRule(rule) {
		return agentErrorf("rule %q is a baseline ACL and cannot be removed", rule)
	}
	return s.withLiveSandbox(ctx, sandbox, func(box Sandbox) error {
		if isBridgeSandbox(box) {
			return agentError("network ACLs require OVN networks")
		}
		if err := s.backend.RemoveACLRule(ctx, sandbox, network, rule); err != nil {
			if errors.Is(err, ErrNotFound) {
				return agentErrorf("rule %q not found on network %q in sandbox %q", rule, network, sandbox)
			}
			return s.backendError(ctx, "remove acl", err)
		}
		return nil
	})
}

// CreateForward exposes an instance port on a reused or newly allocated uplink address.
func (s *Service) CreateForward(
	ctx context.Context,
	sandbox, network string,
	ref Ref,
	port, listenPort int64,
	protocol string,
) (Forward, error) {
	if err := s.rejectIfMac(ctx, sandbox); err != nil {
		return Forward{}, err
	}
	ref, listenPort, protocol, err := prepareForward(sandbox, network, ref, port, listenPort, protocol)
	if err != nil {
		return Forward{}, err
	}
	var created Forward
	err = s.withLiveSandbox(ctx, sandbox, func(box Sandbox) error {
		var createErr error
		created, createErr = s.createPreparedForward(ctx, box, sandbox, network, ref, port, listenPort, protocol)
		return createErr
	})
	if err != nil {
		return Forward{}, err
	}
	return created, nil
}

// InstanceForward finds an existing scalar port forward to a guest.
// An empty Address means no matching forward; this method never exposes a port.
func (s *Service) InstanceForward(ctx context.Context, ref Ref, targetPort int64, protocol string) (Forward, error) {
	if err := s.rejectIfMac(ctx, ref.Sandbox); err != nil {
		return Forward{}, err
	}
	if err := validateRef(ref); err != nil {
		return Forward{}, err
	}
	if targetPort < 1 || targetPort > 65535 || (protocol != "tcp" && protocol != "udp") {
		return Forward{}, agentError("forward lookup requires a valid port and tcp or udp protocol")
	}
	if _, err := s.SandboxExpiry(ctx, ref.Sandbox); err != nil {
		return Forward{}, err
	}
	forward, err := s.backend.InstanceForward(ctx, ref, targetPort, protocol)
	if err != nil {
		return Forward{}, s.mapBackend(ctx, "find instance forward", err)
	}
	return forward, nil
}

// ImpairNIC applies Linux-only tc netem settings inside a guest. It is not gated.
func (s *Service) ImpairNIC(ctx context.Context, ref Ref, nic string, impairment Impairment) error {
	if err := validateRef(ref); err != nil {
		return err
	}
	if err := s.rejectIfMac(ctx, ref.Sandbox); err != nil {
		return err
	}
	if err := validateName(nic); err != nil {
		return err
	}
	if err := validateImpairment(impairment); err != nil {
		return err
	}
	inst, err := s.GetInstance(ctx, ref)
	if err != nil {
		return err
	}
	if inst.Status != statusRunning {
		return agentErrorf("instance %q in sandbox %q is not running", ref.Name, ref.Sandbox)
	}
	if !nicExists(inst, nic) {
		return agentErrorf("nic %q not found on instance %q in sandbox %q", nic, ref.Name, ref.Sandbox)
	}
	if err := s.requireLinuxGuest(ctx, inst); err != nil {
		return err
	}
	guestNIC := nic
	for _, attached := range inst.NICs {
		if attached.Name == nic && attached.GuestName != "" {
			guestNIC = attached.GuestName
			break
		}
	}
	if err := validateName(guestNIC); err != nil {
		return agentErrorf("unsupported guest NIC name %q", guestNIC)
	}
	script := impairCommand(guestNIC, impairment)
	stdout := newDrainingWriter(execOutputLimit)
	stderr := newDrainingWriter(execOutputLimit)
	code, execErr := s.backend.Exec(ctx, ExecRequest{
		Ref:  ref,
		Argv: []string{"sh", "-c", script},
	}, stdout, stderr)
	if execErr != nil {
		if errors.Is(execErr, context.Canceled) || errors.Is(execErr, context.DeadlineExceeded) {
			return execErr
		}
		return s.backendError(ctx, "impair", execErr)
	}
	if code != 0 {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			return agentErrorf("impair failed on nic %q of instance %q", nic, ref.Name)
		}
		return agentErrorf("impair failed on nic %q of instance %q: %s", nic, ref.Name, detail)
	}
	return nil
}

func prepareForward(
	sandbox, network string,
	ref Ref,
	port, listenPort int64,
	protocol string,
) (Ref, int64, string, error) {
	if err := validateNetworkName(network); err != nil {
		return Ref{}, 0, "", err
	}
	if ref.Sandbox == "" {
		ref.Sandbox = sandbox
	}
	if ref.Sandbox != sandbox {
		return Ref{}, 0, "", agentError("instance sandbox must match network sandbox")
	}
	if err := validateRef(ref); err != nil {
		return Ref{}, 0, "", err
	}
	if port < 1 || port > 65535 {
		return Ref{}, 0, "", agentError("port must be between 1 and 65535")
	}
	if listenPort == 0 {
		listenPort = port
	}
	if listenPort < 1 || listenPort > 65535 {
		return Ref{}, 0, "", agentError("listen_port must be between 1 and 65535")
	}
	if protocol == "" {
		protocol = aclProtocolTCP
	}
	if protocol != aclProtocolTCP && protocol != aclProtocolUDP {
		return Ref{}, 0, "", agentErrorf("protocol %q is not supported", protocol)
	}
	return ref, listenPort, protocol, nil
}

func (s *Service) createPreparedForward(
	ctx context.Context,
	box Sandbox,
	sandbox, network string,
	ref Ref,
	port, listenPort int64,
	protocol string,
) (Forward, error) {
	if isBridgeSandbox(box) {
		return Forward{}, agentError("port forwards require an OVN network")
	}
	netw, err := s.backend.GetNetwork(ctx, sandbox, network)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return Forward{}, networkNotFound(network, sandbox)
		}
		return Forward{}, s.backendError(ctx, "get network", err)
	}
	if netw.Kind != kindOVN {
		return Forward{}, agentError("port forwards require an OVN network")
	}
	if !netw.NAT {
		return Forward{}, agentError("net.forward requires a NAT-enabled network; nat=false networks have no uplink")
	}
	instance, err := s.backend.GetInstance(ctx, ref)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return Forward{}, instanceNotFound(ref)
		}
		return Forward{}, s.backendError(ctx, "get instance", err)
	}
	if !instanceHasNetwork(instance, network) {
		return Forward{}, agentErrorf("instance %q has no NIC on network %q", ref.Name, network)
	}
	created, err := s.backend.CreateForward(ctx, sandbox, network, ref, port, listenPort, protocol)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return Forward{}, networkNotFound(network, sandbox)
		}
		return Forward{}, s.backendError(ctx, "create forward", err)
	}
	return created, nil
}

func validateNetworkName(name string) error {
	if name == reservedDefault {
		return nil
	}
	return validateName(name)
}

func validateACLRule(rule ACLRule) error {
	switch rule.Direction {
	case aclDirectionIngress, aclDirectionEgress:
	default:
		return agentErrorf("direction %q is not supported", rule.Direction)
	}
	switch rule.Action {
	case aclActionAllow, aclActionDrop, aclActionReject:
	default:
		return agentErrorf("action %q is not supported", rule.Action)
	}
	if rule.Protocol != "" && rule.Protocol != aclProtocolTCP && rule.Protocol != aclProtocolUDP &&
		rule.Protocol != aclProtocolICMP {
		return agentErrorf("protocol %q is not supported", rule.Protocol)
	}
	if rule.Port != "" && rule.Protocol != aclProtocolTCP && rule.Protocol != aclProtocolUDP {
		return agentError("port requires tcp or udp")
	}
	if rule.Action == aclActionAllow && !excludesProtectedDestinations(rule.Dst) {
		return agentError("allow requires an explicit destination outside management and OOB ranges")
	}
	return nil
}

func excludesProtectedDestinations(destination string) bool {
	prefix, err := netip.ParsePrefix(destination)
	if err != nil {
		address, addressErr := netip.ParseAddr(destination)
		if addressErr != nil {
			return false
		}
		address = address.Unmap()
		prefix = netip.PrefixFrom(address, address.BitLen())
	}
	if prefix.Addr().Is4In6() {
		if prefix.Bits() < mappedIPv4PrefixBits {
			return false
		}
		prefix = netip.PrefixFrom(prefix.Addr().Unmap(), prefix.Bits()-mappedIPv4PrefixBits)
	}
	for _, protected := range protectedPrefixes() {
		if prefix.Overlaps(protected) {
			return false
		}
	}
	return true
}

func protectedPrefixes() [2]netip.Prefix {
	return [2]netip.Prefix{
		netip.PrefixFrom(netip.AddrFrom4([4]byte{10, 10, 10, 0}), protectedIPv4PrefixBits),
		netip.PrefixFrom(netip.AddrFrom4([4]byte{10, 10, 70, 0}), protectedIPv4PrefixBits),
	}
}

func validateCIDR(cidr string) error {
	if _, _, err := net.ParseCIDR(cidr); err != nil {
		return agentErrorf("invalid cidr %q", cidr)
	}
	return nil
}

func validateImpairment(impairment Impairment) error {
	if impairment.Clear {
		return nil
	}
	if impairment.LatencyMS < 0 || impairment.JitterMS < 0 || impairment.RateMbit < 0 || impairment.LossPercent < 0 {
		return agentError("impairment values must be non-negative")
	}
	if math.IsNaN(impairment.LossPercent) || impairment.LossPercent > maxLossPercent {
		return agentError("loss_percent must be between 0 and 100")
	}
	if impairment.JitterMS > 0 && impairment.LatencyMS == 0 {
		return agentError("jitter_ms requires latency_ms")
	}
	if impairment.LatencyMS == 0 && impairment.JitterMS == 0 && impairment.LossPercent == 0 &&
		impairment.RateMbit == 0 {
		return agentError("no impairment specified")
	}
	return nil
}

func instanceHasNetwork(instance Instance, network string) bool {
	for _, nic := range instance.NICs {
		if nic.Network == network {
			return true
		}
	}
	return false
}

func nicExists(instance Instance, nic string) bool {
	for _, attached := range instance.NICs {
		if attached.Name == nic {
			return true
		}
	}
	return false
}

func (s *Service) requireLinuxGuest(ctx context.Context, inst Instance) error {
	osName := inst.OS
	if osName == "" {
		if image, ok := s.catalog.Lookup(inst.Image); ok {
			osName = image.OS
		}
	}
	lowerOS := strings.ToLower(osName)
	if strings.HasPrefix(lowerOS, "windows") || lowerOS == "darwin" || lowerOS == "macos" {
		return agentErrorf("net.impair is not supported on %s guests", osName)
	}
	stdout := newDrainingWriter(execOutputLimit)
	stderr := newDrainingWriter(execOutputLimit)
	code, err := s.backend.Exec(ctx, ExecRequest{
		Ref:  inst.Ref,
		Argv: []string{"uname", "-s"},
	}, stdout, stderr)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		return agentErrorf("net.impair is not supported on non-Linux guests")
	}
	if code != 0 || !strings.EqualFold(strings.TrimSpace(stdout.String()), "Linux") {
		return agentErrorf("net.impair is not supported on non-Linux guests")
	}
	return nil
}

func impairCommand(nic string, impairment Impairment) string {
	if impairment.Clear {
		return fmt.Sprintf(
			"qdisc=$(tc qdisc show dev %s) || exit $?\n"+
				"case \"$qdisc\" in *\"qdisc netem 1: root\"*) tc qdisc del dev %s root ;; esac\n",
			nic, nic,
		)
	}
	return fmt.Sprintf("tc qdisc replace dev %s root handle 1: netem %s\n", nic, netemArgs(impairment))
}

func netemArgs(impairment Impairment) string {
	var parts []string
	if impairment.LatencyMS > 0 {
		if impairment.JitterMS > 0 {
			parts = append(parts, fmt.Sprintf("delay %dms %dms", impairment.LatencyMS, impairment.JitterMS))
		} else {
			parts = append(parts, fmt.Sprintf("delay %dms", impairment.LatencyMS))
		}
	}
	if impairment.LossPercent > 0 {
		parts = append(parts, fmt.Sprintf("loss %s%%", strconv.FormatFloat(impairment.LossPercent, 'f', -1, 64)))
	}
	if impairment.RateMbit > 0 {
		parts = append(parts, fmt.Sprintf("rate %dmbit", impairment.RateMbit))
	}
	return strings.Join(parts, " ")
}
