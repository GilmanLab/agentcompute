// Command images is a temporary router publisher and Incus importer.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	imgoci "github.com/imgoci/go"
)

const representation = "x-gilmanlab-incus-container"
const role = "x-gilmanlab-unified"

var digestRef = regexp.MustCompile(`^ghcr\.io/[a-z0-9._/-]+@sha256:[0-9a-f]{64}$`)
var incusName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

var commands = [][]string{
	{"nft", "--version"},
	{"vtysh", "--help"},
	{"tc", "-V"},
	{"dnsmasq", "--version"},
	{"wg", "--version"},
	{"tcpdump", "--version"},
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "images:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: images publish|fetch|import [flags]")
	}
	mode := args[0]
	if mode != "publish" && mode != "fetch" && mode != "import" {
		return fmt.Errorf("unknown command %q", mode)
	}
	flags := flag.NewFlagSet(mode, flag.ContinueOnError)
	ref := flags.String("ref", "", "GHCR reference (digest required for fetch/import)")
	file := flags.String("file", "", "unified router tarball to publish")
	version := flags.String("version", "", "immutable release version to publish")
	output := flags.String("output", "", "destination tarball for fetch")
	remote := flags.String("remote", "", "configured Incus remote name (required for import)")
	project := flags.String("project", "", "Incus project name (required for import)")
	if err := flags.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	if !strings.HasPrefix(*ref, "ghcr.io/") {
		return errors.New("--ref must name ghcr.io; credentials are scoped to this registry")
	}
	var options []imgoci.Option
	if token := os.Getenv("GHCR_TOKEN"); token != "" {
		user := os.Getenv("GHCR_USERNAME")
		if user == "" {
			return errors.New("GHCR_USERNAME is required with GHCR_TOKEN")
		}
		options = append(options, imgoci.WithCredentials(user, token))
	}
	client, err := imgoci.New(options...)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	if mode == "publish" {
		if *file == "" || *version == "" {
			return errors.New("publish requires --file and --version")
		}
		// Immutable means this tool refuses to replace an existing reference.
		if _, err := client.Fetch(ctx, imgoci.Reference(*ref)); err == nil {
			return fmt.Errorf("release already exists: %s", *ref)
		} else if !errors.Is(err, imgoci.ErrNotFound) {
			return fmt.Errorf("checking release tag: %w", err)
		}
		digest, err := client.Publish(ctx, imgoci.Reference(*ref), imgoci.ReleaseSpec{
			Name:    "router",
			Version: *version,
			Annotations: map[string]string{
				"org.opencontainers.image.source": "https://github.com/GilmanLab/agentcompute",
			},
			Files: []imgoci.FileSpec{
				{Source: imgoci.FromFile(*file), Filename: "router.tar.xz", Selector: imgoci.Selector{
					Architecture:   "amd64",
					Target:         "incus",
					Representation: representation,
					Role:           role,
					Compression:    "none",
				}},
			},
		})
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(map[string]string{"reference": *ref, "digest": digest.String()})
	}
	if !digestRef.MatchString(*ref) {
		return errors.New("fetch/import requires --ref ghcr.io/owner/image@sha256:<64 lowercase hex digits>")
	}
	if mode == "import" &&
		(!incusName.MatchString(*remote) || !incusName.MatchString(*project) || *project == "default") {
		return errors.New("import requires named --remote and non-default --project")
	}
	if mode == "fetch" && *output == "" {
		return errors.New("fetch requires --output")
	}
	release, err := client.Fetch(ctx, imgoci.Reference(*ref))
	if err != nil {
		return err
	}
	selected, err := client.Resolve(release, imgoci.ResolveQuery{
		Architecture:   "amd64",
		Target:         "incus",
		Representation: representation,
		Roles:          []string{role},
		Compressions:   []string{"none"},
	})
	if err != nil {
		return err
	}
	entries := selected.Entries()
	if len(entries) != 1 {
		return errors.New("router release must resolve exactly one unified archive")
	}
	temporary, err := os.MkdirTemp("", "agentcompute-image-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(temporary)
	destination := filepath.Join(temporary, "router.tar.xz")
	if mode == "fetch" {
		destination = *output
	}
	if err := client.FetchFiles(
		ctx,
		release,
		selected,
		imgoci.ToFiles(map[string]string{role: destination}),
	); err != nil {
		return err
	}
	fingerprint := strings.TrimPrefix(entries[0].ContentDigest.String(), "sha256:")
	if mode == "fetch" {
		return json.NewEncoder(os.Stdout).
			Encode(map[string]any{"digest": release.Digest(), "fingerprint": fingerprint, "bytes": entries[0].ContentSize, "file": destination})
	}
	incus := func(ctx context.Context, args ...string) error {
		command := exec.CommandContext(ctx, "incus", append([]string{"--quiet", "--project", *project}, args...)...)
		command.Stdout, command.Stderr = os.Stderr, os.Stderr
		if err := command.Run(); err != nil {
			return fmt.Errorf("incus %s: %w", strings.Join(args, " "), err)
		}
		return nil
	}
	query := func(path string, result any) error {
		command := exec.CommandContext(ctx, "incus", "query", *remote+":"+path+"?project="+*project+"&recursion=1")
		command.Stderr = os.Stderr
		data, err := command.Output()
		if err != nil {
			return fmt.Errorf("querying Incus %s: %w", path, err)
		}
		return json.Unmarshal(data, result)
	}
	// Content is fully verified before Incus sees it; the existing router alias
	// remains untouched until the candidate fingerprint passes its boot test.
	var images []struct{ Fingerprint string }
	if err := query("/1.0/images", &images); err != nil {
		return err
	}
	present := false
	for _, image := range images {
		present = present || image.Fingerprint == fingerprint
	}
	if !present {
		if err := incus(ctx, "image", "import", destination, *remote+":"); err != nil {
			return err
		}
	}
	instance := "router-check-" + filepath.Base(temporary)
	instance = strings.ReplaceAll(instance, "agentcompute-image-", "")
	if err := incus(ctx, "init", *remote+":"+fingerprint, *remote+":"+instance); err != nil {
		return err
	}
	removed := false
	defer func() {
		if removed {
			return
		}
		cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := incus(cleanup, "delete", "-f", *remote+":"+instance); err != nil {
			fmt.Fprintln(os.Stderr, "cleanup:", err)
		}
	}()
	if err := incus(ctx, "start", *remote+":"+instance); err != nil {
		return err
	}
	for _, command := range commands {
		args := append([]string{"exec", *remote + ":" + instance, "--"}, command...)
		if err := incus(ctx, args...); err != nil {
			return err
		}
	}
	if err := incus(ctx, "delete", "-f", *remote+":"+instance); err != nil {
		return err
	}
	removed = true
	var aliases []struct{ Name, Target string }
	if err := query("/1.0/images/aliases", &aliases); err != nil {
		return err
	}
	aliasExists := false
	for _, alias := range aliases {
		aliasExists = aliasExists || alias.Name == "router"
	}
	if aliasExists {
		payload, err := json.Marshal(map[string]string{"target": fingerprint})
		if err != nil {
			return err
		}
		if err := incus(
			ctx,
			"query",
			"-X",
			"PUT",
			"-d",
			string(payload),
			*remote+":/1.0/images/aliases/router?project="+*project,
		); err != nil {
			return err
		}
	} else if err := incus(ctx, "image", "alias", "create", *remote+":router", fingerprint); err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).
		Encode(map[string]any{"digest": release.Digest(), "fingerprint": fingerprint, "bytes": entries[0].ContentSize, "remote": *remote, "project": *project, "alias": "router"})
}
