package incus

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	incusclient "github.com/lxc/incus/v7/client"
	"github.com/lxc/incus/v7/shared/api"
	"github.com/stretchr/testify/require"
)

func connectFixture(t *testing.T, rawURL string) incusclient.InstanceServer {
	t.Helper()
	server, err := incusclient.ConnectIncusWithContext(context.Background(), rawURL, &incusclient.ConnectionArgs{
		InsecureSkipVerify: true,
		SkipGetEvents:      true,
	})
	require.NoError(t, err)
	return server
}

func writeIncusSync(w http.ResponseWriter, metadata any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(api.ResponseRaw{
		Type:       api.SyncResponse,
		Status:     "Success",
		StatusCode: http.StatusOK,
		Metadata:   metadata,
	})
}

func writeIncusError(w http.ResponseWriter, code int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(api.ResponseRaw{
		Type:  api.ErrorResponse,
		Code:  code,
		Error: message,
	})
}
