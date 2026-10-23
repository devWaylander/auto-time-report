# VPN-sidecar: только OpenVPN и iproute2, без сети наружу и без секретов на диске.
# Конфиг на время сессии кладёт бот в общий tmpfs, пароль передаётся через management-сокет
FROM alpine:3.22
RUN apk add --no-cache openvpn iproute2
COPY entrypoint.sh /usr/local/bin/vpn-sidecar
ENTRYPOINT ["/usr/local/bin/vpn-sidecar"]
