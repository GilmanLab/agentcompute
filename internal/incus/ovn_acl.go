package incus

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/lxc/incus/v7/shared/api"

	"github.com/GilmanLab/agentcompute/internal/compute"
)

const (
	aclNameBaseline     = "baseline"
	aclRuleIDPrefix     = "agentcompute:id="
	aclDirectionIngress = "ingress"
	aclDirectionEgress  = "egress"
)

func (c *Client) ensureNetworkACLs(ctx context.Context, sandbox, network string) error {
	if err := c.ensureBaselineACL(ctx, sandbox); err != nil {
		return err
	}
	return c.ensureAgentACL(ctx, sandbox, network)
}

func agentACLName(network string) string {
	sum := sha256.Sum256([]byte(network))
	return "agent-" + hex.EncodeToString(sum[:8])
}

func (c *Client) ensureBaselineACL(ctx context.Context, sandbox string) error {
	existing, _, err := c.Scoped(ctx, projectName(sandbox), "").GetNetworkACL(aclNameBaseline)
	if err == nil {
		if existing.Config[metaSandbox] == sandbox && existing.Config[metaVersion] == versionValue {
			return nil
		}
		return fmt.Errorf("acl %q is not owned by sandbox %q", aclNameBaseline, sandbox)
	}
	if !errors.Is(mapError(err), compute.ErrNotFound) {
		return mapOVNError(err)
	}
	return c.putACL(ctx, sandbox, aclNameBaseline, api.NetworkACLPut{
		Description: "agentcompute baseline management and OOB drops",
		Ingress:     []api.NetworkACLRule{},
		Egress: []api.NetworkACLRule{
			baselineRule(compute.BaselineEgressMgmt, "10.10.10.0/24"),
			baselineRule(compute.BaselineEgressOOB, "10.10.70.0/24"),
		},
		Config: map[string]string{
			metaSandbox: sandbox,
			metaVersion: versionValue,
			metaName:    aclNameBaseline,
		},
	})
}

func baselineRule(id, cidr string) api.NetworkACLRule {
	return api.NetworkACLRule{
		Action:      "drop",
		State:       "enabled",
		Destination: cidr,
		Description: aclRuleIDPrefix + id,
	}
}

func (c *Client) ensureAgentACL(ctx context.Context, sandbox, network string) error {
	name := agentACLName(network)
	existing, _, err := c.Scoped(ctx, projectName(sandbox), "").GetNetworkACL(name)
	if err == nil {
		if existing.Config[metaSandbox] == sandbox && existing.Config[metaVersion] == versionValue {
			return nil
		}
		return fmt.Errorf("acl %q is not owned by sandbox %q", name, sandbox)
	}
	if !errors.Is(mapError(err), compute.ErrNotFound) {
		return mapOVNError(err)
	}
	return c.putACL(ctx, sandbox, name, api.NetworkACLPut{
		Description: "agentcompute agent ACL",
		Ingress:     []api.NetworkACLRule{},
		Egress:      []api.NetworkACLRule{},
		Config: map[string]string{
			metaSandbox: sandbox,
			metaVersion: versionValue,
			metaName:    network,
		},
	})
}

func (c *Client) putACL(ctx context.Context, sandbox, name string, spec api.NetworkACLPut) error {
	srv := c.Scoped(ctx, projectName(sandbox), "")
	existing, etag, err := srv.GetNetworkACL(name)
	if errors.Is(mapError(err), compute.ErrNotFound) {
		return mapOVNError(srv.CreateNetworkACL(api.NetworkACLsPost{
			NetworkACLPost: api.NetworkACLPost{Name: name},
			NetworkACLPut:  spec,
		}))
	}
	if err != nil {
		return mapOVNError(err)
	}
	if existing.Config[metaSandbox] != sandbox || existing.Config[metaVersion] != versionValue {
		return fmt.Errorf("acl %q is not owned by sandbox %q", name, sandbox)
	}
	if name == aclNameBaseline {
		spec.Ingress = existing.Ingress
		spec.Egress = existing.Egress
		if spec.Config == nil {
			spec.Config = map[string]string{}
		}
		for key, value := range existing.Config {
			if spec.Config[key] == "" {
				spec.Config[key] = value
			}
		}
	}
	return mapOVNError(srv.UpdateNetworkACL(name, spec, etag))
}

func (c *Client) AddACLRule(
	ctx context.Context,
	sandbox, network string,
	rule compute.ACLRule,
) (compute.ACLRule, error) {
	ctx, cancel := c.ovnContext(ctx)
	defer cancel()
	if _, err := c.ownedProjectNetwork(ctx, sandbox, network); err != nil {
		return compute.ACLRule{}, err
	}
	if err := c.ensureNetworkACLs(ctx, sandbox, network); err != nil {
		return compute.ACLRule{}, err
	}
	if rule.ID == "" {
		id, err := randomRuleID()
		if err != nil {
			return compute.ACLRule{}, err
		}
		rule.ID = id
	}
	if computeRuleIsBaseline(rule.ID) {
		return compute.ACLRule{}, fmt.Errorf("rule %q is a baseline ACL and cannot be modified", rule.ID)
	}

	srv := c.Scoped(ctx, projectName(sandbox), "")
	acl, etag, err := srv.GetNetworkACL(agentACLName(network))
	if err != nil {
		return compute.ACLRule{}, mapOVNError(err)
	}
	incusRule := toIncusACLRule(rule)
	if rule.Direction == aclDirectionIngress {
		acl.Ingress = append(acl.Ingress, incusRule)
	} else {
		acl.Egress = append(acl.Egress, incusRule)
	}
	if err := srv.UpdateNetworkACL(agentACLName(network), acl.Writable(), etag); err != nil {
		return compute.ACLRule{}, mapOVNError(err)
	}
	return rule, nil
}

func (c *Client) RemoveACLRule(ctx context.Context, sandbox, network, ruleID string) error {
	ctx, cancel := c.ovnContext(ctx)
	defer cancel()
	if computeRuleIsBaseline(ruleID) {
		return fmt.Errorf("rule %q is a baseline ACL and cannot be removed", ruleID)
	}
	if _, err := c.ownedProjectNetwork(ctx, sandbox, network); err != nil {
		return err
	}
	srv := c.Scoped(ctx, projectName(sandbox), "")
	acl, etag, err := srv.GetNetworkACL(agentACLName(network))
	if err != nil {
		return mapOVNError(err)
	}
	ingress, removed := filterACLRules(acl.Ingress, ruleID)
	egress, removedEgress := filterACLRules(acl.Egress, ruleID)
	if !removed && !removedEgress {
		return compute.ErrNotFound
	}
	acl.Ingress = ingress
	acl.Egress = egress
	return mapOVNError(srv.UpdateNetworkACL(agentACLName(network), acl.Writable(), etag))
}

func (c *Client) ownedProjectNetwork(ctx context.Context, sandbox, logical string) (*api.Network, error) {
	network, err := c.projectNetwork(ctx, sandbox, logical)
	if err != nil {
		return nil, err
	}
	if network.Config[metaSandbox] != sandbox || network.Config[metaVersion] != versionValue {
		return nil, compute.ErrNotFound
	}
	return network, nil
}

func (c *Client) deleteOwnedACLs(ctx context.Context, sandbox string) []error {
	srv := c.Scoped(ctx, projectName(sandbox), "")
	acls, err := srv.GetNetworkACLs()
	if err != nil {
		if errors.Is(mapError(err), compute.ErrNotFound) {
			return nil
		}
		return []error{mapError(err)}
	}
	var errs []error
	for _, acl := range acls {
		if acl.Config[metaSandbox] != sandbox || acl.Config[metaVersion] != versionValue {
			continue
		}
		if err := srv.DeleteNetworkACL(acl.Name); err != nil && !errors.Is(mapError(err), compute.ErrNotFound) {
			errs = append(errs, mapError(err))
		}
	}
	return errs
}

func toIncusACLRule(rule compute.ACLRule) api.NetworkACLRule {
	protocol := rule.Protocol
	if protocol == "icmp" {
		protocol = "icmp4"
	}
	return api.NetworkACLRule{
		Action:          rule.Action,
		Source:          rule.Src,
		Destination:     rule.Dst,
		Protocol:        protocol,
		DestinationPort: rule.Port,
		State:           "enabled",
		Description:     aclRuleIDPrefix + rule.ID,
	}
}

func filterACLRules(rules []api.NetworkACLRule, id string) ([]api.NetworkACLRule, bool) {
	out := make([]api.NetworkACLRule, 0, len(rules))
	removed := false
	for _, rule := range rules {
		if aclRuleID(rule) == id {
			removed = true
			continue
		}
		out = append(out, rule)
	}
	return out, removed
}

func aclRuleID(rule api.NetworkACLRule) string {
	return strings.TrimPrefix(rule.Description, aclRuleIDPrefix)
}

func computeRuleIsBaseline(id string) bool {
	switch id {
	case compute.BaselineEgressMgmt, compute.BaselineEgressOOB:
		return true
	default:
		return false
	}
}

func randomRuleID() (string, error) {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return "r-" + hex.EncodeToString(buf[:]), nil
}
