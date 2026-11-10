# mole-server — sing-box REALITY + hysteria2 + Brutal keeper
# NOTE: TCP Brutal DKMS kernel module MUST be installed on the HOST, not in
# this container (it needs kernel headers + /proc write access). The keeper
# Python here writes /proc/net/tcp_brutal/rules — run with --privileged or
# --device /proc:/proc to let it actually apply rules.
#
# Usage:
#   docker build -t ghcr.io/leonting1010/mole-server:latest .
#   docker run -d --name mole-server --net host --privileged \
#     -e REALITY_PRIV=... -e REALITY_SHORT=... -e USER_UUID=... -e HY2_PASSWORD=... \
#     -v /etc/mole:/etc/mole -v /proc:/proc \
#     ghcr.io/leonting1010/mole-server:latest

FROM debian:bookworm-slim AS builder

ARG SB_VERSION=1.14.4 SB_ARCH=amd64 \
    HY2_VERSION=v2.13.0 HY2_ARCH=amd64

ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update && apt-get -qqy install curl ca-certificates python3 && \
    SB_DEB=$(mktemp --suffix=.deb) && \
    curl -fsSL -o "$SB_DEB" "https://github.com/SagerNet/sing-box/releases/download/v${SB_VERSION}/sing-box_${SB_VERSION}_linux_${SB_ARCH}.deb" && \
    dpkg -i "$SB_DEB" && rm -f "$SB_DEB" && \
    HY2_BIN=$(mktemp) && \
    curl -fsSL -o "$HY2_BIN" "https://github.com/HyNetworks/hysteria/releases/download/app/${HY2_VERSION}/hysteria-linux-${HY2_ARCH}" && \
    install -m 755 "$HY2_BIN" /usr/local/bin/hysteria && rm -f "$HY2_BIN"

FROM debian:bookworm-slim

ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update && apt-get -qqy install python3 ca-certificates && \
    rm -rf /var/lib/apt/lists/* && \
    mkdir -p /etc/sing-box /etc/hysteria /etc/mole /var/log/mole

COPY --from=builder /usr/bin/sing-box /usr/local/bin/sing-box
COPY --from=builder /usr/local/bin/hysteria /usr/local/bin/hysteria
COPY brutal-keeper.py /usr/local/bin/mole-brutal-keeper
COPY entrypoint.sh /usr/local/bin/mole-entrypoint

RUN chmod 755 /usr/local/bin/mole-brutal-keeper /usr/local/bin/mole-entrypoint && \
    chmod 755 /usr/local/bin/sing-box /usr/local/bin/hysteria

ENV MOLE_DEFAULT_MBPS=100 \
    MOLE_DEFAULT_GAIN=20

EXPOSE 443/tcp 443/udp 8098/tcp

VOLUME ["/etc/mole"]

ENTRYPOINT ["/usr/local/bin/mole-entrypoint"]
