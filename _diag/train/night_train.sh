#!/bin/bash
# 今夜连续训练守护：等当前 review-ingest 结束后，循环「新片入池 → 金标重定标」直到明早 07:00。
# review-ingest 幂等：无新片时数秒退出，有新片时全量消化；单实例串行，不会并发写 config。
LOG=/d/upload/_diag/train/nightly_train_20260925.log
FF="D:/upload/ffmpeg-master-latest-win64-gpl-shared/ffmpeg-master-latest-win64-gpl-shared/bin/ffmpeg.exe"
END=$(date -d "tomorrow 07:00" +%s)

echo "=== $(date '+%F %T') 夜训守护启动，截止 $(date -d @$END '+%F %T')" >> "$LOG"

# 等当前已在跑的 hleval（review-ingest 57+ 片那批）结束，避免双实例
while tasklist 2>/dev/null | grep -qi "hleval.exe"; do sleep 300; done
echo "=== $(date '+%F %T') 存量入池进程已结束，进入循环" >> "$LOG"

while [ "$(date +%s)" -lt "$END" ]; do
  echo "--- $(date '+%F %T') 轮次：入池" >> "$LOG"
  cd /d/upload/_diag/train || exit 1
  ./hleval.exe review-ingest \
    -downloads D:/upload/downloads \
    -frames D:/upload/_diag/train/_pose_pilot/frames \
    -config D:/upload/_diag/train/_pose_pilot/clips_config.json \
    -pose-out D:/upload/_diag/train/pose_features_go.json \
    -dll D:/upload/onnxruntime.dll \
    -model D:/upload/yolov8n-pose.onnx \
    -ffmpeg "$FF" >> "$LOG" 2>&1
  tail -1 "$LOG" >> "$LOG.round" 2>/dev/null
  echo "--- $(date '+%F %T') 轮次：重定标" >> "$LOG"
  node /d/upload/_diag/train/autogold_sweep.js >> "$LOG" 2>&1
  sleep 1800
done
echo "=== $(date '+%F %T') 夜训守护结束（到点）" >> "$LOG"
