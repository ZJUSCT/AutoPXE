#!/bin/sh
set -eu

# Default configuration
IPA_BASEURI="${IPA_BASEURI:-https://tarballs.opendev.org/openstack/ironic-python-agent/dib}"
IPA_BRANCH="${IPA_BRANCH:-master}"
IPA_FLAVOR="${IPA_FLAVOR:-centos9}"
OUTPUT_DIR="${OUTPUT_DIR:-/output}"

FILENAME="${IPA_FILENAME:-ipa-${IPA_FLAVOR}-${IPA_BRANCH}.tar.gz}"
FILENAME_NO_EXT="${FILENAME%.tar.gz}"

mkdir -p "${OUTPUT_DIR}/tmp"

echo "Downloading IPA from ${IPA_BASEURI}/${FILENAME}..."
cd "${OUTPUT_DIR}/tmp"

if ! curl -f -L -O "${IPA_BASEURI}/${FILENAME}"; then
    echo "ERROR: Failed to download ${IPA_BASEURI}/${FILENAME}" >&2
    exit 1
fi

echo "Extracting ${FILENAME}..."
tar -xaf "${FILENAME}"

# The tarball contains files like:
#   ipa-centos9-master.kernel
#   ipa-centos9-master.initramfs
# We symlink them to fixed names for the HTTP server.
rm -f "${OUTPUT_DIR}/ipa.kernel" "${OUTPUT_DIR}/ipa.initramfs"
ln -s "tmp/${FILENAME_NO_EXT}.kernel" "${OUTPUT_DIR}/ipa.kernel"
ln -s "tmp/${FILENAME_NO_EXT}.initramfs" "${OUTPUT_DIR}/ipa.initramfs"

echo "IPA download complete."
ls -la "${OUTPUT_DIR}/"
