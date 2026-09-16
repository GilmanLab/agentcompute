package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GilmanLab/agentcompute/internal/compute"
)

func TestRuntimeConfigurationRejectsInvalidDocuments(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, extension, body string }{
		{"unknown YAML key", yamlExtension, "incus:\n  remote: nas01\n  host: lab01\n  pool: data\n  typo: true\n"},
		{"second YAML document", yamlExtension, "incus:\n  remote: nas01\n  host: lab01\n  pool: data\n---\n{}\n"},
		{"unknown TOML key", tomlExtension, "[incus]\nremote='nas01'\nhost='lab01'\npool='data'\ntypo=true\n"},
		{"ambiguous endpoint", yamlExtension, "incus:\n  remote: nas01\n  url: https://example.invalid\n  host: lab01\n  pool: data\n"},
		{"overflowing TTL", yamlExtension, "incus:\n  remote: nas01\n  host: lab01\n  pool: data\nsandbox:\n  max_ttl_minutes: 9223372036854775807\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "config"+tc.extension)
			if tc.extension == tomlExtension {
				tc.body += "\n[screenshots]\nbase_url='http://127.0.0.1:8081'\n"
			} else {
				tc.body += "\nscreenshots:\n  base_url: http://127.0.0.1:8081\n"
			}
			require.NoError(t, os.WriteFile(path, []byte(tc.body), 0o600))
			_, err := loadRuntimeConfig(path)
			require.Error(t, err)
		})
	}
}

func TestRuntimeConfigurationResolvesPathsRelativeToFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	require.NoError(
		t,
		os.WriteFile(
			path,
			[]byte(
				"images_file='catalog.yaml'\n[incus]\nurl='https://example.invalid'\nclient_cert='client.crt'\nclient_key='client.key'\nhost='lab01'\npool='data'\n[screenshots]\nbase_url='http://127.0.0.1:8081'\n",
			),
			0o600,
		),
	)
	cfg, err := loadRuntimeConfig(path)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(dir, "catalog.yaml"), cfg.ImagesFile)
	assert.Equal(t, filepath.Join(dir, "client.crt"), cfg.Incus.ClientCert)
	assert.Equal(t, filepath.Join(dir, "client.key"), cfg.Incus.ClientKey)
}

func TestRuntimeConfigurationResolvesLumePaths(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
incus:
  url: https://example.invalid
  pool: data
lume:
  host: mac01
  identity_file: id_ed25519
  known_hosts_file: known_hosts
  guest_known_hosts_file: guest_known_hosts
  guest_keys:
    macos/tahoe/desktop: keys/tahoe
screenshots:
  base_url: http://127.0.0.1:8081
`), 0o600))
	cfg, err := loadRuntimeConfig(path)
	require.NoError(t, err)
	assert.Equal(t, "mac01", cfg.Lume.Host)
	assert.Equal(t, filepath.Join(dir, "id_ed25519"), cfg.Lume.IdentityFile)
	assert.Equal(t, filepath.Join(dir, "known_hosts"), cfg.Lume.KnownHostsFile)
	assert.Equal(t, filepath.Join(dir, "guest_known_hosts"), cfg.Lume.GuestKnownHostsFile)
	assert.Equal(t, filepath.Join(dir, "keys/tahoe"), cfg.Lume.GuestKeys["macos/tahoe/desktop"])
}

func TestRuntimeConfigurationAllowsOmittedLume(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
incus:
  url: https://example.invalid
  pool: data
screenshots:
  base_url: http://127.0.0.1:8081
`), 0o600))
	cfg, err := loadRuntimeConfig(path)
	require.NoError(t, err)
	assert.Empty(t, cfg.Lume.Host)
}

func TestIncusCatalogImagesSkipMacSeeds(t *testing.T) {
	t.Parallel()
	images := []compute.CatalogImage{
		{Name: "router", Platform: "incus", Reference: "images:alpine/3.22"},
		{Name: "macos/tahoe/desktop", Platform: "mac", Seed: "ac-seed-macos-tahoe-desktop"},
	}
	got := incusCatalogImages(images)
	require.Len(t, got, 1)
	assert.Equal(t, "router", got[0].Name)
	merged := mergeCatalogImages(images, []compute.CatalogImage{
		{Name: "router", Platform: "incus", Reference: "images:alpine/3.22", Fingerprint: "abc"},
	})
	require.Len(t, merged, 2)
	assert.Equal(t, "abc", merged[0].Fingerprint)
	assert.Equal(t, "ac-seed-macos-tahoe-desktop", merged[1].Seed)
}
