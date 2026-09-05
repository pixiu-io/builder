package main

import (
	"context"
	"io"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

func TestRunImages(t *testing.T) {
	srv := httptest.NewServer(registry.New())
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")

	img := empty.Image
	ref, err := name.ParseReference(host+"/kube-apiserver:v1.35.7", name.WeakValidation, name.Insecure)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(ref, img); err != nil {
		t.Fatalf("push 失败: %v", err)
	}

	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	err = runImages(context.Background(), host, 50)
	w.Close()
	os.Stdout = old
	out, _ := io.ReadAll(r)
	if err != nil {
		t.Fatalf("runImages: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, "REPOSITORY") || !strings.Contains(s, "TAG") || !strings.Contains(s, "IMAGE ID") {
		t.Fatalf("缺少表头: %s", s)
	}
	if !strings.Contains(s, "kube-apiserver") || !strings.Contains(s, "v1.35.7") {
		t.Fatalf("输出缺少镜像: %s", s)
	}
}

func TestRunImagesLimit(t *testing.T) {
	srv := httptest.NewServer(registry.New())
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")

	for _, tag := range []string{"v1", "v2", "v3"} {
		ref, err := name.ParseReference(host+"/demo:"+tag, name.WeakValidation, name.Insecure)
		if err != nil {
			t.Fatal(err)
		}
		if err := remote.Write(ref, empty.Image); err != nil {
			t.Fatal(err)
		}
	}

	rows, err := collectRegistryImages(context.Background(), host, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("limit=2 应返回 2 条，got %d", len(rows))
	}
}

func TestRunImagesEmpty(t *testing.T) {
	srv := httptest.NewServer(registry.New())
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")

	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	err := runImages(context.Background(), host, 50)
	w.Close()
	os.Stdout = old
	out, _ := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "REPOSITORY") {
		t.Fatalf("空 registry 也应打印表头: %s", out)
	}
}

func TestRunLsTags(t *testing.T) {
	srv := httptest.NewServer(registry.New())
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")

	for _, tag := range []string{"3.9", "3.10"} {
		ref, err := name.ParseReference(host+"/pixiu/pause:"+tag, name.WeakValidation, name.Insecure)
		if err != nil {
			t.Fatal(err)
		}
		if err := remote.Write(ref, empty.Image); err != nil {
			t.Fatal(err)
		}
	}

	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	err := runLsTags(context.Background(), host, "pixiu/pause")
	w.Close()
	os.Stdout = old
	out, _ := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if !strings.Contains(s, "3.9") || !strings.Contains(s, "3.10") {
		t.Fatalf("缺少 tags: %s", s)
	}
	if !strings.Contains(s, "pixiu/pause") {
		t.Fatalf("缺少 repository: %s", s)
	}
}

func TestRunLsTagsStripsHost(t *testing.T) {
	srv := httptest.NewServer(registry.New())
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")

	ref, err := name.ParseReference(host+"/pause:latest", name.WeakValidation, name.Insecure)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(ref, empty.Image); err != nil {
		t.Fatal(err)
	}

	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	err = runLsTags(context.Background(), host, host+"/pause")
	w.Close()
	os.Stdout = old
	out, _ := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "latest") {
		t.Fatalf("got %s", out)
	}
}
