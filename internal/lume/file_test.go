package lume

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/meigma/codemode"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GilmanLab/agentcompute/internal/compute"
)

func TestReadWriteDeleteRoundTrip(t *testing.T) {
	t.Parallel()
	gt := newGuestTransport(t)
	path := filepath.Join(gt.root, "note.txt")
	result, err := gt.client.WriteFile(t.Context(), compute.FileWriteRequest{
		Ref:     gt.ref,
		Path:    path,
		Content: "hello world",
		Mode:    "0640",
	})
	require.NoError(t, err)
	assert.Equal(t, int64(11), result.Bytes)
	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "hello world", string(got))
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o640), info.Mode().Perm())

	read, err := gt.client.ReadFile(t.Context(), compute.FileReadRequest{Ref: gt.ref, Path: path})
	require.NoError(t, err)
	assert.Equal(t, "hello world", read.Content)
	assert.False(t, read.Truncated)

	require.NoError(t, gt.client.DeleteFile(t.Context(), gt.ref, path))
	_, err = os.Stat(path)
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestReadFileTruncatesWithoutWaiting(t *testing.T) {
	t.Parallel()
	gt := newGuestTransport(t)
	path := filepath.Join(gt.root, "stream")
	require.NoError(t, os.WriteFile(path, bytes.Repeat([]byte("0123456789"), 1024*64), 0o644))
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	result, err := gt.client.ReadFile(ctx, compute.FileReadRequest{
		Ref:      gt.ref,
		Path:     path,
		MaxBytes: 8,
	})
	require.NoError(t, err)
	require.NoError(t, ctx.Err(), "bounded reads must not wait for the rest of the file")
	assert.Equal(t, "01234567", result.Content)
	assert.True(t, result.Truncated)
}

func TestReadFileMissingIsNotExist(t *testing.T) {
	t.Parallel()
	gt := newGuestTransport(t)
	path := filepath.Join(gt.root, "missing")
	_, err := gt.client.ReadFile(t.Context(), compute.FileReadRequest{Ref: gt.ref, Path: path})
	require.Error(t, err)
	require.NotErrorIs(t, err, os.ErrNotExist)
	var agentErr *codemode.AgentError
	require.ErrorAs(t, err, &agentErr)

	_, err = gt.client.ReadBinaryFile(t.Context(), gt.ref, path)
	require.ErrorIs(t, err, os.ErrNotExist)

	err = gt.client.DeleteFile(t.Context(), gt.ref, path)
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestReadFilePermissionDeniedIsNotMissing(t *testing.T) {
	t.Parallel()
	gt := newGuestTransport(t)
	dir := filepath.Join(gt.root, "denied")
	require.NoError(t, os.Mkdir(dir, 0o000))
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	path := filepath.Join(dir, "secret")
	_, err := gt.client.ReadBinaryFile(t.Context(), gt.ref, path)
	require.Error(t, err)
	assert.NotErrorIs(t, err, os.ErrNotExist, "permission errors must not look like missing files")
}

func TestReadWriteLiteralGlobPath(t *testing.T) {
	t.Parallel()
	gt := newGuestTransport(t)
	literal := filepath.Join(gt.root, "foo*")
	neighbor := filepath.Join(gt.root, "foobar")
	require.NoError(t, os.WriteFile(neighbor, []byte("neighbor"), 0o644))
	_, err := gt.client.WriteFile(t.Context(), compute.FileWriteRequest{
		Ref:     gt.ref,
		Path:    literal,
		Content: "literal-glob",
	})
	require.NoError(t, err)
	got, err := os.ReadFile(literal)
	require.NoError(t, err)
	assert.Equal(t, "literal-glob", string(got))
	neighborGot, err := os.ReadFile(neighbor)
	require.NoError(t, err)
	assert.Equal(t, "neighbor", string(neighborGot))

	read, err := gt.client.ReadFile(t.Context(), compute.FileReadRequest{Ref: gt.ref, Path: literal})
	require.NoError(t, err)
	assert.Equal(t, "literal-glob", read.Content)
	require.NoError(t, gt.client.DeleteFile(t.Context(), gt.ref, literal))
	_, err = os.Stat(literal)
	require.ErrorIs(t, err, os.ErrNotExist)
	neighborGot, err = os.ReadFile(neighbor)
	require.NoError(t, err)
	assert.Equal(t, "neighbor", string(neighborGot))
}

func TestWriteFileRejectsDirectory(t *testing.T) {
	t.Parallel()
	gt := newGuestTransport(t)
	dir := filepath.Join(gt.root, "outdir")
	require.NoError(t, os.Mkdir(dir, 0o755))
	_, err := gt.client.WriteFile(t.Context(), compute.FileWriteRequest{
		Ref:     gt.ref,
		Path:    dir,
		Content: "nope",
	})
	require.Error(t, err)
	var agentErr *codemode.AgentError
	require.ErrorAs(t, err, &agentErr)
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Empty(t, entries, "writing a directory must not create a child file")
}

func TestReadBinaryFileStreamsBytes(t *testing.T) {
	t.Parallel()
	gt := newGuestTransport(t)
	path := filepath.Join(gt.root, "blob")
	require.NoError(t, os.WriteFile(path, []byte{0x00, 0xff, 'b', 'i', 'n'}, 0o644))
	body, err := gt.client.ReadBinaryFile(t.Context(), gt.ref, path)
	require.NoError(t, err)
	defer body.Close()
	got, err := io.ReadAll(body)
	require.NoError(t, err)
	assert.Equal(t, []byte{0x00, 0xff, 'b', 'i', 'n'}, got)
}

func TestFilePathValidation(t *testing.T) {
	t.Parallel()
	gt := newGuestTransport(t)
	_, err := gt.client.ReadFile(t.Context(), compute.FileReadRequest{
		Ref:  gt.ref,
		Path: "relative",
	})
	require.Error(t, err)
	_, err = gt.client.WriteFile(t.Context(), compute.FileWriteRequest{
		Ref:  gt.ref,
		Path: filepath.Join(gt.root, "x"),
		Mode: "xyz",
	})
	require.Error(t, err)
}
