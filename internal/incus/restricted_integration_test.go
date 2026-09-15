//go:build integration

package incus

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"testing"
	"time"

	incusclient "github.com/lxc/incus/v7/client"
	"github.com/lxc/incus/v7/shared/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRestrictedIdentity uses an existing restricted certificate against a live
// server. It never changes trust entries or project restrictions.
func TestRestrictedIdentity(t *testing.T) {
	url := os.Getenv("AGENTCOMPUTE_TEST_RESTRICTED_URL")
	if url == "" {
		t.Skip("set AGENTCOMPUTE_TEST_RESTRICTED_URL and restricted certificate paths for live acceptance")
	}
	credential := func(name string) string {
		t.Helper()
		path := os.Getenv("AGENTCOMPUTE_TEST_RESTRICTED_" + name)
		require.NotEmpty(t, path, "missing restricted identity input %s", name)
		content, err := os.ReadFile(path)
		require.NoError(t, err)
		return string(content)
	}
	project := os.Getenv("AGENTCOMPUTE_TEST_RESTRICTED_PROJECT")
	require.NotEmpty(t, project, "set an existing project allowed by the restricted certificate")
	require.NotEqual(t, api.ProjectDefaultName, project)
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	server, err := incusclient.ConnectIncusWithContext(ctx, url, &incusclient.ConnectionArgs{
		TLSClientCert: credential("CLIENT_CERT"),
		TLSClientKey:  credential("CLIENT_KEY"),
		TLSServerCert: credential("SERVER_CERT"),
		SkipGetEvents: true,
	})
	require.NoError(t, err)
	defer server.Disconnect()
	info, _, err := server.GetServer()
	require.NoError(t, err)
	require.Equal(t, "trusted", info.Auth, "an untrusted connection cannot qualify a restricted identity")
	projects, err := server.GetProjectNames()
	require.NoError(t, err)
	require.Contains(t, projects, project, "prove positive access to the real allowed project")
	require.NotContains(
		t,
		projects,
		api.ProjectDefaultName,
		"refuse an unrestricted or default-project identity before mutation probes",
	)
	allowed, etag, err := server.GetProject(project)
	require.NoError(t, err)

	t.Run("default instances are filtered", func(t *testing.T) {
		instances, err := server.UseProject(api.ProjectDefaultName).GetInstances(api.InstanceTypeAny)
		require.NoError(t, err)
		assert.Empty(t, instances)
	})
	t.Run("project configuration is not writable", func(t *testing.T) {
		err := server.UpdateProject(project, allowed.Writable(), etag)
		require.Error(t, err)
		assert.True(t, api.StatusErrorCheck(err, http.StatusForbidden), "expected permission denial, got %v", err)
	})
	t.Run("ac prefix does not grant project creation", func(t *testing.T) {
		name := fmt.Sprintf("ac-identity-%d", time.Now().UnixNano())
		err := server.CreateProject(api.ProjectsPost{Name: name})
		if err == nil {
			t.Cleanup(func() {
				_ = server.UseProject(name).DeleteProfile("default")
				require.NoError(t, server.DeleteProject(name))
			})
		}
		require.Error(t, err)
		assert.True(t, api.StatusErrorCheck(err, http.StatusForbidden), "expected permission denial, got %v", err)
	})
}
