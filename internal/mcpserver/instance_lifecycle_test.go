package mcpserver

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/meigma/codemode/authz"

	"github.com/GilmanLab/agentcompute/internal/compute"
)

func TestSnapshotListEmptyItemsAreNonNil(t *testing.T) {
	t.Parallel()

	tc := newTestDeps(t)
	tc.instance.EXPECT().
		ListSnapshots(mock.Anything, compute.Ref{Sandbox: "demo", Name: "web"}).
		Return(nil, nil)

	out, err := instanceAPI{instances: tc.instance}.listSnapshots(
		context.Background(),
		authz.Subject{},
		instanceSnapshotListIn{Sandbox: "demo", Name: "web"},
	)
	require.NoError(t, err)
	require.NotNil(t, out.Items)
	assert.Empty(t, out.Items)
}

func TestWaitConvertsElapsedToWholeSeconds(t *testing.T) {
	t.Parallel()

	tc := newTestDeps(t)
	tc.instance.EXPECT().
		WaitInstance(mock.Anything, compute.WaitRequest{
			Ref:     compute.Ref{Sandbox: "demo", Name: "web"},
			Until:   compute.WaitUntilRunning,
			Timeout: time.Duration(0),
		}).
		Return(compute.WaitResult{Status: "Running", Elapsed: 1500 * time.Millisecond}, nil)

	out, err := instanceAPI{instances: tc.instance}.wait(
		context.Background(),
		authz.Subject{},
		instanceWaitIn{Sandbox: "demo", Name: "web", Until: compute.WaitUntilRunning},
	)
	require.NoError(t, err)
	assert.Equal(t, "Running", out.Status)
	assert.Equal(t, int64(1), out.ElapsedSeconds)
}

func TestCreateResolvesSandboxImage(t *testing.T) {
	t.Parallel()

	tc := newTestDeps(t)
	image := compute.CatalogImage{
		Name:        "golden",
		Kind:        "container",
		Fingerprint: "abc123",
		CPUs:        1,
		MemoryMB:    512,
		DiskGB:      2,
	}
	tc.instance.EXPECT().ResolveImage(mock.Anything, "demo", "golden").Return(image, nil)
	tc.instance.EXPECT().CreateInstance(mock.Anything, mock.Anything).Return(compute.Instance{
		Ref:    compute.Ref{Sandbox: "demo", Name: "web"},
		Kind:   "container",
		Host:   "lab02",
		Status: "Running",
	}, nil)

	out, err := instanceAPI{instances: tc.instance}.create(
		context.Background(),
		authz.Subject{},
		instanceCreateIn{Sandbox: "demo", Name: "web", Image: "golden"},
	)
	require.NoError(t, err)
	assert.Equal(t, "web", out.Name)
	assert.Equal(t, "lab02", out.Host)
	require.NotNil(t, out.Addresses)
}
