package compute_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/GilmanLab/agentcompute/internal/compute"
	"github.com/GilmanLab/agentcompute/internal/compute/mocks"
)

func TestCreateSandboxMacRequiresBackend(t *testing.T) {
	t.Parallel()

	tc := newTestContext(t)
	_, err := tc.service.CreateSandbox(t.Context(), "demo", 0, "subj", "mac")
	requireAgentMessage(t, err, `platform "mac" is not available yet`)
}

func TestCreateSandboxMacUsesMacBackend(t *testing.T) {
	t.Parallel()

	tc := newMixedContext(t)
	tc.incus.EXPECT().GetSandbox(mock.Anything, "demo").Return(compute.Sandbox{}, compute.ErrNotFound)
	tc.mac.EXPECT().GetSandbox(mock.Anything, "demo").Return(compute.Sandbox{}, compute.ErrNotFound)
	tc.mac.EXPECT().CreateSandbox(mock.Anything, mock.AnythingOfType("compute.Sandbox")).
		RunAndReturn(func(_ context.Context, box compute.Sandbox) error {
			assert.Equal(t, "mac", box.Platform)
			assert.Empty(t, box.NetworkKind)
			assert.Empty(t, box.Host)
			return nil
		})

	got, err := tc.service.CreateSandbox(t.Context(), "demo", 0, "subj", "mac")
	require.NoError(t, err)
	assert.Equal(t, "mac", got.Platform)
	assert.Equal(t, "demo", got.Name)
}

func TestCreateSandboxNameIsGloballyUnique(t *testing.T) {
	t.Parallel()

	tc := newMixedContext(t)
	tc.incus.EXPECT().GetSandbox(mock.Anything, "demo").Return(liveSandbox("demo"), nil)

	_, err := tc.service.CreateSandbox(t.Context(), "demo", 0, "subj", "mac")
	requireAgentMessage(t, err, `sandbox "demo" already exists`)
}

func TestListSandboxesMergesBackends(t *testing.T) {
	t.Parallel()

	tc := newMixedContext(t)
	incusBox := liveSandbox("alpha")
	macBox := liveMacSandbox("beta")
	tc.incus.EXPECT().ListSandboxes(mock.Anything).Return([]compute.Sandbox{incusBox}, nil)
	tc.mac.EXPECT().ListSandboxes(mock.Anything).Return([]compute.Sandbox{macBox}, nil)

	got, err := tc.service.ListSandboxes(t.Context())
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, "alpha", got[0].Name)
	assert.Equal(t, "incus", got[0].Platform)
	assert.Equal(t, "beta", got[1].Name)
	assert.Equal(t, "mac", got[1].Platform)
}

func TestGetSandboxMacReturnsEmptyNetworks(t *testing.T) {
	t.Parallel()

	tc := newMixedContext(t)
	box := liveMacSandbox("demo")
	tc.incus.EXPECT().GetSandbox(mock.Anything, "demo").Return(compute.Sandbox{}, compute.ErrNotFound).Times(2)
	tc.mac.EXPECT().GetSandbox(mock.Anything, "demo").Return(box, nil).Times(2)
	tc.mac.EXPECT().ListInstances(mock.Anything, "demo").Return(nil, nil)
	got, instances, networks, err := tc.service.GetSandbox(t.Context(), "demo")
	require.NoError(t, err)
	assert.Equal(t, "mac", got.Platform)
	assert.Empty(t, instances)
	require.NotNil(t, networks)
	assert.Empty(t, networks)
}

func TestMacCreateRejectsUnsupportedPlacement(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, host, network string
	}{
		{name: "network isolation must not silently become NAT", network: "none"},
		{name: "cluster placement must not silently select Studio", host: "lab02"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tc := newMixedContext(t)
			tc.incus.EXPECT().GetSandbox(mock.Anything, "demo").Return(compute.Sandbox{}, compute.ErrNotFound)
			tc.mac.EXPECT().GetSandbox(mock.Anything, "demo").Return(liveMacSandbox("demo"), nil)
			_, err := tc.service.CreateInstance(t.Context(), compute.CreateInstance{
				Ref: compute.Ref{Sandbox: "demo", Name: "desk"}, Image: macosImage(),
				Host: tt.host, Network: tt.network,
			})
			requireAgentMessage(t, err, "unsupported on platform mac")
		})
	}
}

func TestCreateInstanceRejectsIncusImageOnMacSandbox(t *testing.T) {
	t.Parallel()

	tc := newMixedContext(t)
	tc.incus.EXPECT().GetSandbox(mock.Anything, "demo").Return(compute.Sandbox{}, compute.ErrNotFound)
	tc.mac.EXPECT().GetSandbox(mock.Anything, "demo").Return(liveMacSandbox("demo"), nil)

	_, err := tc.service.CreateInstance(t.Context(), compute.CreateInstance{
		Ref:   compute.Ref{Sandbox: "demo", Name: "web"},
		Image: routerImage(),
	})
	requireAgentMessage(t, err, `image "router" is not available on platform "mac"`)
}

func TestMacNetworkOperationsAreUnsupported(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		call func(*testing.T, *compute.Service)
	}{
		{
			name: "create network",
			call: func(t *testing.T, service *compute.Service) {
				t.Helper()
				_, err := service.CreateNetwork(t.Context(), "demo", compute.Network{Name: "lan", Kind: "ovn"})
				requireAgentMessage(t, err, "unsupported on platform mac")
			},
		},
		{
			name: "list networks",
			call: func(t *testing.T, service *compute.Service) {
				t.Helper()
				_, err := service.ListNetworks(t.Context(), "demo")
				requireAgentMessage(t, err, "unsupported on platform mac")
			},
		},
		{
			name: "impair with invalid settings",
			call: func(t *testing.T, service *compute.Service) {
				t.Helper()
				err := service.ImpairNIC(
					t.Context(),
					compute.Ref{Sandbox: "demo", Name: "web"},
					"eth0",
					compute.Impairment{
						JitterMS: 5,
					},
				)
				requireAgentMessage(t, err, "unsupported on platform mac")
			},
		},
		{
			name: "remove baseline acl",
			call: func(t *testing.T, service *compute.Service) {
				t.Helper()
				err := service.RemoveACLRule(t.Context(), "demo", "default", compute.BaselineEgressMgmt)
				requireAgentMessage(t, err, "unsupported on platform mac")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tc := newMixedContext(t)
			tc.incus.EXPECT().GetSandbox(mock.Anything, "demo").Return(compute.Sandbox{}, compute.ErrNotFound)
			tc.mac.EXPECT().GetSandbox(mock.Anything, "demo").Return(liveMacSandbox("demo"), nil)
			tt.call(t, tc.service)
		})
	}
}

func TestListImagesFiltersMacPlatform(t *testing.T) {
	t.Parallel()

	backend := mocks.NewMockBackend(t)
	catalog, err := compute.NewCatalog([]compute.CatalogImage{routerImage(), macosImage()})
	require.NoError(t, err)
	service, err := compute.New(backend, catalog, compute.Options{Host: "lab01"})
	require.NoError(t, err)

	mac := service.ListImages("", nil, "mac")
	require.Len(t, mac, 1)
	assert.Equal(t, "macos/tahoe/desktop", mac[0].Name)
	assert.Equal(t, "ac-seed-macos-tahoe-desktop", mac[0].Seed)
	assert.Empty(t, service.ListImages("macos", nil, "incus"))
}
