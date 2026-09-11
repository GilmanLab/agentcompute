package mcpserver

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/meigma/codemode"
	"github.com/meigma/codemode/authz"

	"github.com/GilmanLab/agentcompute/internal/compute"
)

func TestCreateSandboxHidesPhysicalNetworkIdentity(t *testing.T) {
	t.Parallel()

	tc := newTestDeps(t)
	created := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	expires := created.Add(240 * time.Minute)
	tc.sandbox.EXPECT().
		CreateSandbox(mock.Anything, "", time.Duration(0), string(trustedSubjectID)).
		Return(compute.Sandbox{Name: "demo", Platform: platformIncus, CreatedAt: created, ExpiresAt: expires}, nil)
	tc.sandbox.EXPECT().
		GetSandbox(mock.Anything, "demo").
		Return(compute.Sandbox{Name: "demo", Platform: platformIncus, CreatedAt: created, ExpiresAt: expires}, nil, []compute.Network{{
			Name:         networkDefault,
			PhysicalName: "acdeadbeef",
			Kind:         kindBridge,
			CIDR:         "10.0.0.0/24",
			Gateway:      "10.0.0.1",
		}}, nil)

	out, err := sandboxAPI{sandboxes: tc.sandbox}.create(
		context.Background(),
		authz.Subject{ID: trustedSubjectID},
		sandboxCreateIn{},
	)
	require.NoError(t, err)
	assert.Equal(t, "demo", out.Name)
	assert.Equal(t, "2026-09-11T16:00:00Z", out.ExpiresAt)
	assert.Equal(t, networkDefault, out.Network.Name)
	assert.Equal(t, kindBridge, out.Network.Kind)
	raw, err := json.Marshal(out)
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "acdeadbeef")
	assert.NotContains(t, string(raw), "physical")
}

func TestListSandboxesEmptyItemsAreNonNil(t *testing.T) {
	t.Parallel()

	tc := newTestDeps(t)
	tc.sandbox.EXPECT().ListSandboxes(mock.Anything).Return(nil, nil)

	out, err := sandboxAPI{sandboxes: tc.sandbox}.list(context.Background(), authz.Subject{}, sandboxListIn{})
	require.NoError(t, err)
	require.NotNil(t, out.Items)
	assert.Empty(t, out.Items)
}

func TestNetCreateRejectsOVN(t *testing.T) {
	t.Parallel()

	_, err := netAPI{}.create(context.Background(), authz.Subject{}, netCreateIn{
		Sandbox: "demo",
		Name:    "lan",
		Kind:    new(kindOVN),
	})
	require.Error(t, err)
	var actionable *codemode.AgentError
	require.ErrorAs(t, err, &actionable)
}

func TestNetCreateDefaultsToBridgeAndOmitsPhysicalName(t *testing.T) {
	t.Parallel()

	tc := newTestDeps(t)
	tc.network.EXPECT().
		CreateNetwork(mock.Anything, "demo", compute.Network{Name: "lan", Kind: kindBridge}).
		Return(compute.Network{
			Name:         "lan",
			PhysicalName: "acffffffff",
			Kind:         kindBridge,
			CIDR:         "10.1.0.0/24",
			Gateway:      "10.1.0.1",
		}, nil)

	out, err := netAPI{networks: tc.network}.create(context.Background(), authz.Subject{}, netCreateIn{
		Sandbox: "demo",
		Name:    "lan",
	})
	require.NoError(t, err)
	assert.Equal(t, "lan", out.Name)
	assert.Equal(t, kindBridge, out.Kind)
	raw, err := json.Marshal(out)
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "acffffffff")
}

func TestGetInstanceEmptyCollectionsAreNonNil(t *testing.T) {
	t.Parallel()

	tc := newTestDeps(t)
	tc.instance.EXPECT().
		GetInstance(mock.Anything, compute.Ref{Sandbox: "demo", Name: "rtr"}).
		Return(compute.Instance{Ref: compute.Ref{Sandbox: "demo", Name: "rtr"}}, nil)

	out, err := instanceAPI{instances: tc.instance}.get(
		context.Background(),
		authz.Subject{},
		instanceGetIn{Sandbox: "demo", Name: "rtr"},
	)
	require.NoError(t, err)
	require.NotNil(t, out.NICs)
	require.NotNil(t, out.Snapshots)
	assert.Empty(t, out.NICs)
	assert.Empty(t, out.Snapshots)
}
