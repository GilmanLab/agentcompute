package incus

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRouterCIDRUsesFirstHost(t *testing.T) {
	t.Parallel()

	got, err := routerCIDR("192.168.50.0/24")
	require.NoError(t, err)
	assert.Equal(t, "192.168.50.1/24", got)

	got, err = routerCIDR("192.168.50.1/24")
	require.NoError(t, err)
	assert.Equal(t, "192.168.50.1/24", got)
}

func TestParseIPv4RangeCIDR(t *testing.T) {
	t.Parallel()

	start, end, err := parseIPv4Range("10.10.40.64/26")
	require.NoError(t, err)
	assert.Equal(t, "10.10.40.64", start.String())
	assert.Equal(t, "10.10.40.127", end.String())
}

func TestParseIPv4RangeHyphen(t *testing.T) {
	t.Parallel()

	start, end, err := parseIPv4Range("10.10.40.64-10.10.40.127")
	require.NoError(t, err)
	assert.Equal(t, "10.10.40.64", start.String())
	assert.Equal(t, "10.10.40.127", end.String())
}
