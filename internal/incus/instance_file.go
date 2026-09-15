package incus

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	incusclient "github.com/lxc/incus/v7/client"
	"github.com/meigma/codemode"

	"github.com/GilmanLab/agentcompute/internal/compute"
)

const defaultFileReadLimit = 64 * 1024

// ReadFile pulls a bounded guest file through the Incus agent.
func (c *Client) ReadFile(ctx context.Context, req compute.FileReadRequest) (compute.FileReadResult, error) {
	body, err := c.ReadBinaryFile(ctx, req.Ref, req.Path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return compute.FileReadResult{}, fileErrorf(
				"file not found on instance %q in sandbox %q",
				req.Ref.Name,
				req.Ref.Sandbox,
			)
		}
		return compute.FileReadResult{}, err
	}
	defer body.Close()

	limit := req.MaxBytes
	if limit <= 0 {
		limit = defaultFileReadLimit
	}
	buf, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil {
		return compute.FileReadResult{}, fmt.Errorf("read file: %w", err)
	}
	truncated := int64(len(buf)) > limit
	if truncated {
		buf = buf[:limit]
	}
	return compute.FileReadResult{Content: string(buf), Truncated: truncated}, nil
}

// ReadBinaryFile opens a guest file without passing bytes through exec or text conversion.
// The caller must close the stream and enforce its own byte bound.
func (c *Client) ReadBinaryFile(ctx context.Context, ref compute.Ref, path string) (io.ReadCloser, error) {
	if err := c.requireInstance(ctx, ref); err != nil {
		return nil, err
	}
	srv := c.Scoped(ctx, projectName(ref.Sandbox), "")
	body, info, err := srv.GetInstanceFile(ref.Name, path)
	if err != nil {
		if errors.Is(mapError(err), compute.ErrNotFound) {
			return nil, fmt.Errorf("guest file missing: %w", os.ErrNotExist)
		}
		return nil, mapError(err)
	}
	if info != nil && info.Type != "file" {
		if body != nil {
			_ = body.Close()
		}
		return nil, fileErrorf("path is not a regular file on instance %q in sandbox %q", ref.Name, ref.Sandbox)
	}
	if body == nil {
		return nil, errors.New("file read returned no content")
	}
	return body, nil
}

// WriteFile pushes a bounded guest file through the Incus agent.
func (c *Client) WriteFile(ctx context.Context, req compute.FileWriteRequest) (compute.FileWriteResult, error) {
	if err := c.requireInstance(ctx, req.Ref); err != nil {
		return compute.FileWriteResult{}, err
	}
	mode := -1
	if req.Mode != "" {
		parsed, err := parseOctalMode(req.Mode)
		if err != nil {
			return compute.FileWriteResult{}, err
		}
		mode = parsed
	}
	err := c.Scoped(ctx, projectName(req.Ref.Sandbox), "").
		CreateInstanceFile(req.Ref.Name, req.Path, incusclient.InstanceFileArgs{
			Content:   strings.NewReader(req.Content),
			UID:       -1,
			GID:       -1,
			Mode:      mode,
			Type:      "file",
			WriteMode: "overwrite",
		})
	if err != nil {
		if errors.Is(mapError(err), compute.ErrNotFound) {
			return compute.FileWriteResult{}, fileErrorf(
				"file not found on instance %q in sandbox %q",
				req.Ref.Name,
				req.Ref.Sandbox,
			)
		}
		return compute.FileWriteResult{}, mapError(err)
	}
	return compute.FileWriteResult{Bytes: int64(len(req.Content))}, nil
}

// DeleteFile removes a guest file through the Incus agent.
func (c *Client) DeleteFile(ctx context.Context, ref compute.Ref, path string) error {
	if err := c.requireInstance(ctx, ref); err != nil {
		return err
	}
	err := c.Scoped(ctx, projectName(ref.Sandbox), "").DeleteInstanceFile(ref.Name, path)
	if err != nil {
		if errors.Is(mapError(err), compute.ErrNotFound) {
			return fmt.Errorf("guest file missing: %w", os.ErrNotExist)
		}
		return mapError(err)
	}
	return nil
}

func parseOctalMode(mode string) (int, error) {
	value, err := strconv.ParseUint(mode, 8, 32)
	if err != nil {
		return 0, fileErrorf("mode %q is not an octal file mode", mode)
	}
	return int(value), nil
}

func fileErrorf(format string, args ...any) error {
	return &codemode.AgentError{Message: fmt.Sprintf(format, args...)}
}
