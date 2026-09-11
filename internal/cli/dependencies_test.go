package cli

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/GilmanLab/agentcompute/internal/compute"
	"github.com/GilmanLab/agentcompute/internal/compute/mocks"
	"github.com/GilmanLab/agentcompute/internal/mcpserver"
)

func testDependencies(t *testing.T) *mcpserver.Dependencies {
	t.Helper()
	catalog, err := compute.NewCatalog([]compute.CatalogImage{{
		Name: "router", OS: "alpinelinux", Version: "3.22", Platform: "incus",
		Kind: "container", Kinds: []string{"container"}, Reference: "images:alpine/3.22",
		CPUs: 1, MemoryMB: 512, DiskGB: 2,
	}})
	require.NoError(t, err)
	service, err := compute.New(mocks.NewMockBackend(t), catalog, compute.Options{Host: "lab01"})
	require.NoError(t, err)
	deps := mcpserver.NewDependencies(service)
	return &deps
}
