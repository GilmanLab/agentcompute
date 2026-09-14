package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/meigma/codemode"
	"github.com/meigma/codemode/authz"
	hostmcp "github.com/meigma/codemode/mcpserver"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/pelletier/go-toml/v2"
	"go.yaml.in/yaml/v3"

	"github.com/GilmanLab/agentcompute/internal/compute"
	"github.com/GilmanLab/agentcompute/internal/incus"
	"github.com/GilmanLab/agentcompute/internal/mcpserver"
)

const (
	configFlag        = "config"
	defaultTTLMinutes = 240
	maxTTLMinutes     = 1440
	yamlExtension     = ".yaml"
)

// runtime owns backend connections and the reaper, not individual MCP sessions.
type runtime struct {
	deps  mcpserver.Dependencies
	close func() error
}

type runtimeConfig struct {
	Incus       incusConfig      `yaml:"incus"       toml:"incus"`
	Sandbox     sandboxConfig    `yaml:"sandbox"     toml:"sandbox"`
	Screenshots screenshotConfig `yaml:"screenshots" toml:"screenshots"`
	ImagesFile  string           `yaml:"images_file" toml:"images_file"`
}

type incusConfig struct {
	Remote     string `yaml:"remote"      toml:"remote"`
	URL        string `yaml:"url"         toml:"url"`
	ClientCert string `yaml:"client_cert" toml:"client_cert"`
	ClientKey  string `yaml:"client_key"  toml:"client_key"`
	ServerCert string `yaml:"server_cert" toml:"server_cert"`
	Host       string `yaml:"host"        toml:"host"`
	Pool       string `yaml:"pool"        toml:"pool"`
	OVNUplink  string `yaml:"ovn_uplink"  toml:"ovn_uplink"`
	OVNRanges  string `yaml:"ovn_ranges"  toml:"ovn_ranges"`
}

type sandboxConfig struct {
	DefaultTTLMinutes  int64  `yaml:"default_ttl_minutes"  toml:"default_ttl_minutes"`
	MaxTTLMinutes      int64  `yaml:"max_ttl_minutes"      toml:"max_ttl_minutes"`
	DefaultNetworkKind string `yaml:"default_network_kind" toml:"default_network_kind"`
}

type screenshotConfig struct {
	Dir     string `yaml:"dir"      toml:"dir"`
	BaseURL string `yaml:"base_url" toml:"base_url"`
}

func loadRuntimeConfig(path string) (runtimeConfig, error) {
	cfg := runtimeConfig{
		Sandbox: sandboxConfig{
			DefaultTTLMinutes:  defaultTTLMinutes,
			MaxTTLMinutes:      maxTTLMinutes,
			DefaultNetworkKind: "ovn",
		},
		ImagesFile: "images/catalog.yaml",
	}
	if path == "" {
		return cfg, errors.New("configuration is required: use --config or AGENTCOMPUTE_CONFIG")
	}
	if err := decodeRuntimeConfig(path, &cfg); err != nil {
		return cfg, err
	}
	if err := validateRuntimeConfig(cfg); err != nil {
		return cfg, err
	}
	resolveRuntimePaths(path, &cfg)
	return cfg, nil
}

func decodeRuntimeConfig(path string, cfg *runtimeConfig) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open configuration: %w", err)
	}
	defer file.Close()
	switch strings.ToLower(filepath.Ext(path)) {
	case ".toml":
		err = toml.NewDecoder(file).DisallowUnknownFields().Decode(cfg)
	case yamlExtension, ".yml":
		err = decodeYAMLConfig(file, cfg)
	default:
		err = errors.New("configuration must be a .yaml, .yml, or .toml file")
	}
	if err != nil {
		return fmt.Errorf("decode configuration: %w", err)
	}
	return nil
}

func decodeYAMLConfig(r io.Reader, cfg *runtimeConfig) error {
	decoder := yaml.NewDecoder(r)
	decoder.KnownFields(true)
	if err := decoder.Decode(cfg); err != nil {
		return err
	}
	var extra any
	if next := decoder.Decode(&extra); !errors.Is(next, io.EOF) {
		return errors.New("configuration must contain exactly one YAML document")
	}
	return nil
}

func validateRuntimeConfig(cfg runtimeConfig) error {
	if cfg.Incus.Pool == "" {
		return errors.New("incus.pool is required")
	}
	if (cfg.Incus.Remote == "") == (cfg.Incus.URL == "") {
		return errors.New("configure exactly one of incus.remote and incus.url")
	}
	if cfg.Sandbox.DefaultNetworkKind != "bridge" && cfg.Sandbox.DefaultNetworkKind != "ovn" {
		return errors.New("sandbox.default_network_kind must be bridge or ovn")
	}
	if cfg.Sandbox.DefaultNetworkKind == "bridge" && cfg.Incus.Host == "" {
		return errors.New("incus.host is required for bridge sandboxes")
	}
	const maxDurationMinutes = int64((1<<63 - 1) / time.Minute)
	if cfg.Sandbox.DefaultTTLMinutes <= 0 || cfg.Sandbox.MaxTTLMinutes < cfg.Sandbox.DefaultTTLMinutes ||
		cfg.Sandbox.MaxTTLMinutes > maxDurationMinutes {
		return errors.New("sandbox TTL minutes must be positive, representable durations with default <= maximum")
	}
	if cfg.ImagesFile == "" {
		return errors.New("images_file must not be empty")
	}
	return nil
}

func resolveRuntimePaths(path string, cfg *runtimeConfig) {
	base := filepath.Dir(path)
	for _, value := range []*string{
		&cfg.ImagesFile, &cfg.Incus.ClientCert, &cfg.Incus.ClientKey, &cfg.Incus.ServerCert, &cfg.Screenshots.Dir,
	} {
		if *value != "" && !filepath.IsAbs(*value) {
			*value = filepath.Join(base, *value)
		}
	}
}

func newRuntime(ctx context.Context, path string, logger *slog.Logger) (*runtime, error) {
	cfg, err := loadRuntimeConfig(path)
	if err != nil {
		return nil, err
	}
	catalog, err := compute.LoadCatalog(cfg.ImagesFile)
	if err != nil {
		return nil, err
	}
	client, err := incus.New(ctx, incus.Options{
		Remote: cfg.Incus.Remote, URL: cfg.Incus.URL,
		ClientCert: cfg.Incus.ClientCert, ClientKey: cfg.Incus.ClientKey, ServerCert: cfg.Incus.ServerCert,
		Host: cfg.Incus.Host, Pool: cfg.Incus.Pool,
		OVNUplink: cfg.Incus.OVNUplink, OVNRanges: cfg.Incus.OVNRanges,
	})
	if err != nil {
		return nil, err
	}
	entries, err := client.EnsureCatalog(ctx, catalog.Images())
	if err != nil {
		return nil, errors.Join(err, client.Close())
	}
	catalog, err = compute.NewCatalog(entries)
	if err != nil {
		return nil, errors.Join(err, client.Close())
	}
	service, err := compute.New(client, catalog, compute.Options{
		Host:               cfg.Incus.Host,
		DefaultNetworkKind: cfg.Sandbox.DefaultNetworkKind,
		DefaultTTL:         time.Duration(cfg.Sandbox.DefaultTTLMinutes) * time.Minute,
		MaxTTL:             time.Duration(cfg.Sandbox.MaxTTLMinutes) * time.Minute,
		Logger:             logger,
	})
	if err != nil {
		return nil, errors.Join(err, client.Close())
	}
	lifecycle, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		if reapErr := service.RunReaper(lifecycle); reapErr != nil && lifecycle.Err() == nil {
			logger.ErrorContext(lifecycle, "reaper stopped", "err", reapErr)
		}
	}()
	return &runtime{deps: mcpserver.NewDependencies(service), close: func() error {
		cancel()
		<-done
		return client.Close()
	}}, nil
}

func (o Options) openRuntime(ctx context.Context, logger *slog.Logger) (*runtime, error) {
	if o.Dependencies != nil {
		return &runtime{deps: *o.Dependencies, close: func() error { return nil }}, nil
	}
	return newRuntime(ctx, o.Viper.GetString(configFlag), logger)
}

func newComputeServer(
	logger *slog.Logger,
	version string,
	resolver hostmcp.InvocationResolver,
	deps mcpserver.Dependencies,
) (*mcp.Server, error) {
	return mcpserver.New(mcpserver.Options{
		Version: version, Logger: logger, Resolver: resolver, Deps: deps,
		Runtime: codemode.Options{Authorizer: authz.AllowAll()},
	})
}
