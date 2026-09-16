package lume

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GilmanLab/agentcompute/internal/compute"
)

func TestSandboxPinSidecarSurvivesClientRestart(t *testing.T) {
	t.Parallel()
	client := newTunneledClient(t, http.NotFoundHandler())
	now := time.Now().UTC()
	box := compute.Sandbox{
		Name:      "keep",
		Platform:  "mac",
		Subject:   "creator",
		CreatedAt: now.Add(-time.Hour),
		ExpiresAt: now.Add(-time.Minute),
		Pinned:    true,
		PinnedBy:  "omp",
		PinnedAt:  now.Add(-time.Hour),
	}
	require.NoError(t, client.CreateSandbox(t.Context(), box))
	opts := client.opts
	require.NoError(t, client.Close())
	restarted, err := New(t.Context(), opts)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, restarted.Close()) })
	boxes, err := restarted.ListSandboxes(t.Context())
	require.NoError(t, err)
	require.Len(t, boxes, 1)
	assert.True(t, boxes[0].Pinned)
	assert.Equal(t, "omp", boxes[0].PinnedBy)
	assert.True(t, box.PinnedAt.Equal(boxes[0].PinnedAt))
	assert.True(t, box.ExpiresAt.Equal(boxes[0].ExpiresAt))
	unpinned, err := restarted.PinSandbox(t.Context(), "keep", false, "", time.Time{})
	require.NoError(t, err)
	assert.False(t, unpinned.Pinned)
	assert.Empty(t, unpinned.PinnedBy)
	assert.True(t, unpinned.PinnedAt.IsZero())
	assert.True(t, box.ExpiresAt.Equal(unpinned.ExpiresAt))
	repinned, err := restarted.PinSandbox(t.Context(), "keep", true, "second", now)
	require.NoError(t, err)
	assert.True(t, repinned.Pinned)
	assert.Equal(t, "second", repinned.PinnedBy)
	assert.True(t, now.Equal(repinned.PinnedAt))
	assert.True(t, box.ExpiresAt.Equal(repinned.ExpiresAt))
	assert.Equal(t, "creator", repinned.Subject)
}
