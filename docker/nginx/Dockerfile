FROM nginx:alpine

COPY nginx.conf /etc/nginx/nginx.conf

# Content is served from /srv:
#   /srv/boot.ipxe          - iPXE chainload script
#   /srv/ipa.kernel         - IPA kernel
#   /srv/ipa.initramfs      - IPA initramfs
#   /srv/images/*.qcow2     - Disk images
