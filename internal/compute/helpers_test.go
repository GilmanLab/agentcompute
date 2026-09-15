package compute_test

import (
	"testing"
	"time"

	"github.com/meigma/codemode"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GilmanLab/agentcompute/internal/compute"
	"github.com/GilmanLab/agentcompute/internal/compute/mocks"
)

type testContext struct {
	backend *mocks.MockBackend
	service *compute.Service
}

func newTestContext(t *testing.T) *testContext {
	t.Helper()

	backend := mocks.NewMockBackend(t)
	catalog, err := compute.NewCatalog([]compute.CatalogImage{routerImage()})
	require.NoError(t, err)
	service, err := compute.New(backend, catalog, compute.Options{Host: "lab01"})
	require.NoError(t, err)
	return &testContext{backend: backend, service: service}
}

func routerImage() compute.CatalogImage {
	return compute.CatalogImage{
		Name:      "router",
		OS:        "alpinelinux",
		Version:   "3.22.5",
		Platform:  "incus",
		Kind:      "container",
		Kinds:     []string{"container"},
		Reference: "images:ubuntu/24.04",
		CPUs:      1,
		MemoryMB:  512,
		DiskGB:    2,
	}
}

func macosImage() compute.CatalogImage {
	return compute.CatalogImage{
		Name:     "macos/tahoe/desktop",
		OS:       "macos",
		Version:  "26.6.2",
		Platform: "mac",
		Kind:     "vm",
		Kinds:    []string{"vm"},
		Desktop:  true,
		Seed:     "ac-seed-macos-tahoe-desktop",
		CPUs:     4,
		MemoryMB: 8192,
		DiskGB:   100,
	}
}

func liveSandbox(name string) compute.Sandbox {
	now := time.Now()
	return compute.Sandbox{
		Name:      name,
		Platform:  "incus",
		Host:      "lab01",
		CreatedAt: now.Add(-time.Hour),
		ExpiresAt: now.Add(time.Hour),
	}
}

func liveMacSandbox(name string) compute.Sandbox {
	now := time.Now()
	return compute.Sandbox{
		Name:      name,
		Platform:  "mac",
		CreatedAt: now.Add(-time.Hour),
		ExpiresAt: now.Add(time.Hour),
	}
}

type mixedContext struct {
	incus   *mocks.MockBackend
	mac     *mocks.MockBackend
	service *compute.Service
}

func newMixedContext(t *testing.T) *mixedContext {
	t.Helper()

	incus := mocks.NewMockBackend(t)
	mac := mocks.NewMockBackend(t)
	catalog, err := compute.NewCatalog([]compute.CatalogImage{routerImage(), macosImage()})
	require.NoError(t, err)
	service, err := compute.New(incus, catalog, compute.Options{Host: "lab01", Mac: mac})
	require.NoError(t, err)
	return &mixedContext{incus: incus, mac: mac, service: service}
}

func expiredSandbox() compute.Sandbox {
	now := time.Now()
	return compute.Sandbox{
		Name:      "demo",
		Platform:  "incus",
		Host:      "lab01",
		CreatedAt: now.Add(-2 * time.Hour),
		ExpiresAt: now.Add(-time.Minute),
	}
}

func runningInstance() compute.Instance {
	return compute.Instance{
		Ref:    compute.Ref{Sandbox: "demo", Name: "web"},
		Image:  "router",
		Kind:   "container",
		Host:   "lab01",
		Status: "Running",
	}
}

func requireAgentMessage(t *testing.T, err error, message string) {
	t.Helper()
	require.Error(t, err)
	var agent *codemode.AgentError
	require.ErrorAs(t, err, &agent)
	assert.Equal(t, message, agent.Message)
}
