package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPrepareBuildKubeadmUsesLocalCache(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, kubeadmAssetName("v1.31.6", "amd64"))
	if err := os.WriteFile(path, []byte("cached"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, cleanup, err := prepareBuildKubeadm(context.Background(), nil, "v1.31.6", "amd64", dir, false)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if got != path {
		t.Fatalf("path = %q, want %q", got, path)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o755 {
		t.Fatalf("mode = %v", st.Mode().Perm())
	}
}

func TestKubeadmAssetName(t *testing.T) {
	got := kubeadmAssetName("v1.31.7", "amd64")
	want := "kubeadm-v1.31.7-linux-amd64"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestKubeadmDownloadURL(t *testing.T) {
	got := kubeadmDownloadURL("v1.31.6", "amd64")
	want := "https://dl.k8s.io/release/v1.31.6/bin/linux/amd64/kubeadm"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestDownloadFile(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("kubeadm"))
	}))
	t.Cleanup(srv.Close)

	path := filepath.Join(t.TempDir(), "kubeadm")
	if err := downloadFile(context.Background(), srv.URL, path, 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "kubeadm" {
		t.Fatalf("data = %q", data)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o755 {
		t.Fatalf("mode = %v", st.Mode().Perm())
	}
}

func TestDownloadFileHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	err := downloadFile(context.Background(), srv.URL, filepath.Join(t.TempDir(), "kubeadm"), 0o755)
	if err == nil || !strings.Contains(err.Error(), "HTTP 404") {
		t.Fatalf("expected HTTP 404 error, got %v", err)
	}
}

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
