package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCollectDirFiles(t *testing.T) {
	dir := t.TempDir()
	writeFile := func(rel string) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeFile("a.tar.gz")
	writeFile("sub/b.tar.gz")
	writeFile("sub/deep/c.tar.gz")

	files, err := collectDirFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 3 {
		t.Fatalf("expected 3 files, got %d: %v", len(files), files)
	}
}

func TestFilterSkipped(t *testing.T) {
	files := []string{"a.tar.gz", "b.tar.gz", "sub/c.tar.gz"}

	got := filterSkipped(files, []string{"b.tar.gz"})
	if len(got) != 2 || got[0] != "a.tar.gz" || got[1] != "sub/c.tar.gz" {
		t.Fatalf("got %v", got)
	}
	if len(filterSkipped(files, nil)) != 3 {
		t.Fatal("nil skips should keep all")
	}
	// 按文件名匹配：不同目录下的同名文件都被过滤
	if len(filterSkipped(files, []string{"c.tar.gz"})) != 2 {
		t.Fatalf("basename skip should filter all dirs: %v", filterSkipped(files, []string{"c.tar.gz"}))
	}
}
