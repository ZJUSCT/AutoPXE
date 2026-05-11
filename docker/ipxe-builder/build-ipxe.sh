#!/bin/sh
set -eu

OUTPUT_DIR="${OUTPUT_DIR:-/output}"
IPXE_REPO="${IPXE_REPO:-https://github.com/ipxe/ipxe.git}"
IPXE_BRANCH="${IPXE_BRANCH:-master}"

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
