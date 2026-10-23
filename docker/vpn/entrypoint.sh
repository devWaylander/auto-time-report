#!/bin/sh
# Ждёт конфиг от бота в общем tmpfs, запускает openvpn и после его выхода ждёт следующую сессию
set -eu

dir="${VPN_RUN_DIR:-/run/vpn}"
cfg="$dir/client.ovpn"
# Сокет и файлы доступны группе бота, остальным — нет
umask 007

echo "vpn sidecar: ready"
while :; do
	if [ -f "$cfg" ]; then
		echo "vpn sidecar: session started"
		# --script-security 1 идёт после --config и перекрывает его: пользовательские up/down-скрипты
		# из .ovpn не выполнятся. Вывод openvpn по умолчанию не пишем: в нём адрес корпоративного сервера,
		# а логи Docker лежат на диске
		if [ "${VPN_DEBUG:-0}" = "1" ]; then
			openvpn --config "$cfg" --script-security 1 || echo "vpn sidecar: openvpn exited with $?"
		else
			openvpn --config "$cfg" --script-security 1 >/dev/null 2>&1 || echo "vpn sidecar: openvpn exited with $?"
		fi
		rm -f "$cfg" "$dir/mgmt.sock"
		echo "vpn sidecar: session finished"
	fi
	sleep 1
done
