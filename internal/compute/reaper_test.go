package compute_test

import (
	"errors"
	"testing"

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
