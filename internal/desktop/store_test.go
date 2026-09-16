package desktop

import (
	"bytes"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testPNG(t *testing.T) []byte {
	t.Helper()
	var data bytes.Buffer
	require.NoError(t, png.Encode(&data, image.NewNRGBA(image.Rect(0, 0, 24, 16))))
	return data.Bytes()
}

func testStore(t *testing.T) (*Store, *time.Time) {
	t.Helper()
	store, err := NewStore(t.TempDir(), "https://shots.example")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	now := time.Now()
	store.now = func() time.Time { return now }
	return store, &now
}

func requestShot(t *testing.T, store *Store, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	u, err := url.Parse(target)
	require.NoError(t, err)
	r := httptest.NewRequest(method, u.RequestURI(), nil)
	r.Host = "attacker.example"
	w := httptest.NewRecorder()
	store.ServeHTTP(w, r)
	return w
}

func TestScreenshotHTTP(t *testing.T) {
	store, now := testStore(t)
	data := testPNG(t)
	shot, err := store.Publish("sandbox", now.Add(time.Hour), bytes.NewReader(data))
	require.NoError(t, err)
	assert.Equal(t, 24, shot.Width)
	assert.Equal(t, 16, shot.Height)
	assert.Regexp(t, `^https://shots\.example/screenshots/[a-f0-9]{32}$`, shot.URL)
	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodPost, http.MethodDelete, http.MethodPut} {
		t.Run(method, func(t *testing.T) {
			w := requestShot(t, store, method, shot.URL)
			assert.Equal(t, "nosniff", w.Header().Get("X-Content-Type-Options"))
			assert.Equal(t, "no-store", w.Header().Get("Cache-Control"))
			if method != http.MethodGet && method != http.MethodHead {
				assert.Equal(t, http.StatusMethodNotAllowed, w.Code)
				assert.Equal(t, "GET, HEAD", w.Header().Get("Allow"))
				return
			}
			assert.Equal(t, http.StatusOK, w.Code)
			assert.Equal(t, "image/png", w.Header().Get("Content-Type"))
			if method == http.MethodHead {
				assert.Empty(t, w.Body.Bytes())
			} else {
				assert.Equal(t, data, w.Body.Bytes())
				_, err := png.Decode(w.Body)
				require.NoError(t, err)
			}
		})
	}
}

func TestScreenshotExpiryAndSandboxPurge(t *testing.T) {
	for _, tc := range []struct {
		name    string
		ttl     time.Duration
		advance time.Duration
	}{
		{"retention", time.Hour, 5 * time.Minute},
		{"sandbox expiry", time.Minute, time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, now := testStore(t)
			shot, err := store.Publish("sandbox", now.Add(tc.ttl), bytes.NewReader(testPNG(t)))
			require.NoError(t, err)
			*now = now.Add(tc.advance)
			store.Sweep()
			assert.Equal(t, http.StatusNotFound, requestShot(t, store, http.MethodGet, shot.URL).Code)
		})
	}
	store, now := testStore(t)
	first, err := store.Publish("one", now.Add(time.Hour), bytes.NewReader(testPNG(t)))
	require.NoError(t, err)
	second, err := store.Publish("two", now.Add(time.Hour), bytes.NewReader(testPNG(t)))
	require.NoError(t, err)
	store.PurgeSandbox("one")
	assert.Equal(t, http.StatusNotFound, requestShot(t, store, http.MethodGet, first.URL).Code)
	assert.Equal(t, http.StatusOK, requestShot(t, store, http.MethodGet, second.URL).Code)
	_, err = store.Publish("expired", *now, bytes.NewReader(testPNG(t)))
	require.Error(t, err)
}

func TestPinnedSandboxScreenshotStillExpiresAtRetention(t *testing.T) {
	store, now := testStore(t)
	shot, err := store.Publish("pinned", time.Time{}, bytes.NewReader(testPNG(t)))
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, requestShot(t, store, http.MethodGet, shot.URL).Code)
	*now = now.Add(5 * time.Minute)
	store.Sweep()
	assert.Equal(t, http.StatusNotFound, requestShot(t, store, http.MethodGet, shot.URL).Code)
}

func TestScreenshotSizeAndCapacityRejectWithoutEviction(t *testing.T) {
	store, now := testStore(t)
	pngData := testPNG(t)
	padded := make([]byte, MaxImageBytes)
	copy(padded, pngData)
	_, err := store.Publish(
		"one",
		now.Add(time.Hour),
		io.MultiReader(bytes.NewReader(padded), bytes.NewReader([]byte{0})),
	)
	require.ErrorContains(t, err, "16 MiB")
	var urls []string
	for range 8 {
		var shot Screenshot
		shot, err = store.Publish("one", now.Add(time.Hour), bytes.NewReader(padded))
		require.NoError(t, err)
		urls = append(urls, shot.URL)
	}
	_, err = store.Publish("two", now.Add(time.Hour), bytes.NewReader(pngData))
	require.ErrorContains(t, err, "128 MiB")
	for _, target := range urls {
		assert.Equal(t, http.StatusOK, requestShot(t, store, http.MethodHead, target).Code)
	}
	*now = now.Add(5 * time.Minute)
	_, err = store.Publish("two", now.Add(time.Hour), bytes.NewReader(pngData))
	require.NoError(t, err, "expired images release capacity before rejecting publication")
	_, err = store.Publish("two", now.Add(time.Hour), bytes.NewReader([]byte("not a PNG")))
	require.Error(t, err)
}

func TestScreenshotStartupClearsOnlyOwnedScratch(t *testing.T) {
	dir := t.TempDir()
	keep := filepath.Join(dir, "unrelated")
	require.NoError(t, os.WriteFile(keep, []byte("keep"), 0o600))
	old := filepath.Join(dir, ".agentcompute-screenshots")
	require.NoError(t, os.Mkdir(old, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(old, "old-image"), []byte("expired"), 0o600))
	store, err := NewStore(dir, "https://shots.example")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	_, err = os.Stat(filepath.Join(old, "old-image"))
	require.ErrorIs(t, err, os.ErrNotExist)
	data, err := os.ReadFile(keep)
	require.NoError(t, err)
	assert.Equal(t, "keep", string(data))
	_, err = NewStore(dir, "https://shots.example")
	require.Error(t, err, "a second process must not erase live screenshots")
}
