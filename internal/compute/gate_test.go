package compute

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGateLockHonorsCancellation(t *testing.T) {
	t.Parallel()

	g := newGate()
	unlock, err := g.Lock(t.Context(), "demo")
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()

	_, err = g.Lock(ctx, "demo")
	require.ErrorIs(t, err, context.DeadlineExceeded)

	unlock()
	unlock, err = g.Lock(t.Context(), "demo")
	require.NoError(t, err)
	unlock()
}

func TestGateAllowsDistinctKeys(t *testing.T) {
	t.Parallel()

	g := newGate()
	first, err := g.Lock(t.Context(), "a")
	require.NoError(t, err)
	second, err := g.Lock(t.Context(), "b")
	require.NoError(t, err)
	assert.NotNil(t, first)
	assert.NotNil(t, second)
	first()
	second()
}
