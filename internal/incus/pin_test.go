package incus

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/lxc/incus/v7/shared/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSandboxPinSurvivesClientRestart(t *testing.T) {
	t.Parallel()
	expires := time.Now().Add(-time.Minute).UTC()
	since := expires.Add(-time.Hour)
	var mu sync.Mutex
	project := api.Project{Name: "ac-keep", ProjectPut: api.ProjectPut{Config: map[string]string{
		metaVersion:      versionValue,
		metaExpiresAt:    expires.Format(time.RFC3339Nano),
		metaSubject:      "creator",
		"user.unrelated": "retained",
	}}}
	fixture := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/1.0":
			writeIncusSync(
				w,
				api.Server{ServerUntrusted: api.ServerUntrusted{Auth: "trusted", APIExtensions: []string{"projects"}}},
			)
		case "/1.0/projects/ac-keep":
			if r.Method == http.MethodPut {
				var put api.ProjectPut
				if err := json.NewDecoder(r.Body).Decode(&put); err != nil {
					writeIncusError(w, http.StatusBadRequest, err.Error())
					return
				}
				project.ProjectPut = put
			}
			writeIncusSync(w, project)
		case "/1.0/projects":
			writeIncusSync(w, []api.Project{project})
		default:
			writeIncusError(w, http.StatusNotFound, "not found")
		}
	}))
	t.Cleanup(fixture.Close)
	sdk := connectFixture(t, fixture.URL)
	client := &Client{server: sdk}
	legacy, err := client.GetSandbox(t.Context(), "keep")
	require.NoError(t, err)
	assert.False(t, legacy.Pinned)
	_, err = client.PinSandbox(t.Context(), "keep", true, "omp", since)
	require.NoError(t, err)
	sdk.Disconnect()
	restartedSDK := connectFixture(t, fixture.URL)
	t.Cleanup(restartedSDK.Disconnect)
	restarted := &Client{server: restartedSDK}
	boxes, err := restarted.ListSandboxes(t.Context())
	require.NoError(t, err)
	require.Len(t, boxes, 1)
	assert.True(t, boxes[0].Pinned)
	assert.Equal(t, "omp", boxes[0].PinnedBy)
	assert.True(t, since.Equal(boxes[0].PinnedAt))
	assert.True(t, expires.Equal(boxes[0].ExpiresAt))
	unpinned, err := restarted.PinSandbox(t.Context(), "keep", false, "", time.Time{})
	require.NoError(t, err)
	assert.False(t, unpinned.Pinned)
	assert.Empty(t, unpinned.PinnedBy)
	assert.True(t, unpinned.PinnedAt.IsZero())
	assert.True(t, expires.Equal(unpinned.ExpiresAt))
	assert.Equal(t, "creator", unpinned.Subject)
	mu.Lock()
	assert.Equal(t, "retained", project.Config["user.unrelated"])
	mu.Unlock()
}
