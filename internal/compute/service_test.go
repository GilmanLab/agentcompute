package compute_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/GilmanLab/agentcompute/internal/compute"
	"github.com/GilmanLab/agentcompute/internal/compute/mocks"
)

func TestCreateSandboxNameValidation(t *testing.T) {
	t.Parallel()

	tc := newTestContext(t)
	_, err := tc.service.CreateSandbox(t.Context(), "Default", 0, "subj")
	requireAgentMessage(t, err, `invalid name "Default"`)

	_, err = tc.service.CreateSandbox(t.Context(), "none", 0, "subj")
	requireAgentMessage(t, err, `name "none" is reserved`)
}

func TestCreateSandboxZeroTTLUsesOptionsDefault(t *testing.T) {
	t.Parallel()

	backend := mocks.NewMockBackend(t)
	catalog, err := compute.NewCatalog([]compute.CatalogImage{routerImage()})
	require.NoError(t, err)
	configured := 15 * time.Minute
	service, err := compute.New(backend, catalog, compute.Options{
		Host:       "lab01",
		DefaultTTL: configured,
	})
	require.NoError(t, err)

	backend.EXPECT().GetSandbox(mock.Anything, "demo").Return(compute.Sandbox{}, compute.ErrNotFound)
	backend.EXPECT().CreateSandbox(mock.Anything, mock.AnythingOfType("compute.Sandbox")).
		RunAndReturn(func(_ context.Context, box compute.Sandbox) error {
			assert.WithinDuration(t, time.Now().Add(configured), box.ExpiresAt, time.Second)
			assert.Equal(t, "lab01", box.Host)
			return nil
		})

	got, err := service.CreateSandbox(t.Context(), "demo", 0, "subj")
	require.NoError(t, err)
	assert.WithinDuration(t, time.Now().Add(configured), got.ExpiresAt, time.Second)
}

func TestCreateSandboxGeneratedNameRetriesCollision(t *testing.T) {
	t.Parallel()

	tc := newTestContext(t)
	calls := 0
	tc.backend.EXPECT().GetSandbox(mock.Anything, mock.AnythingOfType("string")).
		RunAndReturn(func(_ context.Context, name string) (compute.Sandbox, error) {
			calls++
			if calls == 1 {
				return compute.Sandbox{Name: name}, nil
			}
			return compute.Sandbox{}, compute.ErrNotFound
		})
	tc.backend.EXPECT().CreateSandbox(mock.Anything, mock.AnythingOfType("compute.Sandbox")).Return(nil)

	box, err := tc.service.CreateSandbox(t.Context(), "", 0, "subj")
	require.NoError(t, err)
	assert.NotEmpty(t, box.Name)
	assert.Equal(t, "lab01", box.Host)
	assert.GreaterOrEqual(t, calls, 2)
	assert.WithinDuration(t, time.Now().Add(240*time.Minute), box.ExpiresAt, 5*time.Second)
}

func TestCreateInstanceRejectsExpiredSandboxAfterGate(t *testing.T) {
	t.Parallel()

	tc := newTestContext(t)
	tc.backend.EXPECT().GetSandbox(mock.Anything, "demo").Return(expiredSandbox("demo"), nil)

	_, err := tc.service.CreateInstance(t.Context(), compute.CreateInstance{
		Ref:   compute.Ref{Sandbox: "demo", Name: "web"},
		Image: routerImage(),
		Kind:  "container",
	})
	requireAgentMessage(t, err, `sandbox "demo" has expired`)
}

func TestExtendSandboxWritesNowPlusTTL(t *testing.T) {
	t.Parallel()

	tc := newTestContext(t)
	live := liveSandbox("demo")
	ttl := 30 * time.Minute
	tc.backend.EXPECT().GetSandbox(mock.Anything, "demo").Return(live, nil)
	tc.backend.EXPECT().ExtendSandbox(mock.Anything, "demo", mock.AnythingOfType("time.Time")).
		RunAndReturn(func(_ context.Context, _ string, expires time.Time) (compute.Sandbox, error) {
			assert.WithinDuration(t, time.Now().Add(ttl), expires, time.Second)
			live.ExpiresAt = expires
			return live, nil
		})

	got, err := tc.service.ExtendSandbox(t.Context(), "demo", ttl)
	require.NoError(t, err)
	assert.WithinDuration(t, time.Now().Add(ttl), got.ExpiresAt, time.Second)
}

func TestGetInstanceNotFoundMessage(t *testing.T) {
	t.Parallel()

	tc := newTestContext(t)
	tc.backend.EXPECT().GetInstance(mock.Anything, compute.Ref{Sandbox: "y", Name: "x"}).
		Return(compute.Instance{}, compute.ErrNotFound)

	_, err := tc.service.GetInstance(t.Context(), compute.Ref{Sandbox: "y", Name: "x"})
	requireAgentMessage(t, err, `instance "x" not found in sandbox "y"`)
}

func TestCreateInstanceRejectsOtherHost(t *testing.T) {
	t.Parallel()

	tc := newTestContext(t)
	tc.backend.EXPECT().GetSandbox(mock.Anything, "demo").Return(liveSandbox("demo"), nil)

	_, err := tc.service.CreateInstance(t.Context(), compute.CreateInstance{
		Ref:   compute.Ref{Sandbox: "demo", Name: "web"},
		Image: routerImage(),
		Kind:  "container",
		Host:  "lab02",
	})
	requireAgentMessage(t, err, `host "lab02" is not sandbox member "lab01"`)
}

func TestCreateNetworkRejectsOVN(t *testing.T) {
	t.Parallel()

	tc := newTestContext(t)
	_, err := tc.service.CreateNetwork(t.Context(), "demo", compute.Network{Name: "lan", Kind: "ovn"})
	requireAgentMessage(t, err, `kind "ovn" is not available yet`)
}

func TestCreateInstanceRejectsMacPlatform(t *testing.T) {
	t.Parallel()

	tc := newTestContext(t)
	image := routerImage()
	image.Platform = "mac"
	_, err := tc.service.CreateInstance(t.Context(), compute.CreateInstance{
		Ref:   compute.Ref{Sandbox: "demo", Name: "web"},
		Image: image,
	})
	requireAgentMessage(t, err, `platform "mac" is not available yet`)
}

func TestCreateInstanceDoesNotHoldGateDuringWait(t *testing.T) {
	t.Parallel()

	tc := newTestContext(t)
	live := liveSandbox("demo")
	inst := runningInstance()
	pending := mocks.NewMockPendingInstance(t)
	waitStarted := make(chan struct{})
	releaseWait := make(chan struct{})

	tc.backend.EXPECT().GetSandbox(mock.Anything, "demo").Return(live, nil).Times(2)
	tc.backend.EXPECT().GetInstance(mock.Anything, compute.Ref{Sandbox: "demo", Name: "web"}).
		Return(compute.Instance{}, compute.ErrNotFound)
	tc.backend.EXPECT().BeginCreateInstance(mock.Anything, mock.Anything).Return(pending, nil)
	pending.EXPECT().Wait(mock.Anything).RunAndReturn(func(context.Context) (compute.Instance, error) {
		close(waitStarted)
		<-releaseWait
		return inst, nil
	})
	tc.backend.EXPECT().ExtendSandbox(mock.Anything, "demo", mock.AnythingOfType("time.Time")).
		Return(live, nil)

	errCh := make(chan error, 1)
	go func() {
		_, err := tc.service.CreateInstance(t.Context(), compute.CreateInstance{
			Ref:   compute.Ref{Sandbox: "demo", Name: "web"},
			Image: routerImage(),
			Kind:  "container",
		})
		errCh <- err
	}()

	select {
	case <-waitStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("Wait was not entered; gate likely still held")
	}

	_, err := tc.service.ExtendSandbox(t.Context(), "demo", time.Hour)
	require.NoError(t, err)
	close(releaseWait)
	require.NoError(t, <-errCh)
}

func TestListImagesFilters(t *testing.T) {
	t.Parallel()

	tc := newTestContext(t)
	all := tc.service.ListImages("", nil, "")
	require.Len(t, all, 1)
	desktop := true
	assert.Empty(t, tc.service.ListImages("", &desktop, ""))
	assert.Len(t, tc.service.ListImages("alpinelinux", nil, "incus"), 1)
}

func TestCatalogImageMissing(t *testing.T) {
	t.Parallel()

	tc := newTestContext(t)
	_, err := tc.service.CatalogImage("missing")
	requireAgentMessage(t, err, `image "missing" not found`)
}

func TestDeleteSandboxMarksExpiryThenDeletes(t *testing.T) {
	t.Parallel()

	tc := newTestContext(t)
	tc.backend.EXPECT().GetSandbox(mock.Anything, "demo").Return(liveSandbox("demo"), nil)
	tc.backend.EXPECT().ExtendSandbox(mock.Anything, "demo", mock.AnythingOfType("time.Time")).
		RunAndReturn(func(_ context.Context, name string, expires time.Time) (compute.Sandbox, error) {
			assert.WithinDuration(t, time.Now(), expires, time.Second)
			box := liveSandbox(name)
			box.ExpiresAt = expires
			return box, nil
		})
	tc.backend.EXPECT().DeleteSandbox(mock.Anything, "demo").Return(nil)

	require.NoError(t, tc.service.DeleteSandbox(t.Context(), "demo"))
}
