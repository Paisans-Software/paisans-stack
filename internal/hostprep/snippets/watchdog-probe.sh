if [ -e /dev/watchdog ]; then echo "device present"; else echo "device absent"; fi
for f in /sys/class/watchdog/*/identity; do
  [ -r "$f" ] || continue
  printf 'identity %s %s\n' "$(basename "$(dirname "$f")")" "$(cat "$f")"
done
printf 'systemd %s\n' "$(systemctl show -p RuntimeWatchdogUSec --value 2>/dev/null)"
if pgrep -x watchdog >/dev/null 2>&1; then echo "daemon running"; else echo "daemon absent"; fi
