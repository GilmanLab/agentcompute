package compute_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/GilmanLab/agentcompute/internal/compute"
	"github.com/GilmanLab/agentcompute/internal/compute/mocks"
)

func TestPinRequiresOperatorBeforeBackendAccess(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, subject string
		identities    []string
	}{
		{"other identity", "agent", []string{"omp"}},
		{"disabled", "omp", nil},
		{"exact match", "OMP", []string{"omp"}},
		{"anonymous", "", []string{""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			backend := mocks.NewMockBackend(t)
			service, err := compute.New(backend, nil, compute.Options{PinIdentities: tc.identities})
			require.NoError(t, err)
			_, err = service.CreateSandbox(t.Context(), "keep", time.Minute, tc.subject, "", true)
			requireAgentMessage(t, err, "pinning requires an operator identity")
			for _, pinned := range []bool{true, false} {
				_, err = service.PinSandbox(t.Context(), "keep", pinned, tc.subject)
				requireAgentMessage(t, err, "pinning requires an operator identity")
			}
		})
	}
}

func TestCreatePinPersistsAttributionAndKeepsTTLLimit(t *testing.T) {
	t.Parallel()
	backend := mocks.NewMockBackend(t)
	service, err := compute.New(backend, nil, compute.Options{PinIdentities: []string{"omp"}})
	require.NoError(t, err)
	backend.EXPECT().GetSandbox(mock.Anything, "keep").Return(compute.Sandbox{}, compute.ErrNotFound)
	backend.EXPECT().CreateSandbox(mock.Anything, mock.MatchedBy(func(box compute.Sandbox) bool {
		return box.Pinned && box.PinnedBy == "omp" && box.PinnedAt.Equal(box.CreatedAt) &&
			box.ExpiresAt.Sub(box.CreatedAt) == time.Minute
	})).Return(nil)
	box, err := service.CreateSandbox(t.Context(), "keep", time.Minute, "omp", "", true)
	require.NoError(t, err)
	assert.True(t, box.Pinned)
	_, err = service.CreateSandbox(t.Context(), "too-long", 25*time.Hour, "omp", "", true)
	requireAgentMessage(t, err, "ttl exceeds maximum of 1440 minutes")
}

func TestPinExistingSandboxPreservesCreatorAndExpiry(t *testing.T) {
	t.Parallel()
	backend := mocks.NewMockBackend(t)
	service, err := compute.New(backend, nil, compute.Options{PinIdentities: []string{"omp"}})
	require.NoError(t, err)
	box := liveSandbox("demo")
	box.Subject = "creator"
	backend.EXPECT().GetSandbox(mock.Anything, "demo").Return(box, nil)
	backend.EXPECT().PinSandbox(mock.Anything, "demo", true, "omp", mock.Anything).
		RunAndReturn(func(_ context.Context, _ string, pinned bool, subject string, since time.Time) (compute.Sandbox, error) {
			box.Pinned, box.PinnedBy, box.PinnedAt = pinned, subject, since
			return box, nil
		}).
		Once()
	expiry := box.ExpiresAt
	got, err := service.PinSandbox(t.Context(), "demo", true, "omp")
	require.NoError(t, err)
	assert.True(t, got.Pinned)
	assert.Equal(t, "omp", got.PinnedBy)
	assert.WithinDuration(t, time.Now(), got.PinnedAt, time.Second)
	assert.Equal(t, "creator", got.Subject)
	assert.Equal(t, expiry, got.ExpiresAt)
}

func TestExpiredPinSurvivesScansUntilUnpinned(t *testing.T) {
	t.Parallel()
	for _, platform := range []string{"incus", "mac"} {
		t.Run(platform, func(t *testing.T) {
			t.Parallel()
			backend := mocks.NewMockBackend(t)
			var logs bytes.Buffer
			opts := compute.Options{
				PinIdentities: []string{"omp", "second"},
				Logger:        slog.New(slog.NewTextHandler(&logs, nil)),
			}
			incusBackend := backend
			if platform == "mac" {
				incusBackend = mocks.NewMockBackend(t)
				incusBackend.EXPECT().GetSandbox(mock.Anything, "demo").Return(compute.Sandbox{}, compute.ErrNotFound)
				incusBackend.EXPECT().ListSandboxes(mock.Anything).Return([]compute.Sandbox{}, nil)
				opts.Mac = backend
			}
			service, err := compute.New(incusBackend, nil, opts)
			require.NoError(t, err)
			box := expiredSandbox()
			box.Platform, box.Pinned, box.PinnedBy, box.PinnedAt = platform, true, "omp", time.Now().Add(-time.Hour)
			originalExpiry := box.ExpiresAt
			backend.EXPECT().
				GetSandbox(mock.Anything, "demo").
				RunAndReturn(func(context.Context, string) (compute.Sandbox, error) { return box, nil })
			backend.EXPECT().
				ListSandboxes(mock.Anything).
				RunAndReturn(func(context.Context) ([]compute.Sandbox, error) { return []compute.Sandbox{box}, nil })
			for range 3 {
				require.NoError(t, service.Reap(t.Context()))
			}
			assert.Equal(t, 3, strings.Count(logs.String(), "level=INFO"))
			assert.Equal(t, 3, strings.Count(logs.String(), "expires_at ignored"))
			assert.Contains(t, logs.String(), "pinned_by=omp")
			assert.Contains(t, logs.String(), "since=")
			again, err := service.PinSandbox(t.Context(), "demo", true, "second")
			require.NoError(t, err)
			assert.Equal(t, "omp", again.PinnedBy)
			assert.Equal(t, box.PinnedAt, again.PinnedAt)
			expiry, err := service.SandboxExpiry(t.Context(), "demo")
			require.NoError(t, err)
			assert.True(t, expiry.IsZero())
			backend.EXPECT().PinSandbox(mock.Anything, "demo", false, "", time.Time{}).RunAndReturn(
				func(context.Context, string, bool, string, time.Time) (compute.Sandbox, error) {
					box.Pinned = false
					box.PinnedBy = ""
					box.PinnedAt = time.Time{}
					return box, nil
				},
			).Once()
			unpinned, err := service.PinSandbox(t.Context(), "demo", false, "omp")
			require.NoError(t, err)
			assert.Equal(t, originalExpiry, unpinned.ExpiresAt)
			assert.False(t, unpinned.Pinned)
			backend.EXPECT().DeleteSandbox(mock.Anything, "demo").Return(nil).Once()
			require.NoError(t, service.Reap(t.Context()))
		})
	}
}

func TestReaperRereadsPinBeforeDeleting(t *testing.T) {
	t.Parallel()
	tc := newTestContext(t)
	stale := expiredSandbox()
	pinned := stale
	pinned.Pinned, pinned.PinnedBy, pinned.PinnedAt = true, "omp", time.Now()
	tc.backend.EXPECT().ListSandboxes(mock.Anything).Return([]compute.Sandbox{stale}, nil)
	tc.backend.EXPECT().GetSandbox(mock.Anything, "demo").Return(pinned, nil)
	require.NoError(t, tc.service.Reap(t.Context()))
}

func TestPinnedSandboxCanExtendAfterExpiry(t *testing.T) {
	t.Parallel()
	tc := newTestContext(t)
	box := expiredSandbox()
	box.Pinned, box.PinnedBy = true, "omp"
	tc.backend.EXPECT().GetSandbox(mock.Anything, "demo").Return(box, nil)
	tc.backend.EXPECT().
		ExtendSandbox(mock.Anything, "demo", mock.Anything).
		RunAndReturn(func(_ context.Context, _ string, expiry time.Time) (compute.Sandbox, error) {
			box.ExpiresAt = expiry
			return box, nil
		})
	got, err := tc.service.ExtendSandbox(t.Context(), "demo", time.Hour)
	require.NoError(t, err)
	assert.True(t, got.Pinned)
	assert.WithinDuration(t, time.Now().Add(time.Hour), got.ExpiresAt, time.Second)
}

func TestDeletePinnedSandboxLeavesFailedDeletionReapable(t *testing.T) {
	t.Parallel()
	tc := newTestContext(t)
	box := liveSandbox("demo")
	box.Pinned, box.PinnedBy = true, "omp"
	tc.backend.EXPECT().
		GetSandbox(mock.Anything, "demo").
		RunAndReturn(func(context.Context, string) (compute.Sandbox, error) { return box, nil })
	tc.backend.EXPECT().
		ExtendSandbox(mock.Anything, "demo", mock.Anything).
		RunAndReturn(func(_ context.Context, _ string, expiry time.Time) (compute.Sandbox, error) {
			box.ExpiresAt = expiry
			return box, nil
		}).
		Once()
	tc.backend.EXPECT().
		PinSandbox(mock.Anything, "demo", false, "", time.Time{}).
		RunAndReturn(func(context.Context, string, bool, string, time.Time) (compute.Sandbox, error) {
			box.Pinned = false
			return box, nil
		}).
		Once()
	tc.backend.EXPECT().DeleteSandbox(mock.Anything, "demo").Return(errors.New("temporary failure")).Once()
	require.Error(t, tc.service.DeleteSandbox(t.Context(), "demo"))
	tc.backend.EXPECT().ListSandboxes(mock.Anything).Return([]compute.Sandbox{box}, nil).Once()
	tc.backend.EXPECT().DeleteSandbox(mock.Anything, "demo").Return(nil).Once()
	require.NoError(t, tc.service.Reap(t.Context()))
}
