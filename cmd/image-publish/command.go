package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	imgoci "github.com/imgoci/go"
	"github.com/spf13/cobra"
)

const (
	ghcrPrefix           = "ghcr.io/"
	imgociArchitecture   = "amd64"
	imgociTarget         = "incus"
	imgociRepresentation = "x-gilmanlab-incus-container"
	imgociRole           = "x-gilmanlab-unified"
	imgociCompression    = "none"
	unifiedFilename      = "router.tar.xz"
	sourceAnnotation     = "https://github.com/GilmanLab/agentcompute"
	digestPrefix         = "sha256:"
	digestHexLen         = 64
	digestKey            = "digest"
)

const transferTimeout = 15 * time.Minute

func newRootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:           "image-publish",
		Short:         "Publish and fetch lab-built Incus images as imgoci releases",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(newPublishCommand())
	root.AddCommand(newFetchCommand())
	root.AddCommand(newInspectCommand())
	return root
}

func newPublishCommand() *cobra.Command {
	var (
		ref      string
		version  string
		file     string
		name     string
		metadata string
		disk     string
	)
	cmd := &cobra.Command{
		Use:           "publish",
		Short:         "Publish an immutable imgoci release",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			files := []imgoci.FileSpec{{
				Source: imgoci.FromFile(file), Filename: unifiedFilename, Selector: unifiedSelector(),
			}}
			if metadata != "" {
				files = []imgoci.FileSpec{
					{Source: imgoci.FromFile(metadata), Filename: "incus.tar.xz", Selector: vmSelector("metadata")},
					{Source: imgoci.FromFile(disk), Filename: "disk.qcow2", Selector: vmSelector("disk")},
				}
			}
			return publishRelease(cmd, ref, version, name, files)
		},
	}
	cmd.Flags().StringVar(&ref, "ref", "", "GHCR tag reference to publish")
	cmd.Flags().StringVar(&version, "version", "", "immutable release version")
	cmd.Flags().StringVar(&file, "file", "", "unified router tarball to publish")
	cmd.Flags().StringVar(&name, "name", "router", "image release name")
	cmd.Flags().StringVar(&metadata, "metadata", "", "split VM metadata archive")
	cmd.Flags().StringVar(&disk, "disk", "", "split VM qcow2 disk")
	_ = cmd.MarkFlagRequired("ref")
	_ = cmd.MarkFlagRequired("version")
	cmd.MarkFlagsOneRequired("file", "metadata")
	cmd.MarkFlagsMutuallyExclusive("file", "metadata")
	cmd.MarkFlagsRequiredTogether("metadata", "disk")
	return cmd
}

func newFetchCommand() *cobra.Command {
	var (
		ref    string
		output string
	)
	var vm bool
	cmd := &cobra.Command{
		Use:           "fetch",
		Short:         "Fetch and verify an imgoci release by digest",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if vm {
				return fetchVMRelease(cmd, ref, output)
			}
			return fetchRelease(cmd, ref, output)
		},
	}
	cmd.Flags().StringVar(&ref, "ref", "", "GHCR digest reference to fetch")
	cmd.Flags().StringVar(&output, "output", "", "destination tarball, or new directory with --vm")
	cmd.Flags().BoolVar(&vm, "vm", false, "fetch split VM metadata and disk")
	_ = cmd.MarkFlagRequired("ref")
	_ = cmd.MarkFlagRequired("output")
	return cmd
}

func publishRelease(cmd *cobra.Command, ref, version, name string, files []imgoci.FileSpec) error {
	if err := requireGHCRRef(ref); err != nil {
		return err
	}
	client, err := imgociClientFromEnv()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), transferTimeout)
	defer cancel()

	if _, fetchErr := client.Fetch(ctx, imgoci.Reference(ref)); fetchErr == nil {
		return fmt.Errorf("release already exists: %s", ref)
	} else if !errors.Is(fetchErr, imgoci.ErrNotFound) {
		return fmt.Errorf("checking release tag: %w", fetchErr)
	}

	digest, err := client.Publish(ctx, imgoci.Reference(ref), imgoci.ReleaseSpec{
		Name: name, Version: version,
		Annotations: map[string]string{"org.opencontainers.image.source": sourceAnnotation},
		Files:       files,
	})
	if err != nil {
		return err
	}
	return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]string{
		"reference": ref, digestKey: digest.String(),
	})
}

func fetchRelease(cmd *cobra.Command, ref, output string) error {
	if err := requireGHCRDigestRef(ref); err != nil {
		return err
	}
	client, err := imgociClientFromEnv()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), transferTimeout)
	defer cancel()

	release, selected, err := resolveUnified(ctx, client, ref)
	if err != nil {
		return err
	}
	entries := selected.Entries()
	if err := client.FetchFiles(
		ctx,
		release,
		selected,
		imgoci.ToFiles(map[string]string{imgociRole: output}),
	); err != nil {
		return err
	}

	return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{
		digestKey:     release.Digest(),
		"fingerprint": contentFingerprint(entries[0]),
		"bytes":       entries[0].ContentSize,
		"file":        output,
	})
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

func requireGHCRRef(ref string) error {
	if !strings.HasPrefix(ref, ghcrPrefix) {
		return errors.New("--ref must name ghcr.io; credentials are scoped to this registry")
	}
	return nil
}

func requireGHCRDigestRef(ref string) error {
	if err := requireGHCRRef(ref); err != nil {
		return err
	}
	if !isImgociDigestRef(ref) {
		return errors.New("fetch requires --ref ghcr.io/owner/image@sha256:<64 lowercase hex digits>")
	}
	return nil
}

func isImgociDigestRef(ref string) bool {
	name, digest, ok := strings.Cut(ref, "@")
	if !ok || !strings.HasPrefix(name, ghcrPrefix) {
		return false
	}
	if !strings.HasPrefix(digest, digestPrefix) {
		return false
	}
	hex := digest[len(digestPrefix):]
	if len(hex) != digestHexLen {
		return false
	}
	for i := range hex {
		c := hex[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func unifiedSelector() imgoci.Selector {
	return imgoci.Selector{
		Architecture:   imgociArchitecture,
		Target:         imgociTarget,
		Representation: imgociRepresentation,
		Role:           imgociRole,
		Compression:    imgociCompression,
	}
}

func vmSelector(role string) imgoci.Selector {
	return imgoci.Selector{
		Architecture: imgociArchitecture, Target: imgociTarget,
		Representation: "incus-vm", Role: role, Compression: imgociCompression,
	}
}

func fetchVMRelease(cmd *cobra.Command, ref, output string) error {
	if err := requireGHCRDigestRef(ref); err != nil {
		return err
	}
	client, err := imgociClientFromEnv()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), transferTimeout)
	defer cancel()
	release, err := client.Fetch(ctx, imgoci.Reference(ref))
	if err != nil {
		return err
	}
	selected, err := client.Resolve(release, imgoci.ResolveQuery{
		Architecture: imgociArchitecture, Target: imgociTarget,
		Representation: "incus-vm", Roles: []string{"metadata", "disk"},
		Compressions: []string{imgociCompression},
	})
	if err != nil {
		return err
	}
	if err := client.FetchFiles(ctx, release, selected, imgoci.ToDir(output)); err != nil {
		return err
	}
	return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{
		digestKey: release.Digest(), "directory": output,
	})
}

func newInspectCommand() *cobra.Command {
	var ref string
	cmd := &cobra.Command{
		Use: "inspect", Short: "Report whether an immutable release exists", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := requireGHCRRef(ref); err != nil {
				return err
			}
			client, err := imgociClientFromEnv()
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), transferTimeout)
			defer cancel()
			release, err := client.Fetch(ctx, imgoci.Reference(ref))
			if errors.Is(err, imgoci.ErrNotFound) {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{"exists": false})
			}
			if err != nil {
				return err
			}
			return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{
				"exists": true, digestKey: release.Digest(),
			})
		},
	}
	cmd.Flags().StringVar(&ref, "ref", "", "GHCR tag reference to inspect")
	_ = cmd.MarkFlagRequired("ref")
	return cmd
}

func contentFingerprint(entry imgoci.FileEntry) string {
	return strings.TrimPrefix(entry.ContentDigest.String(), digestPrefix)
}
