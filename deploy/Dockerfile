# mole-server — sing-box REALITY + hysteria2 + Brutal keeper
# NOTE: TCP Brutal DKMS kernel module MUST be installed on the HOST, not in
# this container (it needs kernel headers + /proc write access). The keeper
# Python here writes /proc/net/tcp_brutal/rules — run with --privileged or
# -v /proc:/proc to let it actually apply rules.
#
# Usage:
#   docker build -t ghcr.io/leonting1010/mole-server:latest .
#   docker run -d --name mole-server --net host --privileged \
#     -e MOLE_SERVER_IP=<public-server-ip> \
#     -v /etc/mole:/etc/mole -v /proc:/proc \
#     ghcr.io/leonting1010/mole-server:latest

FROM debian:bookworm-slim AS builder

ENV DEBIAN_FRONTEND=noninteractive
COPY install-functions.sh /tmp/mole-install-functions.sh
RUN apt-get update && apt-get -qqy install curl ca-certificates python3 psmisc && \
    mkdir -p /etc/systemd/system && \
    bash -c 'source /tmp/mole-install-functions.sh; mole_install_singbox; mole_install_hy2'

FROM debian:bookworm-slim

ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update && apt-get -qqy install python3 ca-certificates openssl iproute2 curl && \
    rm -rf /var/lib/apt/lists/* && \
    mkdir -p /etc/sing-box /etc/hysteria /etc/mole /var/log/mole

COPY --from=builder /usr/bin/sing-box /usr/local/bin/sing-box
COPY --from=builder /usr/local/bin/hysteria /usr/local/bin/hysteria
COPY brutal-keeper.py /usr/local/bin/mole-brutal-keeper
COPY configure.py /usr/local/bin/mole-configure
COPY entrypoint.sh /usr/local/bin/mole-entrypoint

RUN chmod 755 /usr/local/bin/mole-brutal-keeper /usr/local/bin/mole-entrypoint && \
    chmod 755 /usr/local/bin/sing-box /usr/local/bin/hysteria

ENV MOLE_DEFAULT_MBPS=100 \
    MOLE_DEFAULT_GAIN=20

EXPOSE 443/tcp 443/udp

VOLUME ["/etc/mole"]

ENTRYPOINT ["/usr/local/bin/mole-entrypoint"]
