package main

import (
	"os"
	"path/filepath"
	"testing"
)

// spans 必须是空切片而非 nil —— nil 会被 JSON 序列化成 `"spans": null`，
// 让复核页（c.spans.find）与控制台详情直接抛错。§22 记录过 4 片 null 导致复核页整页 500。
func TestRiPrelabelNeverNil(t *testing.T) {
	feats := make([]poseSec, 32) // 全无人（detected=0）
	spans := riPrelabel(feats, 8, 0.2, 0.6, 0.14)
	if spans == nil {
		t.Fatal("riPrelabel 返回 nil：JSON 会写成 null，下游会崩")
	}
	// 4 个连续 none 窗会被合并成 1 段（同类合并），范围覆盖全片。
	if len(spans) != 1 {
		t.Fatalf("全 none 的连续窗应合并为 1 段，得到 %d: %v", len(spans), spans)
	}
	if spans[0]["label"] != "none" || spans[0]["start"] != 0 || spans[0]["end"] != 32 {
		t.Errorf("应为 none 段 0-32，得到 %v", spans[0])
	}
}

// 连续同类窗必须合并成一段。
func TestRiPrelabelMerges(t *testing.T) {
	feats := make([]poseSec, 24)
	for i := range feats {
		feats[i] = poseSec{0.9, 0.05, 0.5, 1.0, 1}
	}
	spans := riPrelabel(feats, 8, 0.2, 0.6, 0.14)
	if len(spans) != 1 {
		t.Fatalf("连续 3 个 dance 窗应合并为 1 段，得到 %d: %v", len(spans), spans)
	}
	if spans[0]["start"] != 0 || spans[0]["end"] != 24 {
		t.Errorf("合并段范围应为 0-24，得到 %v-%v", spans[0]["start"], spans[0]["end"])
	}
}

// 空输入不 panic，且返回空切片（不是 nil）。
func TestRiPrelabelEmpty(t *testing.T) {
	spans := riPrelabel(nil, 8, 0.2, 0.6, 0.14)
	if spans == nil || len(spans) != 0 {
		t.Errorf("空输入应返回空切片，得到 %#v", spans)
	}
}

// 标签切换处必须断开成两段，不能跨类合并。
func TestRiPrelabelSplitsOnLabelChange(t *testing.T) {
	feats := make([]poseSec, 16)
	for i := 0; i < 8; i++ {
		feats[i] = poseSec{0.9, 0.05, 0.5, 1.0, 1} // dance
	}
	for i := 8; i < 16; i++ {
		feats[i] = poseSec{0.9, 0.5, 0.5, 1.0, 1} // closeup
	}
	spans := riPrelabel(feats, 8, 0.2, 0.6, 0.14)
	if len(spans) != 2 {
		t.Fatalf("dance→closeup 应切成 2 段，得到 %d: %v", len(spans), spans)
	}
	if spans[0]["label"] != "dance" || spans[1]["label"] != "closeup" {
		t.Errorf("段标签顺序错误: %v", spans)
	}
}

// skip 标记只对「源文件字节数未变」的片生效：文件被重录（大小变化）时必须重试。
// 背景（2026-09-27）：11 片待入池里 10 片是 3~18 秒碎片，不落标记则每轮重扫重抽。
func TestRiSkipMarkRoundTrip(t *testing.T) {
	root := t.TempDir()
	const stem = "年年_2026-09-25_07-08-54_006"

	if riSkipFresh(root, stem, 153220) {
		t.Fatal("无标记时不应判定为可跳过")
	}

	riWriteSkip(root, stem, "too-few-frames", 3, 153220)
	if !riSkipFresh(root, stem, 153220) {
		t.Fatal("标记存在且 size 一致时应判定为可跳过")
	}
	if riSkipFresh(root, stem, 999999) {
		t.Error("源文件 size 变化（重录）后应重试，不得跳过")
	}
	if riSkipFresh(root, stem, 0) {
		t.Error("size<=0（stat 失败）不得跳过，宁可多跑一次也不漏片")
	}
}

// 标记文件损坏时不得跳过（宁可重跑）。
func TestRiSkipFreshIgnoresCorrupt(t *testing.T) {
	root := t.TempDir()
	const stem = "坏标记片_2026-09-27_00-00-00_000"
	if err := os.MkdirAll(filepath.Join(root, stem), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(riSkipPath(root, stem), []byte("{不是 JSON"), 0o644); err != nil {
		t.Fatal(err)
	}
	if riSkipFresh(root, stem, 12345) {
		t.Error("标记损坏时应重试，不得跳过")
	}
}
