#!/bin/sh
set -eu

INTERFACE="${DNSMASQ_INTERFACE:-eth0}"
LISTEN_ADDRESS="${LISTEN_ADDRESS:-192.168.100.1}"
DHCP_RANGE="${DHCP_RANGE:-192.168.100.100,192.168.100.200,255.255.255.0,12h}"
GATEWAY_IP="${GATEWAY_IP:-192.168.100.1}"
DNS_SERVER="${DNS_SERVER:-8.8.8.8}"

echo "Configuring dnsmasq..."
echo "  Interface:       ${INTERFACE}"
echo "  Listen Address:  ${LISTEN_ADDRESS}"
echo "  DHCP Range:      ${DHCP_RANGE}"
echo "  Gateway:         ${GATEWAY_IP}"
echo "  DNS:             ${DNS_SERVER}"

sed -i "s|{{LISTEN_ADDRESS}}|${LISTEN_ADDRESS}|g" /etc/dnsmasq.conf
sed -i "s|{{DHCP_RANGE}}|${DHCP_RANGE}|g" /etc/dnsmasq.conf
sed -i "s|{{GATEWAY_IP}}|${GATEWAY_IP}|g" /etc/dnsmasq.conf
sed -i "s|{{DNS_SERVER}}|${DNS_SERVER}|g" /etc/dnsmasq.conf

# Verify iPXE firmware is present
echo "TFTP root contents:"
ls -la /var/lib/tftpboot/ || echo "WARNING: no iPXE firmware found"

exec dnsmasq -d -C /etc/dnsmasq.conf -i "${INTERFACE}"
