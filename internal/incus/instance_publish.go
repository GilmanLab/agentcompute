package incus

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	incusclient "github.com/lxc/incus/v7/client"
	"github.com/lxc/incus/v7/shared/api"
	"github.com/meigma/codemode"

	"github.com/GilmanLab/agentcompute/internal/compute"
)

const (
	metaPublished            = metaPrefix + "published"
	metaCPUs                 = metaPrefix + "cpus"
	metaMemoryMB             = metaPrefix + "memory_mb"
	metaDiskGB               = metaPrefix + "disk_gb"
	metaKind                 = metaPrefix + "kind"
	sourceInstance           = "instance"
	defaultPublishedCPUs     = 1
	defaultPublishedMemoryMB = 512
	defaultPublishedDiskGB   = 2
)

// PublishInstance creates a project-local image alias from a guest.
//
// The image stays in the sandbox project. Catalog copies already in the
// project are left untouched and nothing is published to a remote or the
// default project.
func (c *Client) PublishInstance(ctx context.Context, ref compute.Ref, image string) (string, error) {
	if err := c.requireInstance(ctx, ref); err != nil {
		return "", err
	}
	inst, err := c.GetInstance(ctx, ref)
	if err != nil {
		return "", err
	}
	project := projectName(ref.Sandbox)
	srv := c.Scoped(ctx, project, "")
	source, _, err := srv.GetInstance(ref.Name)
	if err != nil {
		return "", mapError(err)
	}
	if source.StatusCode != api.Stopped {
		return "", publishErrorf("instance %q must be stopped before publishing", ref.Name)
	}
	if err = requireMissingImageAlias(srv, ref.Sandbox, image); err != nil {
		return "", err
	}
	op, err := srv.CreateImage(api.ImagesPost{
		ImagePut: api.ImagePut{
			Public:     false,
			Profiles:   []string{},
			Properties: publishedImageProperties(source.Config, inst, ref.Sandbox, image),
		},
		Aliases: []api.ImageAlias{{Name: image}},
		Source: &api.ImagesPostSource{
			Type:    sourceInstance,
			Name:    ref.Name,
			Project: project,
		},
	}, nil)
	if err != nil {
		if isConflict(err) {
			return "", publishErrorf("image %q already exists in sandbox %q", image, ref.Sandbox)
		}
		return "", mapError(err)
	}
	if err = waitOp(ctx, op); err != nil {
		return "", err
	}
	fingerprint := imageFingerprint(op.Get().Metadata)
	if fingerprint == "" {
		return "", fmt.Errorf("publish instance %q returned no image fingerprint", ref.Name)
	}
	return image, ensureImageAlias(srv, image, fingerprint)
}

// GetSandboxImage returns a published sandbox-local image as a catalog entry.
func (c *Client) GetSandboxImage(ctx context.Context, sandbox, name string) (compute.CatalogImage, error) {
	if _, _, err := c.getProject(ctx, sandbox); err != nil {
		return compute.CatalogImage{}, err
	}
	srv := c.Scoped(ctx, projectName(sandbox), "")
	alias, _, err := srv.GetImageAlias(name)
	if err != nil {
		return compute.CatalogImage{}, mapError(err)
	}
	image, _, err := srv.GetImage(alias.Target)
	if err != nil {
		return compute.CatalogImage{}, mapError(err)
	}
	if image.Properties[metaPublished] != configTrue || image.Properties[metaVersion] != versionValue {
		return compute.CatalogImage{}, compute.ErrNotFound
	}
	kind := image.Properties[metaKind]
	if kind == "" {
		kind = kindContainer
		if image.Type == string(api.InstanceTypeVM) {
			kind = kindVM
		}
	}
	cpus := parseInt64(image.Properties[metaCPUs])
	if cpus <= 0 {
		cpus = defaultPublishedCPUs
	}
	memory := parseInt64(image.Properties[metaMemoryMB])
	if memory <= 0 {
		memory = defaultPublishedMemoryMB
	}
	disk := parseInt64(image.Properties[metaDiskGB])
	if disk <= 0 {
		disk = defaultPublishedDiskGB
	}
	return compute.CatalogImage{
		Name:        name,
		OS:          publishedOS(image.Properties),
		Version:     publishedVersion(image.Properties),
		Platform:    platformIncus,
		Kind:        kind,
		Kinds:       []string{kind},
		Desktop:     isTrue(image.Properties[metaDesktop]),
		Fingerprint: image.Fingerprint,
		CPUs:        cpus,
		MemoryMB:    memory,
		DiskGB:      disk,
	}, nil
}

func imageFingerprint(metadata map[string]any) string {
	if metadata == nil {
		return ""
	}
	raw, ok := metadata["fingerprint"].(string)
	if !ok {
		return ""
	}
	return raw
}

func publishedOS(properties map[string]string) string {
	if osName := properties["os"]; osName != "" {
		return osName
	}
	return "unknown"
}

func publishedVersion(properties map[string]string) string {
	if version := properties["release"]; version != "" {
		return version
	}
	if version := properties["version"]; version != "" {
		return version
	}
	return "published"
}

func publishErrorf(format string, args ...any) error {
	return &codemode.AgentError{Message: fmt.Sprintf(format, args...)}
}

func requireMissingImageAlias(srv incusclient.InstanceServer, sandbox, image string) error {
	_, _, err := srv.GetImageAlias(image)
	if err == nil {
		return publishErrorf("image %q already exists in sandbox %q", image, sandbox)
	}
	if mapped := mapError(err); !errors.Is(mapped, compute.ErrNotFound) {
		return mapped
	}
	return nil
}

func ensureImageAlias(srv incusclient.InstanceServer, image, fingerprint string) error {
	alias, _, err := srv.GetImageAlias(image)
	if err == nil {
		if alias.Target != fingerprint {
			return publishErrorf("image %q already refers to another fingerprint", image)
		}
		return nil
	}
	if mapped := mapError(err); !errors.Is(mapped, compute.ErrNotFound) {
		return mapped
	}
	if err = srv.CreateImageAlias(api.ImageAliasesPost{
		ImageAliasesEntry: api.ImageAliasesEntry{
			Name:                 image,
			ImageAliasesEntryPut: api.ImageAliasesEntryPut{Target: fingerprint},
		},
	}); err != nil {
		return mapError(err)
	}
	return nil
}

func publishedImageProperties(
	config map[string]string,
	inst compute.Instance,
	sandbox, image string,
) map[string]string {
	properties := make(map[string]string)
	for key, value := range config {
		if suffix, ok := strings.CutPrefix(key, "image."); ok {
			properties[suffix] = value
		}
	}
	cpus := inst.CPUs
	if cpus <= 0 {
		cpus = defaultPublishedCPUs
	}
	memory := inst.MemoryMB
	if memory <= 0 {
		memory = defaultPublishedMemoryMB
	}
	disk := inst.DiskGB
	if disk <= 0 {
		disk = defaultPublishedDiskGB
	}
	properties[metaVersion] = versionValue
	properties[metaSandbox] = sandbox
	properties[metaImage] = image
	properties[metaPublished] = configTrue
	properties[metaDesktop] = strconv.FormatBool(inst.Desktop)
	properties[metaKind] = inst.Kind
	properties[metaCPUs] = strconv.FormatInt(cpus, 10)
	properties[metaMemoryMB] = strconv.FormatInt(memory, 10)
	properties[metaDiskGB] = strconv.FormatInt(disk, 10)
	return properties
}
