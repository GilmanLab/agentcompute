package mcpserver

import (
	"math"
	"strings"
	"time"

	"github.com/GilmanLab/agentcompute/internal/compute"
)

const (
	maxTTLMinutes     = math.MaxInt64 / int64(time.Minute)
	maxTimeoutSeconds = math.MaxInt64 / int64(time.Second)
)

func formatTime(value time.Time) string {
	return value.UTC().Format(time.RFC3339)
}

func minutesToDuration(minutes int64) (time.Duration, error) {
	if minutes < 1 {
		return 0, agentError("ttl_minutes must be positive")
	}
	if minutes > maxTTLMinutes {
		return 0, agentError("ttl_minutes is too large")
	}
	return time.Duration(minutes) * time.Minute, nil
}

func secondsToDuration(seconds int64) (time.Duration, error) {
	if seconds < 0 {
		return 0, agentError("timeout_seconds must be non-negative")
	}
	if seconds > maxTimeoutSeconds {
		return 0, agentError("timeout_seconds is too large")
	}
	return time.Duration(seconds) * time.Second, nil
}

func parseEnv(raw string) (map[string]string, error) {
	env := make(map[string]string)
	for line := range strings.SplitSeq(raw, "\n") {
		line = strings.TrimSuffix(line, "\r")
		if line == "" {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok || key == "" {
			return nil, agentErrorf("env entry %q must be KEY=VALUE", line)
		}
		env[key] = value
	}
	return env, nil
}

func deref[T any](ptr *T, fallback T) T {
	if ptr == nil {
		return fallback
	}
	return *ptr
}

func nonNilStrings(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}

func nicAddresses(nics []compute.NIC) map[string][]string {
	out := make(map[string][]string, len(nics))
	for _, nic := range nics {
		out[nic.Name] = nonNilStrings(nic.Addresses)
	}
	return out
}

func networkDTO(network compute.Network) networkOut {
	return networkOut{
		Name:    network.Name,
		Kind:    network.Kind,
		CIDR:    network.CIDR,
		Gateway: network.Gateway,
	}
}

func networkDTOs(networks []compute.Network) []networkOut {
	out := make([]networkOut, 0, len(networks))
	for _, network := range networks {
		out = append(out, networkDTO(network))
	}
	return out
}

func nicDTO(nic compute.NIC) nicOut {
	return nicOut{
		Name:      nic.Name,
		Network:   nic.Network,
		MAC:       nic.MAC,
		Addresses: nonNilStrings(nic.Addresses),
	}
}

func nicDTOs(nics []compute.NIC) []nicOut {
	out := make([]nicOut, 0, len(nics))
	for _, nic := range nics {
		out = append(out, nicDTO(nic))
	}
	return out
}

func instanceListItemDTO(instance compute.Instance) instanceListItem {
	return instanceListItem{
		Name:      instance.Ref.Name,
		Kind:      instance.Kind,
		Image:     instance.Image,
		Status:    instance.Status,
		Addresses: nicAddresses(instance.NICs),
	}
}

func instanceListItemDTOs(instances []compute.Instance) []instanceListItem {
	out := make([]instanceListItem, 0, len(instances))
	for _, instance := range instances {
		out = append(out, instanceListItemDTO(instance))
	}
	return out
}

func sandboxListItemDTO(sandbox compute.Sandbox, instances int64) sandboxListItem {
	return sandboxListItem{
		Name:      sandbox.Name,
		Platform:  sandbox.Platform,
		CreatedAt: formatTime(sandbox.CreatedAt),
		ExpiresAt: formatTime(sandbox.ExpiresAt),
		Instances: instances,
	}
}

func imageListItemDTO(image compute.CatalogImage) imageListItem {
	return imageListItem{
		Name:        image.Name,
		OS:          image.OS,
		Version:     image.Version,
		Kind:        image.Kind,
		Desktop:     image.Desktop,
		Platform:    image.Platform,
		Description: image.Description,
	}
}

func defaultNetwork(networks []compute.Network) (compute.Network, bool) {
	for _, network := range networks {
		if network.Name == networkDefault {
			return network, true
		}
	}
	return compute.Network{}, false
}
