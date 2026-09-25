#!/bin/bash
# 5fps 评估窗抽帧 + 关键点轨迹扫描（#3 时序动力学）
cd /d/upload/_diag/train/_traj || exit 1
FF="/d/upload/ffmpeg-master-latest-win64-gpl-shared/ffmpeg-master-latest-win64-gpl-shared/bin/ffmpeg.exe"
node -e "
const fs=require('fs');
const picks=JSON.parse(fs.readFileSync('windows.json','utf8'));
for(const p of picks){for(const w of p.windows){console.log(p.src+'\t'+w.start+'\t'+w.id)}}
" | while IFS=$'\t' read -r src start id; do
  n=$(ls "frames/$id"/f_*.jpg 2>/dev/null | wc -l)
  if [ "$n" -lt 100 ]; then
    echo "抽帧: $id"
    "$FF" -y -nostdin -loglevel error -ss "$start" -t 32 -i "$src" -vf "fps=5,scale=480:-2" -q:v 5 "frames/$id/f_%04d.jpg"
  fi
  echo "  $id: $(ls frames/$id/f_*.jpg 2>/dev/null | wc -l) 帧"
done
echo "=== 抽帧完成，开始 traj-scan ==="
/d/upload/_diag/train/hleval.exe traj-scan -frames /d/upload/_diag/train/_traj/frames -out /d/upload/_diag/train/_traj/traj.json -dll /d/upload/onnxruntime.dll -model /d/upload/yolov8n-pose.onnx
echo "=== 全部完成 ==="
