package compute_test

import (
	"context"
	"io"
	"testing"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/GilmanLab/agentcompute/internal/compute"
	"github.com/GilmanLab/agentcompute/internal/compute/mocks"
)

func TestImpairRejectsUnknownNIC(t *testing.T) {
	t.Parallel()

	tc := newTestContext(t)
	inst := runningLinuxGuest()
	tc.backend.EXPECT().GetInstance(mock.Anything, inst.Ref).Return(inst, nil)

	err := tc.service.ImpairNIC(t.Context(), inst.Ref, "eth1", compute.Impairment{LatencyMS: 100})
	requireAgentContains(t, err, `"eth1"`)
}

func TestImpairRejectsNonLinuxCatalogGuest(t *testing.T) {
	t.Parallel()

	tc := newWindowsImpairContext(t)
	inst := compute.Instance{
		Ref:    compute.Ref{Sandbox: "demo", Name: "win"},
		Image:  "windows/11/desktop",
		Kind:   "vm",
		Status: "Running",
		NICs:   []compute.NIC{{Name: "eth0", Network: "default"}},
	}
	tc.backend.EXPECT().GetInstance(mock.Anything, inst.Ref).Return(inst, nil)

	err := tc.service.ImpairNIC(t.Context(), inst.Ref, "eth0", compute.Impairment{LatencyMS: 100})
	requireAgentContains(t, err, "windows")
}

func TestImpairRejectsNonLinuxUname(t *testing.T) {
	t.Parallel()

	tc := newTestContext(t)
	inst := runningLinuxGuest()
	inst.Image = "published"
	tc.backend.EXPECT().GetInstance(mock.Anything, inst.Ref).Return(inst, nil)
	tc.backend.EXPECT().Exec(mock.Anything, unameRequest(), mock.Anything, mock.Anything).
		RunAndReturn(func(_ context.Context, _ compute.ExecRequest, stdout, _ io.Writer) (int64, error) {
			_, _ = stdout.Write([]byte("Darwin\n"))
			return 0, nil
		})

	err := tc.service.ImpairNIC(t.Context(), inst.Ref, "eth0", compute.Impairment{LatencyMS: 100})
	requireAgentContains(t, err, "non-Linux")
}


func TestImpairSurfacesGuestStderr(t *testing.T) {
	t.Parallel()

	tc := newTestContext(t)
	inst := runningLinuxGuest()
	tc.backend.EXPECT().GetInstance(mock.Anything, inst.Ref).Return(inst, nil)
	tc.backend.EXPECT().Exec(mock.Anything, unameRequest(), mock.Anything, mock.Anything).
		RunAndReturn(writeUnameLinux)
	tc.backend.EXPECT().Exec(mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		RunAndReturn(func(_ context.Context, _ compute.ExecRequest, _, stderr io.Writer) (int64, error) {
			_, _ = stderr.Write([]byte("RTNETLINK answers: No such file or directory\n"))
			return 2, nil
		})

	err := tc.service.ImpairNIC(t.Context(), inst.Ref, "eth0", compute.Impairment{RateMbit: 10})
	requireAgentContains(t, err, "RTNETLINK answers: No such file or directory")
}

func runningLinuxGuest() compute.Instance {
	inst := runningInstance()
	inst.NICs = []compute.NIC{{Name: "eth0", Network: "default"}}
	return inst
}

func newWindowsImpairContext(t *testing.T) *testContext {
	t.Helper()

	backend := mocks.NewMockBackend(t)
	catalog, err := compute.NewCatalog([]compute.CatalogImage{{
		Name:      "windows/11/desktop",
		OS:        "windows",
		Version:   "11",
		Kind:      "vm",
		Kinds:     []string{"vm"},
		Alias:     "windows/11/desktop",
		CPUs:      2,
		MemoryMB:  4096,
		DiskGB:    40,
	}})
	require.NoError(t, err)
	service, err := compute.New(backend, catalog, compute.Options{Host: "lab01"})
	require.NoError(t, err)
	return &testContext{backend: backend, service: service}
}

func unameRequest() any {
	return mock.MatchedBy(func(req compute.ExecRequest) bool {
		return len(req.Argv) == 2 && req.Argv[0] == "uname" && req.Argv[1] == "-s"
	})
}

func writeUnameLinux(_ context.Context, _ compute.ExecRequest, stdout, _ io.Writer) (int64, error) {
	_, _ = stdout.Write([]byte("Linux\n"))
	return 0, nil
}
