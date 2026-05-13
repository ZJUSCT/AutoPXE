# Embedded iPXE binaries

This directory holds the prebuilt iPXE bootloader artifacts that `autopxe`
embeds via `go:embed` and serves over TFTP:

- `undionly.kpxe`   — BIOS / legacy x86 PXE
- `snponly.efi`     — UEFI x86_64

These files are produced by `docker/ipxe-builder`. From the repo root:

```sh
docker build -t autopxe/ipxe-builder docker/ipxe-builder
docker run --rm -v "$(pwd)/internal/assets/ipxe":/output autopxe/ipxe-builder
```

After the build, `go build ./cmd/autopxe` will pick the artifacts up.

The placeholder `.gitkeep` files in this directory exist so `go:embed` succeeds
on a fresh checkout. They are replaced by real binaries when the builder runs.
