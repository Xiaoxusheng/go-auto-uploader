#!/bin/bash
OLD_PID=$(pgrep -f '/home/upload/uploader' | head -1)
[ -n "$OLD_PID" ] && kill "$OLD_PID"
for i in $(seq 1 15); do
  pgrep -f '/home/upload/uploader' >/dev/null || break
  sleep 1
done
pgrep -f '/home/upload/uploader' >/dev/null && kill -9 $(pgrep -f '/home/upload/uploader') 2>/dev/null
sleep 1
cd /home/upload
mv -f /home/upload/uploader.new /home/upload/uploader
chmod 755 /home/upload/uploader
mapfile -t ARGS < /home/upload/.restart_args.txt
nohup "${ARGS[@]}" >> /home/upload/uploader_console.log 2>&1 &
sleep 4
echo "PID: $(pgrep -f '/home/upload/uploader')"
ss -tlnp | grep 8888 | head -1
tail -5 /home/upload/uploader_console.log
