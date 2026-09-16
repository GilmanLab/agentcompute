package compute_test

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/GilmanLab/agentcompute/internal/compute"
)

func TestReapRetriesPartialDelete(t *testing.T) {
	t.Parallel()

	tc := newTestContext(t)
	expired := expiredSandbox()
	live := liveSandbox("keep")

	tc.backend.EXPECT().ListSandboxes(mock.Anything).Return([]compute.Sandbox{expired, live}, nil).Times(2)
	tc.backend.EXPECT().GetSandbox(mock.Anything, "demo").Return(expired, nil).Times(2)
	tc.backend.EXPECT().DeleteSandbox(mock.Anything, "demo").Return(errors.New("partial delete")).Once()
	tc.backend.EXPECT().DeleteSandbox(mock.Anything, "demo").Return(nil).Once()

	err := tc.service.Reap(t.Context())
	require.Error(t, err)

	require.NoError(t, tc.service.Reap(t.Context()))
}

func TestReapSkipsUnexpiredAfterReread(t *testing.T) {
	t.Parallel()

	tc := newTestContext(t)
	listed := expiredSandbox()
	tc.backend.EXPECT().ListSandboxes(mock.Anything).Return([]compute.Sandbox{listed}, nil)
	tc.backend.EXPECT().GetSandbox(mock.Anything, "demo").Return(liveSandbox("demo"), nil)

	require.NoError(t, tc.service.Reap(t.Context()))
}

func TestReapScansBothBackends(t *testing.T) {
	t.Parallel()

	tc := newMixedContext(t)
	incusExpired := expiredSandbox()
	incusExpired.Name = "incus-box"
	macExpired := liveMacSandbox("mac-box")
	macExpired.ExpiresAt = time.Now().Add(-time.Minute)

	tc.incus.EXPECT().ListSandboxes(mock.Anything).Return([]compute.Sandbox{incusExpired}, nil)
	tc.mac.EXPECT().ListSandboxes(mock.Anything).Return([]compute.Sandbox{macExpired}, nil)
	tc.incus.EXPECT().GetSandbox(mock.Anything, "incus-box").Return(incusExpired, nil).Times(2)
	tc.incus.EXPECT().DeleteSandbox(mock.Anything, "incus-box").Return(nil)
	tc.incus.EXPECT().GetSandbox(mock.Anything, "mac-box").Return(compute.Sandbox{}, compute.ErrNotFound).Times(2)
	tc.mac.EXPECT().GetSandbox(mock.Anything, "mac-box").Return(macExpired, nil).Times(2)
	tc.mac.EXPECT().DeleteSandbox(mock.Anything, "mac-box").Return(nil)

	require.NoError(t, tc.service.Reap(t.Context()))
}
