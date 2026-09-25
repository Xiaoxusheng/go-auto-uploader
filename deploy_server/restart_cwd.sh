#!/bin/bash
PID=$(pgrep -f '/home/upload/uploader' | head -1)
[ -n "$PID" ] && kill "$PID"
for i in $(seq 1 15); do
  pgrep -f '/home/upload/uploader' >/dev/null || break
  sleep 1
done
cd /home/upload
mapfile -t ARGS < /home/upload/.restart_args.txt
nohup "${ARGS[@]}" >> /home/upload/uploader_console.log 2>&1 &
sleep 4
NEWPID=$(pgrep -f '/home/upload/uploader' | head -1)
echo "NEW PID: $NEWPID"
echo "CWD: $(readlink /proc/$NEWPID/cwd)"
tail -4 /home/upload/uploader_console.log
