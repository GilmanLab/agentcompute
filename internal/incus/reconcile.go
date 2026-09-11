package incus

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode"

	imgoci "github.com/imgoci/go"
	incusclient "github.com/lxc/incus/v7/client"
	"github.com/lxc/incus/v7/shared/api"

	"github.com/GilmanLab/agentcompute/internal/compute"
)

const (
	imageBuildProject    = "image-build"
	imageBuildProfile    = "default"
	digestProperty       = "agentcompute.digest"
	ghcrPrefix           = "ghcr.io/"
	imgociArchitecture   = "amd64"
	imgociTarget         = "incus"
	imgociRepresentation = "x-gilmanlab-incus-container"
	imgociRole           = "x-gilmanlab-unified"
	imgociCompression    = "none"
	unifiedFilename      = "router.tar.xz"
	digestPrefix         = "sha256:"
	digestHexLen         = 64
	smokePrefix          = "ac-smoke-"
	smokeIDBytes         = 4
	maxInstanceName      = 63
	sourceTypeImage      = "image"
	startAction          = "start"
	stopAction           = "stop"
	trueCommand          = "/bin/true"
)

const (
	smokeTimeout      = 3 * time.Minute
	cleanupTimeout    = time.Minute
	readyPollInterval = 2 * time.Second
	stateTimeoutSec   = 30
)

// EnsureCatalog imports catalog images into the image-build project and returns copies
// with derived fingerprints.
//
// Lab-built imgoci digest references are fetched, verified, imported, and promoted to a
// local alias only after a successful smoke launch. A matching agentcompute.digest on
// the current alias skips import and smoke. Upstream remote:alias references only ensure
// the Incus remote exists. Returned entries keep the original Reference.
func (c *Client) EnsureCatalog(ctx context.Context, images []compute.CatalogImage) ([]compute.CatalogImage, error) {
	if c == nil {
		return nil, errors.New("incus client is nil")
	}
	out := make([]compute.CatalogImage, 0, len(images))
	for _, image := range images {
		ensured, err := c.ensureImage(ctx, image)
		if err != nil {
			return nil, mapError(err)
		}
		out = append(out, ensured)
	}
	return out, nil
}

func (c *Client) ensureImage(ctx context.Context, image compute.CatalogImage) (compute.CatalogImage, error) {
	out := copyCatalogImage(image)
	switch {
	case isImgociDigestRef(image.Reference):
		fingerprint, err := c.ensureDigestImage(ctx, image)
		if err != nil {
			return compute.CatalogImage{}, err
		}
		out.Fingerprint = fingerprint
		return out, nil
	case isUpstreamRef(image.Reference):
		if err := c.ensureUpstreamImage(ctx, image.Reference); err != nil {
			return compute.CatalogImage{}, err
		}
		return out, nil
	default:
		return compute.CatalogImage{}, fmt.Errorf(
			"catalog image %q: unsupported reference %q",
			image.Name,
			image.Reference,
		)
	}
}

func (c *Client) ensureDigestImage(ctx context.Context, image compute.CatalogImage) (string, error) {
	digest, ok := imgociDigest(image.Reference)
	if !ok {
		return "", fmt.Errorf("catalog image %q: invalid imgoci reference %q", image.Name, image.Reference)
	}
	server := c.Scoped(ctx, imageBuildProject, c.host)
	if fingerprint, hit, err := aliasHasDigest(server, image.Name, digest); err != nil {
		return "", err
	} else if hit {
		return fingerprint, nil
	}

	fingerprint, err := importVerifiedImage(ctx, server, image.Reference, digest)
	if err != nil {
		return "", fmt.Errorf("catalog image %q: %w", image.Name, err)
	}
	if err := smokeLaunch(ctx, server, image.Name, fingerprint); err != nil {
		return "", fmt.Errorf("catalog image %q: %w", image.Name, err)
	}
	if err := recordDigest(server, fingerprint, digest); err != nil {
		return "", fmt.Errorf("catalog image %q: %w", image.Name, err)
	}
	if err := promoteAlias(server, image.Name, fingerprint); err != nil {
		return "", fmt.Errorf("catalog image %q: %w", image.Name, err)
	}
	return fingerprint, nil
}

func (c *Client) ensureUpstreamImage(ctx context.Context, reference string) error {
	remote, _, ok := splitRemoteAlias(reference)
	if !ok {
		return fmt.Errorf("invalid upstream reference %q", reference)
	}
	if _, err := c.RemoteImage(ctx, remote); err != nil {
		return fmt.Errorf("ensuring remote %q: %w", remote, err)
	}
	return nil
}

func importVerifiedImage(
	ctx context.Context,
	server incusclient.InstanceServer,
	reference, digest string,
) (string, error) {
	if fingerprint, found, err := imageWithDigest(server, digest); err != nil {
		return "", err
	} else if found {
		return fingerprint, nil
	}

	client, err := imgociClientFromEnv()
	if err != nil {
		return "", err
	}
	release, selected, err := resolveUnified(ctx, client, reference)
	if err != nil {
		return "", err
	}
	entries := selected.Entries()
	contentFP := contentFingerprint(entries[0])

	if existing, _, getErr := server.GetImage(contentFP); getErr == nil {
		return existing.Fingerprint, nil
	} else if !isNotFound(getErr) {
		return "", getErr
	}

	temporary, err := os.MkdirTemp("", "agentcompute-image-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(temporary)
	destination := filepath.Join(temporary, unifiedFilename)
	if fetchErr := client.FetchFiles(
		ctx,
		release,
		selected,
		imgoci.ToFiles(map[string]string{imgociRole: destination}),
	); fetchErr != nil {
		return "", fetchErr
	}

	imported, err := importUnifiedTarball(ctx, server, destination)
	if err != nil {
		if isConflict(err) {
			image, _, getErr := server.GetImage(contentFP)
			if getErr != nil {
				return "", err
			}
			return image.Fingerprint, nil
		}
		return "", err
	}
	return imported, nil
}

func importUnifiedTarball(ctx context.Context, server incusclient.InstanceServer, path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()

	req := api.ImagesPost{
		Filename: filepath.Base(path),
	}
	op, err := server.CreateImage(req, &incusclient.ImageCreateArgs{
		MetaFile: file,
		MetaName: filepath.Base(path),
		Type:     string(api.InstanceTypeContainer),
	})
	if err != nil {
		return "", err
	}
	if err := op.WaitContext(ctx); err != nil {
		return "", err
	}
	return fingerprintFromOp(op)
}

func recordDigest(server incusclient.InstanceServer, fingerprint, digest string) error {
	image, etag, err := server.GetImage(fingerprint)
	if err != nil {
		return err
	}
	current, ok := image.Properties[digestProperty]
	if ok && current == digest {
		return nil
	}
	if ok && current != digest {
		return fmt.Errorf("image %s has digest %s, not %s", fingerprint, current, digest)
	}
	writable := image.Writable()
	if writable.Properties == nil {
		writable.Properties = map[string]string{}
	}
	writable.Properties[digestProperty] = digest
	return server.UpdateImage(fingerprint, writable, etag)
}

func smokeLaunch(
	ctx context.Context,
	server incusclient.InstanceServer,
	imageName, fingerprint string,
) (err error) {
	name, err := smokeInstanceName(imageName)
	if err != nil {
		return err
	}
	req := api.InstancesPost{
		Name: name,
		Type: api.InstanceTypeContainer,
		Source: api.InstanceSource{
			Type:        sourceTypeImage,
			Fingerprint: fingerprint,
		},
	}
	req.Profiles = []string{imageBuildProfile}
	op, err := server.CreateInstance(req)
	if err != nil {
		return err
	}
	defer func() {
		if delErr := deleteInstance(ctx, server, name); delErr != nil {
			err = errors.Join(err, delErr)
		}
	}()
	if err = op.WaitContext(ctx); err != nil {
		return err
	}

	smokeCtx, cancel := context.WithTimeout(ctx, smokeTimeout)
	defer cancel()
	startOp, err := server.UpdateInstanceState(name, api.InstanceStatePut{
		Action:  startAction,
		Timeout: stateTimeoutSec,
	}, "")
	if err != nil {
		return err
	}
	if err = startOp.WaitContext(smokeCtx); err != nil {
		return err
	}
	if err = waitGuestReady(smokeCtx, server, name); err != nil {
		return err
	}
	for _, command := range routerChecks() {
		if err = execCommand(smokeCtx, server, name, command); err != nil {
			return fmt.Errorf("smoke check %s: %w", strings.Join(command, " "), err)
		}
	}
	return nil
}

func promoteAlias(server incusclient.InstanceServer, name, fingerprint string) error {
	alias, etag, err := server.GetImageAlias(name)
	if isNotFound(err) {
		return server.CreateImageAlias(api.ImageAliasesPost{
			ImageAliasesEntry: api.ImageAliasesEntry{
				Name: name,
				ImageAliasesEntryPut: api.ImageAliasesEntryPut{
					Target: fingerprint,
				},
			},
		})
	}
	if err != nil {
		return err
	}
	if alias.Target == fingerprint {
		return nil
	}
	return server.UpdateImageAlias(name, api.ImageAliasesEntryPut{
		Description: alias.Description,
		Target:      fingerprint,
	}, etag)
}

func aliasHasDigest(server incusclient.InstanceServer, name, digest string) (string, bool, error) {
	alias, _, err := server.GetImageAlias(name)
	if isNotFound(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	image, _, err := server.GetImage(alias.Target)
	if isNotFound(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if image.Properties[digestProperty] != digest {
		return "", false, nil
	}
	return image.Fingerprint, true, nil
}

func imageWithDigest(server incusclient.InstanceServer, digest string) (string, bool, error) {
	images, err := server.GetImages()
	if err != nil {
		return "", false, err
	}
	for _, image := range images {
		if image.Properties[digestProperty] == digest {
			return image.Fingerprint, true, nil
		}
	}
	return "", false, nil
}

func waitGuestReady(ctx context.Context, server incusclient.InstanceServer, name string) error {
	if err := execCommand(ctx, server, name, []string{trueCommand}); err == nil {
		return nil
	}
	ticker := time.NewTicker(readyPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("instance %s was not reachable: %w", name, ctx.Err())
		case <-ticker.C:
			if err := execCommand(ctx, server, name, []string{trueCommand}); err == nil {
				return nil
			}
		}
	}
}

func execCommand(ctx context.Context, server incusclient.InstanceServer, name string, command []string) error {
	done := make(chan bool)
	args := &incusclient.InstanceExecArgs{
		Stdin:    bytes.NewReader(nil),
		Stdout:   io.Discard,
		Stderr:   io.Discard,
		DataDone: done,
	}
	op, err := server.ExecInstance(name, api.InstanceExecPost{
		Command:   command,
		WaitForWS: true,
	}, args)
	if err != nil {
		return err
	}
	waitErr := op.WaitContext(ctx)
	select {
	case <-done:
	case <-ctx.Done():
		if waitErr == nil {
			waitErr = ctx.Err()
		}
	}
	if waitErr != nil {
		return waitErr
	}
	code, ok := op.Get().Metadata["return"].(float64)
	if !ok {
		return errors.New("exec returned no exit status")
	}
	if code != 0 {
		return fmt.Errorf("exit %d", int(code))
	}
	return nil
}

func deleteInstance(ctx context.Context, server incusclient.InstanceServer, name string) error {
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	server = requestContext(cleanup, server.UseTarget(""))

	stopOp, err := server.UpdateInstanceState(name, api.InstanceStatePut{
		Action:  stopAction,
		Timeout: stateTimeoutSec,
		Force:   true,
	}, "")
	if err == nil {
		_ = stopOp.WaitContext(cleanup)
	}

	delOp, err := server.DeleteInstance(name)
	if isNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	return delOp.WaitContext(cleanup)
}

func resolveUnified(ctx context.Context, client *imgoci.Client, ref string) (*imgoci.Release, *imgoci.Resolved, error) {
	release, err := client.Fetch(ctx, imgoci.Reference(ref))
	if err != nil {
		return nil, nil, err
	}
	selected, err := client.Resolve(release, imgoci.ResolveQuery{
		Architecture:   imgociArchitecture,
		Target:         imgociTarget,
		Representation: imgociRepresentation,
		Roles:          []string{imgociRole},
		Compressions:   []string{imgociCompression},
	})
	if err != nil {
		return nil, nil, err
	}
	if len(selected.Entries()) != 1 {
		return nil, nil, errors.New("router release must resolve exactly one unified archive")
	}
	return release, selected, nil
}

func imgociClientFromEnv() (*imgoci.Client, error) {
	var options []imgoci.Option
	if token := os.Getenv("GHCR_TOKEN"); token != "" {
		user := os.Getenv("GHCR_USERNAME")
		if user == "" {
			return nil, errors.New("GHCR_USERNAME is required with GHCR_TOKEN")
		}
		options = append(options, imgoci.WithCredentials(user, token))
	}
	return imgoci.New(options...)
}

func copyCatalogImage(image compute.CatalogImage) compute.CatalogImage {
	image.Kinds = slices.Clone(image.Kinds)
	image.Fingerprint = ""
	return image
}

func isImgociDigestRef(ref string) bool {
	name, digest, ok := strings.Cut(ref, "@")
	if !ok || !strings.HasPrefix(name, ghcrPrefix) || !strings.HasPrefix(digest, digestPrefix) {
		return false
	}
	hexDigits := digest[len(digestPrefix):]
	if len(hexDigits) != digestHexLen {
		return false
	}
	for i := range hexDigits {
		c := hexDigits[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func imgociDigest(ref string) (string, bool) {
	if !isImgociDigestRef(ref) {
		return "", false
	}
	_, digest, _ := strings.Cut(ref, "@")
	return digest, true
}

func isUpstreamRef(ref string) bool {
	_, _, ok := splitRemoteAlias(ref)
	return ok
}

func splitRemoteAlias(ref string) (string, string, bool) {
	if isImgociDigestRef(ref) {
		return "", "", false
	}
	remote, alias, ok := strings.Cut(ref, ":")
	if !ok || remote == "" || alias == "" || strings.Contains(remote, "/") {
		return "", "", false
	}
	return remote, alias, true
}

func contentFingerprint(entry imgoci.FileEntry) string {
	return strings.TrimPrefix(entry.ContentDigest.String(), digestPrefix)
}

func fingerprintFromOp(op incusclient.Operation) (string, error) {
	fp, ok := op.Get().Metadata["fingerprint"].(string)
	if !ok || fp == "" {
		return "", errors.New("image import returned no fingerprint")
	}
	return fp, nil
}

func smokeInstanceName(image string) (string, error) {
	suffix, err := randomHex(smokeIDBytes)
	if err != nil {
		return "", err
	}
	cleaned := strings.Map(func(r rune) rune {
		if unicode.IsUpper(r) {
			r = unicode.ToLower(r)
		}
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			return r
		}
		return -1
	}, image)
	if cleaned == "" {
		cleaned = sourceTypeImage
	}
	name := smokePrefix + cleaned + "-" + suffix
	if len(name) <= maxInstanceName {
		return name, nil
	}
	keep := max(maxInstanceName-len(smokePrefix)-len(suffix)-1, 1)
	keep = min(keep, len(cleaned))
	return smokePrefix + cleaned[:keep] + "-" + suffix, nil
}

func randomHex(nBytes int) (string, error) {
	buf := make([]byte, nBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

func routerChecks() [][]string {
	const versionFlag = "--version"
	return [][]string{
		{"nft", versionFlag},
		{"vtysh", "--help"},
		{"tc", "-V"},
		{"dnsmasq", versionFlag},
		{"wg", versionFlag},
		{"tcpdump", versionFlag},
	}
}

func isNotFound(err error) bool {
	return api.StatusErrorCheck(err, http.StatusNotFound)
}
