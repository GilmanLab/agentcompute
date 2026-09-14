//go:build integration

package cli

import (
	"bytes"
	"context"
	"fmt"
	"image"
	_ "image/png"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GilmanLab/agentcompute/internal/incus"
)

// TestDesktopAcceptance drives real Driver tools through the production stdio
// MCP server. The representative client never receives an uplink-facing NIC;
// a separate viewer guest verifies the human VNC endpoint and reboot recovery.
func TestDesktopAcceptance(t *testing.T) {
	fx := loadOVNFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 12*time.Minute)
	defer cancel()
	root, dir := integrationRoot(t), t.TempDir()
	binary := buildAgentcompute(ctx, t, root, dir)
	config := writeOVNConfig(t, dir, root, fx)
	imageName := envOr("AGENTCOMPUTE_TEST_DESKTOP_IMAGE", "ubuntu/24.04/desktop")
	name := fmt.Sprintf("desktop-%x", time.Now().UnixNano())
	backend, err := incus.New(ctx, incus.Options{Remote: fx.Remote, Pool: "data", OVNUplink: fx.Uplink, OVNRanges: fx.Ranges})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, backend.Close()) })
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 2*time.Minute)
		defer stop()
		if _, err := backend.GetSandbox(cleanup, name); err == nil {
			assert.NoError(t, backend.DeleteSandbox(cleanup, name))
		}
	})
	session, _ := startClusterClient(ctx, t, binary, config,
		"sandbox.create", "sandbox.extend", "instance.create", "instance.get", "instance.exec",
		"instance.wait", "instance.restart", "net.create", "net.attach", "net.forward",
		"desktop.info", "desktop.enable", "desktop.call", "desktop.screenshot")

	started := time.Now()
	representative := integrationExecute(ctx, t, session, fmt.Sprintf(`def main():
    name = %q
    sandbox.create(name=name, ttl_minutes=10)
    net.create(sandbox=name, name="lan", cidr="192.168.50.0/24", nat=False)
    net.create(sandbox=name, name="wan", cidr="10.99.0.0/24", nat=True)
    instance.create(sandbox=name, name="rtr", image="router", network="lan")
    net.attach(sandbox=name, instance="rtr", network="wan")
    nat = instance.exec(sandbox=name, name="rtr", command="/opt/router/nat --mode port-restricted --inside eth0 --outside eth1")
    if nat["exit_code"] != 0:
        fail(nat["stderr"])
    instance.create(sandbox=name, name="client", image=%q, network="lan")
    waited = instance.wait(sandbox=name, name="client", until="desktop", timeout_seconds=300)
    shot = desktop.screenshot(sandbox=name, instance="client")
    called = desktop.call(sandbox=name, instance="client", tool="list_apps")
    if not called["ok"]:
        fail(called["summary"])
    return {"router": instance.get(sandbox=name, name="rtr"), "client": instance.get(sandbox=name, name="client"), "screenshot": shot, "apps": json.decode(called["result"]), "waited": waited}
`, name, imageName))
	t.Logf("representative desktop program: %s", time.Since(started))
	clientNICs := asSlice(t, asMap(t, representative["client"])["nics"])
	require.Len(t, clientNICs, 1, "the representative client must remain private-only")
	assert.Equal(t, "lan", asMap(t, clientNICs[0])["network"])
	apps := asSlice(t, asMap(t, representative["apps"])["apps"])
	var editorInstalled bool
	for _, app := range apps {
		if asMap(t, app)["bundle_id"] == "org.gnome.TextEditor" {
			editorInstalled = true
		}
	}
	require.True(t, editorInstalled, "native list_apps must expose the installed editor")
	shot := asMap(t, representative["screenshot"])
	originalURL := asString(t, shot["url"])
	_, dimensions := fetchDesktopPNG(t, originalURL)
	assert.Equal(t, jsonInt(t, shot["width"]), int64(dimensions.Width))
	assert.Equal(t, jsonInt(t, shot["height"]), int64(dimensions.Height))

	interaction := integrationExecute(ctx, t, session, fmt.Sprintf(`def main():
    name = %q
    launched = desktop.call(sandbox=name, instance="client", tool="launch_app", args=json.encode({"name":"gnome-text-editor"}))
    if not launched["ok"]:
        fail(launched["summary"])
    app = json.decode(launched["result"])
    pid = app["pid"]
    window = app["windows"][0]["window_id"]
    args = json.encode({"pid":pid,"window_id":window,"include_accessibility_tree":True})
    before_call = desktop.call(sandbox=name, instance="client", tool="get_window_state", args=args)
    if not before_call["ok"]:
        fail(before_call["summary"])
    before = json.decode(before_call["result"])
    token = ""
    for element in before["elements"]:
        if element.get("label") == "New tab" and "click" in element.get("actions", []):
            token = element["element_token"]
            break
    if not token:
        fail("editor snapshot has no actionable New tab element")
    clicked = desktop.call(sandbox=name, instance="client", tool="click", args=json.encode({"pid":pid,"window_id":window,"element_token":token,"delivery_mode":"foreground"}))
    if not clicked["ok"]:
        fail(clicked["summary"])
    after_call = desktop.call(sandbox=name, instance="client", tool="get_window_state", args=args)
    if not after_call["ok"]:
        fail(after_call["summary"])
    after = json.decode(after_call["result"])
    return {"before":before["tree_markdown"], "after":after["tree_markdown"], "before_url":before_call["screenshot_url"], "after_url":after_call["screenshot_url"], "click":json.decode(clicked["result"]), "scaled":desktop.screenshot(sandbox=name, instance="client", max_dimension=640), "window_scaled":desktop.screenshot(sandbox=name, instance="client", pid=pid, window_id=window, max_dimension=640), "window_width":before["window_bounds"]["width"]}
`, name))
	assert.Equal(t, 1, strings.Count(asString(t, interaction["before"]), `tab panel = "New Document"`))
	assert.Equal(t, 2, strings.Count(asString(t, interaction["after"]), `tab panel = "New Document"`))
	beforePNG, _ := fetchDesktopPNG(t, asString(t, interaction["before_url"]))
	afterPNG, _ := fetchDesktopPNG(t, asString(t, interaction["after_url"]))
	assert.False(t, bytes.Equal(beforePNG, afterPNG), "the verified second tab must also change the screenshot")
	scaled := asMap(t, interaction["scaled"])
	_, scaledDimensions := fetchDesktopPNG(t, asString(t, scaled["url"]))
	assert.LessOrEqual(t, scaledDimensions.Width, 640)
	assert.LessOrEqual(t, scaledDimensions.Height, 640)
	windowScaled := asMap(t, interaction["window_scaled"])
	_, windowDimensions := fetchDesktopPNG(t, asString(t, windowScaled["url"]))
	assert.LessOrEqual(t, max(windowDimensions.Width, windowDimensions.Height), 640)
	assert.InDelta(t, float64(jsonInt(t, interaction["window_width"]))/float64(windowDimensions.Width), windowScaled["scale"], 1e-9)
	t.Logf("token click native outcome (state verified separately): %v", interaction["click"])
	if evidence := os.Getenv("AGENTCOMPUTE_TEST_DESKTOP_EVIDENCE"); evidence != "" {
		require.NoError(t, os.MkdirAll(evidence, 0o700))
		require.NoError(t, os.WriteFile(evidence+"/before.png", beforePNG, 0o600))
		require.NoError(t, os.WriteFile(evidence+"/after.png", afterPNG, 0o600))
	}

	viewer := integrationExecute(ctx, t, session, fmt.Sprintf(`def main():
    name = %q
    instance.create(sandbox=name, name="viewer", image=%q)
    enabled = desktop.enable(sandbox=name, instance="viewer")
    forward = net.forward(sandbox=name, network="default", instance="viewer", port=5900, listen_port=5900, protocol="tcp")
    before = desktop.info(sandbox=name, instance="viewer")
    instance.restart(sandbox=name, name="viewer")
    waited = instance.wait(sandbox=name, name="viewer", until="desktop", timeout_seconds=300)
    after = desktop.info(sandbox=name, instance="viewer")
    return {"enabled":enabled,"forward":forward,"before":before,"after":after,"waited":waited,"private_client":instance.get(sandbox=name, name="client")}
`, name, imageName))
	afterInfo := asMap(t, viewer["after"])
	assert.True(t, jsonBool(t, asMap(t, viewer["enabled"])["ready"]))
	assert.True(t, jsonBool(t, afterInfo["ready"]))
	assert.Equal(t, "0.28.1", afterInfo["driver_version"])
	assert.Contains(t, asSlice(t, afterInfo["tools"]), "get_window_state")
	assert.Len(t, asSlice(t, asMap(t, viewer["private_client"])["nics"]), 1)
	forward := asMap(t, viewer["forward"])
	vncAddress := asString(t, afterInfo["vnc"])
	assert.Equal(t, net.JoinHostPort(asString(t, forward["address"]), fmt.Sprint(jsonInt(t, forward["port"]))), vncAddress)
	vnc, err := net.DialTimeout("tcp", vncAddress, 10*time.Second)
	require.NoError(t, err)
	defer vnc.Close()
	require.NoError(t, vnc.SetReadDeadline(time.Now().Add(10*time.Second)))
	banner := make([]byte, 12)
	_, err = io.ReadFull(vnc, banner)
	require.NoError(t, err)
	assert.Equal(t, "RFB 003.008\n", string(banner))
	t.Logf("reboot ready and VNC reachable: %s", vncAddress)

	// Shorten the sandbox lifetime after publication. Its first screenshot was
	// initially retained for five minutes, so only lifecycle purge can make it
	// disappear at the newly shortened expiry.
	extended := integrationExecute(ctx, t, session, fmt.Sprintf(`def main():
    return sandbox.extend(name=%q, ttl_minutes=1)
`, name))
	expires, err := time.Parse(time.RFC3339, asString(t, extended["expires_at"]))
	require.NoError(t, err)
	httpClient := &http.Client{Timeout: 5 * time.Second}
	require.Eventually(t, func() bool {
		response, err := httpClient.Get(originalURL)
		if err != nil {
			return false
		}
		defer response.Body.Close()
		return response.StatusCode == http.StatusNotFound
	}, 100*time.Second, time.Second, "expired sandbox screenshot must be purged by the running reaper")
	t.Logf("screenshot 404 observed %s after shortened sandbox expiry", time.Since(expires))
}

func fetchDesktopPNG(t *testing.T, url string) ([]byte, image.Config) {
	t.Helper()
	client := &http.Client{Timeout: 15 * time.Second}
	response, err := client.Get(url)
	require.NoError(t, err)
	defer response.Body.Close()
	require.Equal(t, http.StatusOK, response.StatusCode)
	assert.Equal(t, "image/png", response.Header.Get("Content-Type"))
	assert.Equal(t, "nosniff", response.Header.Get("X-Content-Type-Options"))
	assert.Equal(t, "no-store", response.Header.Get("Cache-Control"))
	data, err := io.ReadAll(io.LimitReader(response.Body, (16<<20)+1))
	require.NoError(t, err)
	require.LessOrEqual(t, len(data), 16<<20)
	decoded, format, err := image.Decode(bytes.NewReader(data))
	require.NoError(t, err)
	require.Equal(t, "png", format)
	bounds := decoded.Bounds()
	return data, image.Config{Width: bounds.Dx(), Height: bounds.Dy()}
}
