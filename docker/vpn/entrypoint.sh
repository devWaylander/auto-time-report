#!/bin/sh
# Ждёт конфиг от бота в общем tmpfs, запускает openvpn и после его выхода ждёт следующую сессию
set -eu

dir="${VPN_RUN_DIR:-/run/vpn}"
cfg="$dir/client.ovpn"
# Бот укладывается в 5 минут на всю отправку, с запасом
max="${VPN_MAX_SESSION:-360}"
# Сокет и файлы доступны группе бота, остальным - нет
umask 007

echo "vpn sidecar: ready"
while :; do
	if [ -f "$cfg" ]; then
		echo "vpn sidecar: session started"
		# --script-security 1 идёт после --config и перекрывает его: пользовательские up/down-скрипты
		# из .ovpn не выполнятся. Вывод openvpn по умолчанию не пишем: в нём адрес корпоративного сервера,
		# а логи Docker лежат на диске. timeout - потолок сессии, если бот упал и не погасил туннель
		if [ "${VPN_DEBUG:-0}" = "1" ]; then
			timeout "$max" openvpn --config "$cfg" --script-security 1 || echo "vpn sidecar: openvpn exited with $?"
		else
			timeout "$max" openvpn --config "$cfg" --script-security 1 >/dev/null 2>&1 || echo "vpn sidecar: openvpn exited with $?"
		fi
		rm -f "$cfg" "$dir/mgmt.sock"
		echo "vpn sidecar: session finished"
	fi
	sleep 1
done
