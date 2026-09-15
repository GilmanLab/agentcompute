package compute

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

const (
	catalogSchemaVersion = 1
	platformIncus        = "incus"
	sha256Prefix         = "@sha256:"
	sha256HexLength      = 64
)

// Catalog is an immutable curated image list loaded from Phase 1 YAML.
type Catalog struct {
	images []CatalogImage
	byName map[string]CatalogImage
}

type catalogFile struct {
	SchemaVersion int                `yaml:"schema_version"`
	Images        []catalogFileImage `yaml:"images"`
}

type catalogFileImage struct {
	Name        string   `yaml:"name"`
	OS          string   `yaml:"os"`
	Version     string   `yaml:"version"`
	Platform    string   `yaml:"platform"`
	Kind        string   `yaml:"kind"`
	Kinds       []string `yaml:"kinds"`
	Desktop     bool     `yaml:"desktop"`
	Description string   `yaml:"description"`
	Reference   string   `yaml:"reference"`
	Alias       string   `yaml:"alias"`
	CPUs        int64    `yaml:"cpus"`
	MemoryMB    int64    `yaml:"memory_mb"`
	DiskGB      int64    `yaml:"disk_gb"`
}

// LoadCatalog reads a strict schema_version=1 catalog YAML file.
func LoadCatalog(path string) (*Catalog, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open catalog: %w", err)
	}
	defer file.Close()

	decoder := yaml.NewDecoder(file)
	decoder.KnownFields(true)

	var parsed catalogFile
	if err := decoder.Decode(&parsed); err != nil {
		return nil, fmt.Errorf("parse catalog: %w", err)
	}
	if parsed.SchemaVersion != catalogSchemaVersion {
		return nil, fmt.Errorf("catalog schema_version must be %d", catalogSchemaVersion)
	}
	if parsed.Images == nil {
		return nil, errors.New("catalog images must be a list")
	}

	images := make([]CatalogImage, 0, len(parsed.Images))
	for i, entry := range parsed.Images {
		image, err := entry.toCatalogImage()
		if err != nil {
			return nil, fmt.Errorf("catalog images[%d]: %w", i, err)
		}
		images = append(images, image)
	}
	return NewCatalog(images)
}

// NewCatalog validates and freezes a catalog from already parsed entries.
func NewCatalog(images []CatalogImage) (*Catalog, error) {
	cloned := make([]CatalogImage, 0, len(images))
	byName := make(map[string]CatalogImage, len(images))
	for i, image := range images {
		normalized, err := normalizeCatalogImage(image)
		if err != nil {
			return nil, fmt.Errorf("catalog images[%d]: %w", i, err)
		}
		if _, exists := byName[normalized.Name]; exists {
			return nil, fmt.Errorf("duplicate catalog image %q", normalized.Name)
		}
		cloned = append(cloned, normalized)
		byName[normalized.Name] = normalized
	}
	return &Catalog{images: cloned, byName: byName}, nil
}

// Images returns a copy of every catalog entry.
func (c *Catalog) Images() []CatalogImage {
	if c == nil || len(c.images) == 0 {
		return []CatalogImage{}
	}
	out := make([]CatalogImage, len(c.images))
	for i, image := range c.images {
		out[i] = cloneCatalogImage(image)
	}
	return out
}

// Lookup returns a copy of the named catalog entry.
func (c *Catalog) Lookup(name string) (CatalogImage, bool) {
	if c == nil {
		return CatalogImage{}, false
	}
	image, ok := c.byName[name]
	if !ok {
		return CatalogImage{}, false
	}
	return cloneCatalogImage(image), true
}

func (entry catalogFileImage) toCatalogImage() (CatalogImage, error) {
	return normalizeCatalogImage(CatalogImage{
		Name:        entry.Name,
		OS:          entry.OS,
		Version:     entry.Version,
		Platform:    entry.Platform,
		Kind:        entry.Kind,
		Kinds:       entry.Kinds,
		Desktop:     entry.Desktop,
		Description: entry.Description,
		Reference:   entry.Reference,
		Alias:       entry.Alias,
		CPUs:        entry.CPUs,
		MemoryMB:    entry.MemoryMB,
		DiskGB:      entry.DiskGB,
	})
}

func normalizeCatalogImage(image CatalogImage) (CatalogImage, error) {
	if image.Name == "" {
		return CatalogImage{}, errors.New("name is required")
	}
	if image.OS == "" {
		return CatalogImage{}, errors.New("os is required")
	}
	if image.Version == "" {
		return CatalogImage{}, errors.New("version is required")
	}
	if image.Kind == "" {
		return CatalogImage{}, errors.New("kind is required")
	}
	if len(image.Kinds) == 0 {
		return CatalogImage{}, errors.New("kinds is required")
	}
	if !slices.Contains(image.Kinds, image.Kind) {
		return CatalogImage{}, fmt.Errorf("kind %q is not listed in kinds", image.Kind)
	}
	if (image.Reference == "") == (image.Alias == "") {
		return CatalogImage{}, errors.New("exactly one of reference or alias is required")
	}
	if image.Alias != "" && (strings.TrimSpace(image.Alias) != image.Alias ||
		strings.ContainsAny(image.Alias, ":\x00\r\n")) {
		return CatalogImage{}, errors.New("alias must be a cluster-local image alias")
	}
	if strings.HasPrefix(strings.ToLower(image.OS), "windows") && image.Reference != "" {
		return CatalogImage{}, errors.New("windows images must use a cluster-local alias")
	}
	if image.Reference != "" && !validReference(image.Reference) {
		return CatalogImage{}, fmt.Errorf("reference %q is neither a digest nor a remote alias", image.Reference)
	}
	if image.CPUs <= 0 {
		return CatalogImage{}, errors.New("cpus must be positive")
	}
	if image.MemoryMB <= 0 {
		return CatalogImage{}, errors.New("memory_mb must be positive")
	}
	if image.DiskGB <= 0 {
		return CatalogImage{}, errors.New("disk_gb must be positive")
	}
	if image.Platform == "" {
		image.Platform = platformIncus
	}
	image.Kinds = slices.Clone(image.Kinds)
	return image, nil
}

func validReference(reference string) bool {
	if index := strings.LastIndex(reference, sha256Prefix); index >= 0 {
		digest := reference[index+len(sha256Prefix):]
		if index == 0 || len(digest) != sha256HexLength {
			return false
		}
		for i := range digest {
			c := digest[i]
			switch {
			case c >= '0' && c <= '9', c >= 'a' && c <= 'f', c >= 'A' && c <= 'F':
			default:
				return false
			}
		}
		return true
	}
	remote, alias, ok := strings.Cut(reference, ":")
	return ok && remote != "" && alias != ""
}

func cloneCatalogImage(image CatalogImage) CatalogImage {
	image.Kinds = slices.Clone(image.Kinds)
	return image
}
