# spikes/images

Throwaway publisher and importer for the `router` image. It exists to prove
the path GHCR release → digest → verified bytes → `incus image import` → boot
test → alias promotion before that logic moves into the Go server's catalog
reconciler (Phase 2). It is deliberately small and ugly; do not extend it.

It is a separate Go module so the server module never depends on it. Pinned:
`github.com/imgoci/go v0.1.0` (pre-v1; bump deliberately).

## Commands

```sh
cd spikes/images && go build -o images .

# Publish an immutable release (refuses an existing tag).
GHCR_USERNAME=<user> GHCR_TOKEN=<token> \
  ./images publish --ref ghcr.io/gilmanlab/agentcompute/router:<version> \
    --version <version> --file router.tar.xz

# Fetch and verify by digest only.
./images fetch --ref ghcr.io/gilmanlab/agentcompute/router@sha256:<digest> \
  --output router.tar.xz

# Verify, import, boot-test, then move the `router` alias.
INCUS_CONF=<dir with client.crt/client.key/servercerts/nas01.crt/config.yml> \
  ./images import --ref ghcr.io/gilmanlab/agentcompute/router@sha256:<digest> \
    --remote nas01 --project image-build
```

`import` never touches the existing `router` alias until the candidate
fingerprint has booted and run `nft --version`, `vtysh --help`, `tc -V`,
`dnsmasq --version`, `wg --version`, and `tcpdump --version`. The test
instance is deleted on every exit path. `GHCR_TOKEN` is only presented to
`ghcr.io`; anything else is rejected before a connection is made.

`smoke.sh` is the CI variant: it boots a local, unpublished tarball under a
temporary alias in the same project and cleans up only what it created.
