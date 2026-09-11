package compute

import (
	"bytes"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDrainingWriterCapsAndKeepsAccepting(t *testing.T) {
	t.Parallel()

	writer := newDrainingWriter(execOutputLimit)
	first := bytes.Repeat([]byte("a"), execOutputLimit)
	wrote, err := writer.Write(first)
	require.NoError(t, err)
	assert.Equal(t, execOutputLimit, wrote)
	assert.False(t, writer.Truncated())
	assert.Len(t, writer.String(), execOutputLimit)

	overflow := bytes.Repeat([]byte("b"), 128)
	wrote, err = writer.Write(overflow)
	require.NoError(t, err)
	assert.Equal(t, len(overflow), wrote, "overflow must be drained, not blocked")
	assert.True(t, writer.Truncated())
	assert.Equal(t, string(first), writer.String())

	wrote, err = writer.Write([]byte("more"))
	require.NoError(t, err)
	assert.Equal(t, 4, wrote)
	assert.True(t, writer.Truncated())
}

func TestDrainingWriterTruncatesMidWrite(t *testing.T) {
	t.Parallel()

	writer := newDrainingWriter(8)
	wrote, err := writer.Write([]byte("abcdefghijkl"))
	require.NoError(t, err)
	assert.Equal(t, 12, wrote)
	assert.True(t, writer.Truncated())
	assert.Equal(t, "abcdefgh", writer.String())
}

func TestDrainingWriterImplementsWriter(t *testing.T) {
	t.Parallel()

	var writer io.Writer = newDrainingWriter(4)
	n, err := writer.Write([]byte("hello"))
	require.NoError(t, err)
	assert.Equal(t, 5, n)
}
