package highlight

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// Cut 把高光段从源文件抽出并拼接为一个 mp4。
//
// 两阶段：先逐段 -c copy 抽成临时 ts（不重编码，快且无损），
// 再用 concat demuxer 合并并转封装为 mp4。
// -ss 前置属关键帧对齐，允许 ±2 秒误差，对高光足够。
func Cut(ctx context.Context, ffmpegBin, src, outPath string, segs []Segment) error {
	if ffmpegBin == "" {
		ffmpegBin = "ffmpeg"
	}
	if len(segs) == 0 {
		return fmt.Errorf("没有可裁切的高光段")
	}

	tmpDir, err := os.MkdirTemp("", "highlight-cut-")
	if err != nil {
		return fmt.Errorf("创建临时目录失败: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	parts := make([]string, 0, len(segs))
	for i, s := range segs {
		if s.Duration() <= 0 {
			continue
		}
		p := filepath.Join(tmpDir, fmt.Sprintf("part_%03d.ts", i))
		err := runFFmpeg(ctx, ffmpegBin, []string{
			"-hide_banner", "-nostdin", "-y",
			"-ss", strconv.Itoa(s.Start),
			"-i", src,
			"-t", strconv.Itoa(s.Duration()),
			"-map", "0:v:0?", "-map", "0:a:0?",
			"-c", "copy",
			"-avoid_negative_ts", "make_zero",
			"-f", "mpegts", p,
		})
		if err != nil {
			return fmt.Errorf("抽取第 %d 段(%d-%ds)失败: %w", i+1, s.Start, s.End, err)
		}
		parts = append(parts, p)
	}
	if len(parts) == 0 {
		return fmt.Errorf("没有有效的高光段")
	}

	// concat demuxer 清单：路径统一转正斜杠，避免 Windows 反斜杠被当转义符。
	listPath := filepath.Join(tmpDir, "list.txt")
	var b strings.Builder
	for _, p := range parts {
		line := strings.ReplaceAll(filepath.ToSlash(p), "'", `'\''`)
		b.WriteString("file '" + line + "'\n")
	}
	if err := os.WriteFile(listPath, []byte(b.String()), 0o644); err != nil {
		return fmt.Errorf("写入拼接清单失败: %w", err)
	}

	// 先写 .part 再原子改名，避免上传链路扫到半成品。
	// 必须显式 -f mp4：输出名是 .part，ffmpeg 无法从扩展名推断格式。
	part := outPath + ".part"
	defer os.Remove(part)
	err = runFFmpeg(ctx, ffmpegBin, []string{
		"-hide_banner", "-nostdin", "-y",
		"-f", "concat", "-safe", "0",
		"-i", listPath,
		"-c", "copy",
		"-bsf:a", "aac_adtstoasc",
		"-movflags", "+faststart",
		"-f", "mp4", part,
	})
	if err != nil {
		return fmt.Errorf("拼接高光失败: %w", err)
	}
	if info, serr := os.Stat(part); serr != nil || info.Size() == 0 {
		return fmt.Errorf("拼接输出为空: %v", serr)
	}
	return os.Rename(part, outPath)
}

// runFFmpeg 执行 ffmpeg，失败时把 stderr 尾部带进错误信息（截断，避免刷屏）。
func runFFmpeg(ctx context.Context, bin string, args []string) error {
	cmd := exec.CommandContext(ctx, bin, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%v | %s", err, tail(stderr.String(), 400))
	}
	return nil
}

// tail 取字符串末尾 n 个字符并压掉换行。
func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return strings.ReplaceAll(s, "\n", " ")
	}
	return strings.ReplaceAll(s[len(s)-n:], "\n", " ")
}
