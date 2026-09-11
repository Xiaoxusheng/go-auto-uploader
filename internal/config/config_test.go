package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultAndSaveLoad(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.json")
	st := NewStore(p)
	err := st.LoadFromDisk(CLI{Dirs: "/a,/b", Workers: 5, ScanInterval: 10, Server: "http://x"})
	if err != nil {
		t.Fatal(err)
	}
	c := st.Get()
	if c.Workers != 5 || len(c.Dirs) != 2 || c.Dirs[0] != "/a" {
		t.Fatalf("unexpected cfg %+v", c)
	}

	st.Update(func(x *Config) { x.ConvertMP4 = true })
	if err := st.Save(); err != nil {
		t.Fatal(err)
	}

	st2 := NewStore(p)
	if _, err := Load(p); err != nil {
		t.Fatal(err)
	}
	if err := st2.LoadFromDisk(CLI{}); err != nil {
		t.Fatal(err)
	}
	if !st2.Get().ConvertMP4 {
		t.Fatal("ConvertMP4 should persist")
	}
	if st2.Get().Workers != 5 {
		t.Fatal("workers should persist")
	}
}

func TestGetCopiesDirs(t *testing.T) {
	st := NewStore(filepath.Join(t.TempDir(), "c.json"))
	st.Replace(Config{Dirs: []string{"x"}, Workers: 1})
	got := st.Get()
	got.Dirs[0] = "hacked"
	if st.Get().Dirs[0] != "x" {
		t.Fatal("Get must copy Dirs slice")
	}
}

func TestValidate(t *testing.T) {
	c := Config{Workers: 0, ScanInterval: 0}
	if c.Validate() == nil {
		t.Fatal("should fail")
	}
	c.Workers, c.ScanInterval = 1, 1
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	_ = os.TempDir()
}

// 历史散落配置文件应被合并进 config.json 并改名 .bak，且旧字段语义不丢。
func TestMigrateLegacy(t *testing.T) {
	dir := t.TempDir()
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(old)

	files := map[string]string{
		"builtin_config.json":  `{"quality":"hd","segment_time":5,"watermark_text":"abc"}`,
		"builtin_cookies.json": `{"douyin":"ck1"}`,
		"bilibili_config.json": `{"enable":true,"tid":174}`,
	}
	for name, body := range files {
		if err := os.WriteFile(name, []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
	}

	st := NewStore(filepath.Join(dir, "config.json"))
	if err := st.LoadFromDisk(CLI{Workers: 1, ScanInterval: 1}); err != nil {
		t.Fatal(err)
	}
	migrated, err := st.MigrateLegacy()
	if err != nil {
		t.Fatal(err)
	}
	if len(migrated) != 3 {
		t.Fatalf("migrated=%v", migrated)
	}

	c := st.Get()
	if c.Builtin.Quality != "hd" || c.Builtin.SegmentTime != 5 || c.Builtin.WatermarkText != "abc" {
		t.Fatalf("builtin=%+v", c.Builtin)
	}
	if c.Builtin.Cookies.Douyin != "ck1" {
		t.Fatalf("cookies=%+v", c.Builtin.Cookies)
	}
	if !c.Bilibili.Enable || c.Bilibili.Tid != 174 {
		t.Fatalf("bilibili=%+v", c.Bilibili)
	}
	if c.DataDirPath() != "./data" {
		t.Fatalf("dataDir=%q", c.DataDirPath())
	}

	for name := range files {
		if _, err := os.Stat(name); err == nil {
			t.Fatalf("%s 应已被改名", name)
		}
		if _, err := os.Stat(name + ".bak"); err != nil {
			t.Fatalf("%s.bak 缺失", name)
		}
	}

	// 二次迁移应无操作（幂等）
	if again, _ := st.MigrateLegacy(); len(again) != 0 {
		t.Fatalf("second migrate=%v", again)
	}
}

// 解析失败的历史文件必须保持原样，不能被改名吞掉。
func TestMigrateLegacyKeepsBrokenFile(t *testing.T) {
	dir := t.TempDir()
	old, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(old)

	if err := os.WriteFile("builtin_config.json", []byte("{not json"), 0644); err != nil {
		t.Fatal(err)
	}
	st := NewStore(filepath.Join(dir, "config.json"))
	if _, err := st.MigrateLegacy(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat("builtin_config.json"); err != nil {
		t.Fatal("损坏的历史文件应保持原样以便人工修复")
	}
}

func TestBuiltinDefaults(t *testing.T) {
	b := BuiltinSettings{}
	b.ApplyDefaults()
	if b.Quality != "uhd" || b.CheckInterval != 30 || b.SavePath != "./downloads" ||
		b.WatermarkFontSize != 38 || b.WatermarkPosition != "bottom-right" {
		t.Fatalf("defaults=%+v", b)
	}
}
