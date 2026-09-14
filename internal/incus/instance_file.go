package incus

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	incusclient "github.com/lxc/incus/v7/client"
	"github.com/meigma/codemode"

	"github.com/GilmanLab/agentcompute/internal/compute"
)

const defaultFileReadLimit = 64 * 1024

// ReadFile pulls a bounded guest file through the Incus agent.
func (c *Client) ReadFile(ctx context.Context, req compute.FileReadRequest) (compute.FileReadResult, error) {
	if err := c.requireInstance(ctx, req.Ref); err != nil {
		return compute.FileReadResult{}, err
	}
	srv := c.Scoped(ctx, projectName(req.Ref.Sandbox), "")
	body, info, err := srv.GetInstanceFile(req.Ref.Name, req.Path)
	if err != nil {
		if errors.Is(mapError(err), compute.ErrNotFound) {
			return compute.FileReadResult{}, fileErrorf(
				"file not found on instance %q in sandbox %q",
				req.Ref.Name,
				req.Ref.Sandbox,
			)
		}
		return compute.FileReadResult{}, mapError(err)
	}
	if info != nil && info.Type == "directory" {
		if body != nil {
			_ = body.Close()
		}
		return compute.FileReadResult{}, fileErrorf(
			"path is a directory on instance %q in sandbox %q",
			req.Ref.Name,
			req.Ref.Sandbox,
		)
	}
	if body == nil {
		return compute.FileReadResult{}, errors.New("file read returned no content")
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
