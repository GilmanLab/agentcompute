package compute_test

import (
	"context"
	"testing"
	"time"

	"github.com/meigma/codemode"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/GilmanLab/agentcompute/internal/compute"
	"github.com/GilmanLab/agentcompute/internal/compute/mocks"
)

func TestCreateSandboxNameValidation(t *testing.T) {
	t.Parallel()

	tc := newTestContext(t)
	_, err := tc.service.CreateSandbox(t.Context(), "Default", 0, "subj", "")
	requireAgentMessage(t, err, `invalid name "Default"`)

	_, err = tc.service.CreateSandbox(t.Context(), "none", 0, "subj", "")
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
			return nil
		})

	got, err := service.CreateSandbox(t.Context(), "demo", 0, "subj", "")
	require.NoError(t, err)
	assert.WithinDuration(t, time.Now().Add(configured), got.ExpiresAt, time.Second)
}

func TestCreateSandboxGeneratedNameRetriesCollision(t *testing.T) {
	t.Parallel()

	tc := newTestContext(t)
	collision := ""
	tc.backend.EXPECT().GetSandbox(mock.Anything, mock.AnythingOfType("string")).
		RunAndReturn(func(_ context.Context, name string) (compute.Sandbox, error) {
			if collision == "" {
				collision = name
			}
			if name == collision {
				return compute.Sandbox{Name: name}, nil
			}
			return compute.Sandbox{}, compute.ErrNotFound
		})
	tc.backend.EXPECT().CreateSandbox(mock.Anything, mock.AnythingOfType("compute.Sandbox")).Return(nil)

	box, err := tc.service.CreateSandbox(t.Context(), "", 0, "subj", "")
	require.NoError(t, err)
	assert.NotEqual(t, collision, box.Name)
}

func TestCreateInstanceRejectsExpiredSandboxAfterGate(t *testing.T) {
	t.Parallel()

	tc := newTestContext(t)
	tc.backend.EXPECT().GetSandbox(mock.Anything, "demo").Return(expiredSandbox(), nil)

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

func TestCreateNetworkRejectsMixedKind(t *testing.T) {
	t.Parallel()

	tc := newTestContext(t)
	tc.backend.EXPECT().GetSandbox(mock.Anything, "demo").Return(liveSandbox("demo"), nil)
	_, err := tc.service.CreateNetwork(t.Context(), "demo", compute.Network{Name: "lan", Kind: "ovn"})
	requireAgentMessage(t, err, `cannot create a "ovn" network in a "bridge" sandbox`)
}

func TestCreateNetworkRejectsBridgeInOVNSandbox(t *testing.T) {
	t.Parallel()

	tc := newTestContext(t)
	box := liveSandbox("demo")
	box.NetworkKind = "ovn"
	tc.backend.EXPECT().GetSandbox(mock.Anything, "demo").Return(box, nil)
	_, err := tc.service.CreateNetwork(t.Context(), "demo", compute.Network{Name: "lan", Kind: "bridge"})
	requireAgentMessage(t, err, `cannot create a "bridge" network in a "ovn" sandbox`)
}

func TestCreateInstanceRejectsPlatformMismatch(t *testing.T) {
	t.Parallel()

	tc := newTestContext(t)
	tc.backend.EXPECT().GetSandbox(mock.Anything, "demo").Return(liveSandbox("demo"), nil)
	image := macosImage()
	_, err := tc.service.CreateInstance(t.Context(), compute.CreateInstance{
		Ref:   compute.Ref{Sandbox: "demo", Name: "web"},
		Image: image,
	})
	requireAgentMessage(t, err, `image "macos/tahoe/desktop" is not available on platform "incus"`)
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

func TestCreateInstanceOVNLeavesHostEmpty(t *testing.T) {
	t.Parallel()

	tc := newTestContext(t)
	box := liveSandbox("demo")
	box.NetworkKind = "ovn"
	box.Host = ""
	pending := mocks.NewMockPendingInstance(t)
	pending.EXPECT().Wait(mock.Anything).Return(runningInstance(), nil)
	tc.backend.EXPECT().GetSandbox(mock.Anything, "demo").Return(box, nil)
	tc.backend.EXPECT().GetInstance(mock.Anything, compute.Ref{Sandbox: "demo", Name: "web"}).
		Return(compute.Instance{}, compute.ErrNotFound)
	tc.backend.EXPECT().
		BeginCreateInstance(mock.Anything, mock.MatchedBy(func(req compute.CreateInstance) bool {
			return req.Host == "" && req.Network == "default"
		})).
		Return(pending, nil)

	_, err := tc.service.CreateInstance(t.Context(), compute.CreateInstance{
		Ref:   compute.Ref{Sandbox: "demo", Name: "web"},
		Image: routerImage(),
		Kind:  "container",
		Start: true,
	})
	require.NoError(t, err)
}

func TestRemoveACLRuleRejectsBaseline(t *testing.T) {
	t.Parallel()

	tc := newTestContext(t)
	for _, id := range []string{compute.BaselineEgressMgmt, compute.BaselineEgressOOB} {
		err := tc.service.RemoveACLRule(t.Context(), "demo", "default", id)
		var agentErr *codemode.AgentError
		require.ErrorAs(t, err, &agentErr)
	}
}

func TestImpairRejectsJitterWithoutLatency(t *testing.T) {
	t.Parallel()

	tc := newTestContext(t)
	err := tc.service.ImpairNIC(t.Context(), compute.Ref{Sandbox: "demo", Name: "web"}, "eth0", compute.Impairment{
		JitterMS: 5,
	})
	requireAgentMessage(t, err, "jitter_ms requires latency_ms")
}

func TestACLAllowsCannotOverrideBaseline(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name      string
		direction string
		dst       string
	}{
		{name: "wildcard", direction: "egress"},
		{name: "supernet", direction: "egress", dst: "10.0.0.0/8"},
		{name: "management subset", direction: "egress", dst: "10.10.10.128/25"},
		{name: "OOB host", direction: "egress", dst: "10.10.70.20"},
		{name: "mapped prefix", direction: "egress", dst: "::ffff:10.10.10.0/120"},
		{name: "unbounded selector", direction: "egress", dst: "@external"},
		{name: "reversed ingress", direction: "ingress"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			tc := newTestContext(t)
			_, err := tc.service.AddACLRule(t.Context(), "demo", "default", compute.ACLRule{
				Direction: test.direction,
				Action:    "allow",
				Dst:       test.dst,
			})
			var agentErr *codemode.AgentError
			require.ErrorAs(t, err, &agentErr)
		})
	}
}
