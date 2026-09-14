package compute

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadCatalogDigestAndAlias(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "catalog.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
schema_version: 1
images:
  - name: router
    os: alpinelinux
    version: "3.22.5"
    kinds: [container]
    kind: container
    reference: ghcr.io/gilmanlab/agentcompute/router@sha256:6b3ecd8336b6fce7006e764e01373199e2cc1cf46172132b53e0aaebd27be889
    cpus: 1
    memory_mb: 512
    disk_gb: 2
  - name: ubuntu
    os: ubuntu
    version: "24.04"
    kinds: [container, vm]
    kind: container
    platform: incus
    desktop: true
    description: upstream
    reference: images:ubuntu/24.04
    cpus: 2
    memory_mb: 1024
    disk_gb: 8
`), 0o600))

	catalog, err := LoadCatalog(path)
	require.NoError(t, err)

	images := catalog.Images()
	require.Len(t, images, 2)
	assert.Equal(t, "incus", images[0].Platform)
	assert.False(t, images[0].Desktop)
	assert.Equal(t, "ubuntu", images[1].Name)
	assert.True(t, images[1].Desktop)
	assert.Equal(t, "upstream", images[1].Description)

	router, ok := catalog.Lookup("router")
	require.True(t, ok)
	assert.Equal(t, "router", router.Name)
	router.Name = "mutated"
	original, ok := catalog.Lookup("router")
	require.True(t, ok)
	assert.Equal(t, "router", original.Name)
}

func TestLoadCatalogRejectsUnknownKeysAndBadSchema(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{
			name:    "unknown key",
			yaml:    "schema_version: 1\nimages: []\nextra: true\n",
			wantErr: "parse catalog:",
		},
		{
			name:    "wrong schema",
			yaml:    "schema_version: 2\nimages: []\n",
			wantErr: "catalog schema_version must be 1",
		},
		{
			name: "bad digest",
			yaml: `schema_version: 1
images:
  - name: router
    os: alpine
    version: "1"
    kinds: [container]
    kind: container
    reference: ghcr.io/x@sha256:dead
    cpus: 1
    memory_mb: 1
    disk_gb: 1
`,
			wantErr: "neither a digest nor a remote alias",
		},
		{
			name: "kind not in kinds",
			yaml: `schema_version: 1
images:
  - name: router
    os: alpine
    version: "1"
    kinds: [container]
    kind: vm
    reference: images:ubuntu/24.04
    cpus: 1
    memory_mb: 1
    disk_gb: 1
`,
			wantErr: `kind "vm" is not listed in kinds`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "catalog.yaml")
			require.NoError(t, os.WriteFile(path, []byte(tt.yaml), 0o600))
			_, err := LoadCatalog(path)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestNewCatalogDuplicateNames(t *testing.T) {
	t.Parallel()

	image := CatalogImage{
		Name: "router", OS: "alpine", Version: "1", Kind: "container",
		Kinds: []string{"container"}, Reference: "images:ubuntu/24.04",
		CPUs: 1, MemoryMB: 1, DiskGB: 1,
	}
	_, err := NewCatalog([]CatalogImage{image, image})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `duplicate catalog image "router"`)
}

func TestCatalogImagesEmptyCopy(t *testing.T) {
	t.Parallel()

	catalog, err := NewCatalog(nil)
	require.NoError(t, err)
	assert.Empty(t, catalog.Images())
	_, ok := catalog.Lookup("missing")
	assert.False(t, ok)
}

func TestWindowsCatalogRequiresLocalAlias(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, alias, reference string
		valid                  bool
	}{
		{name: "local candidate", alias: "windows/11/desktop-26100", valid: true},
		{name: "missing source"},
		{name: "ambiguous source", alias: "windows/11/desktop-26100", reference: "images:windows"},
		{name: "remote alias", alias: "images:windows"},
		{name: "remote reference", reference: "images:windows"},
		{name: "registry", reference: "ghcr.io/example/windows@sha256:6b3ecd8336b6fce7006e764e01373199e2cc1cf46172132b53e0aaebd27be889"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			image := CatalogImage{
				Name: "windows/11/desktop", OS: "Windows", Version: "11",
				Kind: "vm", Kinds: []string{"vm"}, Desktop: true,
				Alias: tt.alias, Reference: tt.reference,
				CPUs: 2, MemoryMB: 4096, DiskGB: 64,
			}
			catalog, err := NewCatalog([]CatalogImage{image})
			if !tt.valid {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			got, ok := catalog.Lookup(image.Name)
			require.True(t, ok)
			assert.Equal(t, tt.alias, got.Alias)
			assert.Empty(t, got.Reference)
		})
	}
}
