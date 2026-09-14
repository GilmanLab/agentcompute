package desktop

import (
	"bytes"
	"encoding/json"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/meigma/codemode"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func fixture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	require.NoError(t, err)
	return string(data)
}

func TestClassifyCallPinnedFixtures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		file         string
		exitCode     int64
		wantOK       bool
		wantExact    string
		assertResult func(t *testing.T, got CallResult)
	}{
		{
			name:      "exit-zero plain-text missing pid fails closed",
			file:      "click-missing-pid.stdout",
			exitCode:  0,
			wantOK:    false,
			wantExact: "Missing required integer field: pid",
		},
		{
			name:     "valid click preserves native fields without claiming effect",
			file:     "click-with-pid.stdout",
			exitCode: 0,
			wantOK:   true,
			assertResult: func(t *testing.T, got CallResult) {
				t.Helper()
				var body map[string]any
				require.NoError(t, json.Unmarshal([]byte(got.Result), &body))
				assert.Equal(t, "unverifiable", body["effect"])
				assert.Equal(t, "accessibility", body["route"])
				delivery, ok := body["delivery"].(map[string]any)
				require.True(t, ok)
				assert.Equal(t, "background", delivery["mode"])
			},
		},
		{
			name:     "snapshot preserves element tokens",
			file:     "snapshot-tree.stdout",
			exitCode: 0,
			wantOK:   true,
			assertResult: func(t *testing.T, got CallResult) {
				t.Helper()
				var body map[string]any
				require.NoError(t, json.Unmarshal([]byte(got.Result), &body))
				assert.Equal(t, "s00000001", body["snapshot_id"])
				elements, ok := body["elements"].([]any)
				require.True(t, ok)
				require.NotEmpty(t, elements)
				first, ok := elements[0].(map[string]any)
				require.True(t, ok)
				assert.Equal(t, "s00000001:0", first["element_token"])
				var token string
				for _, raw := range elements {
					el, ok := raw.(map[string]any)
					require.True(t, ok)
					if el["label"] == "New tab" {
						token, _ = el["element_token"].(string)
					}
				}
				assert.Equal(t, "s00000001:5", token)
			},
		},
		{
			name:     "desktop state is structured content",
			file:     "desktop.stdout",
			exitCode: 0,
			wantOK:   true,
			assertResult: func(t *testing.T, got CallResult) {
				t.Helper()
				var body map[string]any
				require.NoError(t, json.Unmarshal([]byte(got.Result), &body))
				assert.Equal(t, "linux", body["platform"])
				assert.EqualValues(t, 1280, body["screen_width"])
				assert.EqualValues(t, 800, body["screen_height"])
				assert.NotContains(t, got.Result, "screenshot_png_b64")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := classifyCall(tt.exitCode, fixture(t, tt.file), "")
			assert.Equal(t, tt.wantOK, got.OK)
			if tt.wantExact != "" {
				assert.Equal(t, tt.wantExact, got.Summary)
				assert.JSONEq(t, "null", got.Result)
			}
			if tt.assertResult != nil {
				tt.assertResult(t, got)
			}
		})
	}
}

func TestClassifyCallPreservesIntegerPrecision(t *testing.T) {
	t.Parallel()
	got := classifyCall(0, `{"window_id":18446744073709551615}`, "")
	require.True(t, got.OK)
	var result struct {
		WindowID uint64 `json:"window_id"`
	}
	require.NoError(t, json.Unmarshal([]byte(got.Result), &result))
	assert.Equal(t, ^uint64(0), result.WindowID)
}

func TestClassifyCallSafetyAndAmbiguity(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		exitCode int64
		stdout   string
		stderr   string
		wantOK   bool
		wantSum  string
	}{
		{
			name:     "native [OK] text is success",
			exitCode: 0,
			stdout:   "[OK]\n",
			wantOK:   true,
			wantSum:  "[OK]",
		},
		{
			name:     "native [OK] with trailing text is success",
			exitCode: 0,
			stdout:   "[OK] done",
			wantOK:   true,
			wantSum:  "[OK] done",
		},
		{
			name:     "ambiguous non-JSON text fails closed with exact diagnostic",
			exitCode: 0,
			stdout:   "something went wrong maybe",
			wantOK:   false,
			wantSum:  "something went wrong maybe",
		},
		{
			name:     "JSON string is not structured content",
			exitCode: 0,
			stdout:   `"Missing required integer field: pid"`,
			wantOK:   false,
			wantSum:  `"Missing required integer field: pid"`,
		},
		{
			name:     "nonzero exit prefers stderr diagnostic",
			exitCode: 1,
			stdout:   "ignored",
			stderr:   "Cua Driver daemon is not running on /run/user/1000/cua-driver.sock.",
			wantOK:   false,
			wantSum:  "Cua Driver daemon is not running on /run/user/1000/cua-driver.sock.",
		},
		{
			name:     "empty exit-zero output fails closed",
			exitCode: 0,
			wantOK:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := classifyCall(tt.exitCode, tt.stdout, tt.stderr)
			assert.Equal(t, tt.wantOK, got.OK)
			assert.Equal(t, tt.wantSum, got.Summary)
			if !tt.wantOK {
				assert.JSONEq(t, "null", got.Result)
			}
		})
	}
}

func TestStripImagesRemovesKnownBase64Fields(t *testing.T) {
	t.Parallel()

	payload := strings.Repeat("A", 600)
	raw := map[string]any{
		"element_token":      "s00000001:5",
		"snapshot_id":        "s00000001",
		"screenshot_png_b64": payload,
		"screenshot_width":   float64(822),
		"content": []any{
			map[string]any{"type": "image", "data": payload, "mimeType": "image/png"},
			map[string]any{"type": "text", "text": "hello"},
		},
		"note": "data:image/png;base64," + payload,
	}

	got := stripImages(raw).(map[string]any)
	assert.Equal(t, "s00000001:5", got["element_token"])
	assert.Equal(t, "s00000001", got["snapshot_id"])
	assert.EqualValues(t, 822, got["screenshot_width"])
	assert.NotContains(t, got, "screenshot_png_b64")
	content := got["content"].([]any)
	imageBlock := content[0].(map[string]any)
	assert.NotContains(t, imageBlock, "data")
	assert.Equal(t, "image/png", imageBlock["mimeType"])
	assert.NotContains(t, got, "note")

	encoded, err := marshalJSON(got)
	require.NoError(t, err)
	assert.NotContains(t, encoded, payload)
	assert.NotContains(t, encoded, "screenshot_png_b64")
}

func TestParseDumpDocsUsesPinnedCatalog(t *testing.T) {
	t.Parallel()

	version, tools, err := parseDumpDocs([]byte(fixture(t, "dump-docs.json")))
	require.NoError(t, err)
	assert.Equal(t, "0.28.1", version)
	assert.Contains(t, tools, "get_desktop_state")
	assert.Contains(t, tools, "get_window_state")
	assert.Contains(t, tools, "click")
	assert.Contains(t, tools, "list_apps")
	assert.Equal(t, "list_apps", tools[0])
	assert.NotContains(t, tools, "")
}

func TestNormalizeArgsAndScreenshotValidation(t *testing.T) {
	t.Parallel()

	payload, err := normalizeArgs("")
	require.NoError(t, err)
	assert.Equal(t, "{}", payload)

	payload, err = normalizeArgs(`{"element_token":"s00000001:5","pid":1220}`)
	require.NoError(t, err)
	assert.Equal(t, `{"element_token":"s00000001:5","pid":1220}`, payload)

	_, err = normalizeArgs(`["click"]`)
	requireAgentMessage(t, err, "args must be a JSON object")
	_, err = normalizeArgs("not-json")
	requireAgentMessage(t, err, "args must be a JSON object")

	require.NoError(t, validateScreenshotArgs(0, 0, 0))
	require.NoError(t, validateScreenshotArgs(1220, 31457284, 1280))
	requireAgentMessage(t, validateScreenshotArgs(1220, 0, 0), "pid and window_id must both be set or both omitted")
	requireAgentMessage(t, validateScreenshotArgs(0, 31457284, 0), "pid and window_id must both be set or both omitted")
	requireAgentMessage(t, validateScreenshotArgs(0, 0, -1), "max_dimension must be a positive integer")
}

func TestBoundImageHonorsMaxDimension(t *testing.T) {
	t.Parallel()

	src := image.NewNRGBA(image.Rect(0, 0, 1280, 800))
	unchanged := boundImage(src, 0)
	assert.Equal(t, src, unchanged)
	unchanged = boundImage(src, 1280)
	assert.Equal(t, src, unchanged)

	got := boundImage(src, 640)
	assert.Equal(t, 640, got.Bounds().Dx())
	assert.Equal(t, 400, got.Bounds().Dy())
}

func TestPublishBoundedPNGSetsCoordinateScale(t *testing.T) {
	t.Parallel()

	store, err := NewStore(t.TempDir(), "https://shots.example")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })

	var buf bytes.Buffer
	require.NoError(t, png.Encode(&buf, image.NewNRGBA(image.Rect(0, 0, 100, 50))))
	shot, err := publishBoundedPNG(store, "demo", time.Now().Add(time.Hour), buf.Bytes(), 40)
	require.NoError(t, err)
	assert.Equal(t, 40, shot.Width)
	assert.Equal(t, 20, shot.Height)
	assert.InDelta(t, 2.5, shot.Scale, 1e-9)
	assert.NotEmpty(t, shot.URL)
}

func requireAgentMessage(t *testing.T, err error, message string) {
	t.Helper()
	require.Error(t, err)
	var agent *codemode.AgentError
	require.ErrorAs(t, err, &agent)
	assert.Equal(t, message, agent.Message)
}
