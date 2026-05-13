#!/bin/sh
set -eu

OUTPUT_DIR="${OUTPUT_DIR:-/output}"
IPXE_REPO="${IPXE_REPO:-https://github.com/ipxe/ipxe.git}"
IPXE_BRANCH="${IPXE_BRANCH:-master}"

# Optional ccache integration. Caller mounts a host directory at $CCACHE_DIR
# and exports USE_CCACHE=1; we shim ccache in front of gcc/cc on PATH so
# iPXE's Makefile picks it up transparently.
if [ "${USE_CCACHE:-0}" = "1" ] && command -v ccache > /dev/null 2>&1; then
    echo "ccache enabled (CCACHE_DIR=${CCACHE_DIR:-default})"
    mkdir -p /tmp/ccache-shim
    ln -sf "$(command -v ccache)" /tmp/ccache-shim/gcc
    ln -sf "$(command -v ccache)" /tmp/ccache-shim/cc
    export PATH="/tmp/ccache-shim:${PATH}"
    ccache --max-size="${CCACHE_MAX_SIZE:-500M}" > /dev/null
    ccache -z > /dev/null
fi

echo "Cloning iPXE (${IPXE_BRANCH})..."
git clone --depth 1 --branch "${IPXE_BRANCH}" "${IPXE_REPO}" /tmp/ipxe

cd /tmp/ipxe/src

# Enable serial console
sed -i 's|^//#define[[:space:]]*CONSOLE_SERIAL|#define CONSOLE_SERIAL|g' config/console.h

# Build BIOS firmware
echo "Building undionly.kpxe..."
make bin/undionly.kpxe EMBED=/bin/embed.ipxe NO_WERROR=1 -j"$(nproc)"

# Detect arch for EFI build
ARCH=$(uname -m | sed 's/aarch/arm/')

# Build EFI firmware
echo "Building snponly.efi for ${ARCH}..."
make "bin-${ARCH}-efi/snponly.efi" EMBED=/bin/embed.ipxe NO_WERROR=1 -j"$(nproc)"

mkdir -p "${OUTPUT_DIR}"
cp bin/undionly.kpxe "${OUTPUT_DIR}/"
cp "bin-${ARCH}-efi/snponly.efi" "${OUTPUT_DIR}/"

echo "iPXE build complete."
ls -la "${OUTPUT_DIR}/"

# Print ccache stats for visibility in CI logs.
if [ "${USE_CCACHE:-0}" = "1" ] && command -v ccache > /dev/null 2>&1; then
    echo "--- ccache stats ---"
    ccache -s
fi
