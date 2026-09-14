package mcpserver

import (
	"context"

	"github.com/meigma/codemode"
	"github.com/meigma/codemode/authz"

	"github.com/GilmanLab/agentcompute/internal/compute"
	"github.com/GilmanLab/agentcompute/internal/desktop"
)

const (
	capabilityDesktopInfo       = "desktop.info"
	capabilityDesktopEnable     = "desktop.enable"
	capabilityDesktopCall       = "desktop.call"
	capabilityDesktopScreenshot = "desktop.screenshot"
)

type desktopService interface {
	Info(context.Context, compute.Ref) (desktop.Info, error)
	Enable(context.Context, compute.Ref) (bool, error)
	Call(context.Context, compute.Ref, string, string) (desktop.CallResult, error)
	Screenshot(context.Context, compute.Ref, int64, int64, int64) (desktop.Screenshot, error)
}

type desktopInstanceIn struct {
	Sandbox  string `json:"sandbox"`
	Instance string `json:"instance"`
}

type desktopInfoOut struct {
	Ready         bool     `json:"ready"`
	OS            string   `json:"os"`
	DriverVersion string   `json:"driver_version"`
	Tools         []string `json:"tools"`
	VNC           *string  `json:"vnc,omitempty"`
}

type desktopEnableOut struct {
	Ready bool `json:"ready"`
}

type desktopCallIn struct {
	Sandbox  string  `json:"sandbox"`
	Instance string  `json:"instance"`
	Tool     string  `json:"tool"`
	Args     *string `json:"args,omitempty"`
}

type desktopCallOut struct {
	OK            bool    `json:"ok"`
	Summary       string  `json:"summary"`
	Result        string  `json:"result"`
	ScreenshotURL *string `json:"screenshot_url,omitempty"`
}

type desktopScreenshotIn struct {
	Sandbox      string `json:"sandbox"`
	Instance     string `json:"instance"`
	PID          *int64 `json:"pid,omitempty"`
	WindowID     *int64 `json:"window_id,omitempty"`
	MaxDimension *int64 `json:"max_dimension,omitempty"`
}

type desktopScreenshotOut struct {
	URL    string  `json:"url"`
	Width  int64   `json:"width"`
	Height int64   `json:"height"`
	Scale  float64 `json:"scale"`
}

type desktopAPI struct {
	driver desktopService
}

func registerDesktop(builder *codemode.Builder, deps Dependencies) {
	api := desktopAPI{driver: deps.Desktop}
	codemode.Register(builder, codemode.Capability[desktopInstanceIn, desktopInfoOut]{
		ID: capabilityDesktopInfo, Name: capabilityDesktopInfo,
		Summary: "Inspect Driver readiness, version, native tool names, and the human VNC endpoint.",
		Handler: api.info,
	})
	codemode.Register(builder, codemode.Capability[desktopInstanceIn, desktopEnableOut]{
		ID: capabilityDesktopEnable, Name: capabilityDesktopEnable,
		Summary: "Verify the installed Driver daemon answers in the guest graphical session.",
		Handler: api.enable,
	})
	codemode.Register(builder, codemode.Capability[desktopCallIn, desktopCallOut]{
		ID:      capabilityDesktopCall,
		Name:    capabilityDesktopCall,
		Summary: "Call a native Driver tool with a JSON object string; decode result with json.decode. Screenshots return URLs only. Linux token clicks also need pid; snapshot again to verify effects.",
		Handler: api.call,
	})
	codemode.Register(builder, codemode.Capability[desktopScreenshotIn, desktopScreenshotOut]{
		ID:      capabilityDesktopScreenshot,
		Name:    capabilityDesktopScreenshot,
		Summary: "Capture the desktop or a pid/window_id pair without an accessibility tree. Return a PNG URL, dimensions, and image-to-Driver coordinate scale.",
		Handler: api.screenshot,
	})
}

func (api desktopAPI) info(ctx context.Context, _ authz.Subject, in desktopInstanceIn) (desktopInfoOut, error) {
	info, err := api.driver.Info(ctx, compute.Ref{Sandbox: in.Sandbox, Name: in.Instance})
	if err != nil {
		return desktopInfoOut{}, err
	}
	out := desktopInfoOut{Ready: info.Ready, OS: info.OS, DriverVersion: info.DriverVersion, Tools: info.Tools}
	if info.VNC != "" {
		out.VNC = &info.VNC
	}
	return out, nil
}

func (api desktopAPI) enable(ctx context.Context, _ authz.Subject, in desktopInstanceIn) (desktopEnableOut, error) {
	ready, err := api.driver.Enable(ctx, compute.Ref{Sandbox: in.Sandbox, Name: in.Instance})
	return desktopEnableOut{Ready: ready}, err
}

func (api desktopAPI) call(ctx context.Context, _ authz.Subject, in desktopCallIn) (desktopCallOut, error) {
	result, err := api.driver.Call(
		ctx,
		compute.Ref{Sandbox: in.Sandbox, Name: in.Instance},
		in.Tool,
		deref(in.Args, "{}"),
	)
	if err != nil {
		return desktopCallOut{}, err
	}
	out := desktopCallOut{OK: result.OK, Summary: result.Summary, Result: result.Result}
	if result.ScreenshotURL != "" {
		out.ScreenshotURL = &result.ScreenshotURL
	}
	return out, nil
}

func (api desktopAPI) screenshot(
	ctx context.Context,
	_ authz.Subject,
	in desktopScreenshotIn,
) (desktopScreenshotOut, error) {
	shot, err := api.driver.Screenshot(
		ctx,
		compute.Ref{Sandbox: in.Sandbox, Name: in.Instance},
		deref(in.PID, 0),
		deref(in.WindowID, 0),
		deref(in.MaxDimension, 0),
	)
	if err != nil {
		return desktopScreenshotOut{}, err
	}
	return desktopScreenshotOut{
		URL:    shot.URL,
		Width:  int64(shot.Width),
		Height: int64(shot.Height),
		Scale:  shot.Scale,
	}, nil
}
