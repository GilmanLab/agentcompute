package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
