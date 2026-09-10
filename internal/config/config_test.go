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
