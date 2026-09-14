package incus

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/lxc/incus/v7/shared/api"
	"github.com/meigma/codemode"
)

const (
	memberStatusOnline = "Online"
	schedulerManual    = "manual"
	schedulerGroup     = "group"
)

type memberLoad struct {
	name    string
	freeRAM uint64
	load    float64
}

func (c *Client) resolveTarget(ctx context.Context, host string) (string, error) {
	if host != "" {
		if err := c.ensureOnlineMember(ctx, host); err != nil {
			return "", err
		}
		return host, nil
	}
	return c.selectLeastLoaded(ctx)
}

func (c *Client) ensureOnlineMember(ctx context.Context, host string) error {
	members, err := c.listedMembers(ctx)
	if err != nil {
		return err
	}
	if len(members) == 0 {
		if host == c.host {
			return nil
		}
		return placementErrorf("host %q is not available", host)
	}
	for _, member := range members {
		if member.ServerName != host {
			continue
		}
		if !memberOnline(member) {
			return placementErrorf("host %q is not online", host)
		}
		return nil
	}
	return placementErrorf("host %q is not available", host)
}

func (c *Client) selectLeastLoaded(ctx context.Context) (string, error) {
	members, err := c.listedMembers(ctx)
	if err != nil {
		return "", err
	}
	if len(members) == 0 {
		if c.host == "" {
			return "", placementError("no online cluster member is available")
		}
		return c.host, nil
	}

	candidates := make([]memberLoad, 0, len(members))
	srv := c.Scoped(ctx, api.ProjectDefaultName, "")
	for _, member := range members {
		if !memberOnline(member) || !autoPlaceable(member) {
			continue
		}
		load := memberLoad{name: member.ServerName}
		state, _, stateErr := srv.GetClusterMemberState(member.ServerName)
		if stateErr != nil {
			return "", fmt.Errorf("inspect member %q load: %w", member.ServerName, mapError(stateErr))
		}
		if state == nil || len(state.SysInfo.LoadAverages) == 0 {
			return "", fmt.Errorf("member %q returned no load information", member.ServerName)
		}
		load.freeRAM = state.SysInfo.FreeRAM
		load.load = state.SysInfo.LoadAverages[0]
		candidates = append(candidates, load)
	}
	name := pickLeastLoaded(candidates)
	if name == "" {
		return "", placementError("no online cluster member is available")
	}
	return name, nil
}

func (c *Client) listedMembers(ctx context.Context) ([]api.ClusterMember, error) {
	if c.server == nil || !c.server.IsClustered() {
		return nil, nil
	}
	members, err := c.Scoped(ctx, api.ProjectDefaultName, "").GetClusterMembers()
	if err != nil {
		return nil, mapError(err)
	}
	return members, nil
}

func pickLeastLoaded(members []memberLoad) string {
	if len(members) == 0 {
		return ""
	}
	return slices.MinFunc(members, cmpMemberLoad).name
}

func cmpMemberLoad(a, b memberLoad) int {
	if a.freeRAM != b.freeRAM {
		if a.freeRAM > b.freeRAM {
			return -1
		}
		return 1
	}
	if a.load != b.load {
		if a.load < b.load {
			return -1
		}
		return 1
	}
	return strings.Compare(a.name, b.name)
}

func memberOnline(member api.ClusterMember) bool {
	return strings.EqualFold(member.Status, memberStatusOnline)
}

func autoPlaceable(member api.ClusterMember) bool {
	scheduler := strings.ToLower(strings.TrimSpace(member.Config["scheduler.instance"]))
	return scheduler != schedulerManual && scheduler != schedulerGroup
}

func placementError(message string) error {
	return &codemode.AgentError{Message: message}
}

func placementErrorf(format string, args ...any) error {
	return &codemode.AgentError{Message: fmt.Sprintf(format, args...)}
}
