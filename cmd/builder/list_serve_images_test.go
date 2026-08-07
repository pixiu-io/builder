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

// TestListServeImages 起一个临时 registry，推送镜像后用 listServeImages 查询，
// 验证能列出已加载镜像（host/repo:tag）。
func TestListServeImages(t *testing.T) {
	srv := httptest.NewServer(registry.New())
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")

	// 推送一个镜像到测试 registry
	img := empty.Image
	ref, err := name.ParseReference(host+"/kube-apiserver:v1.35.7", name.WeakValidation, name.Insecure)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(ref, img); err != nil {
		t.Fatalf("push 失败: %v", err)
	}

	// 捕获 stdout
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	err = listServeImages(context.Background(), host)
	w.Close()
	os.Stdout = old
	out, _ := io.ReadAll(r)
	if err != nil {
		t.Fatalf("listServeImages: %v", err)
	}
	if !strings.Contains(string(out), "kube-apiserver:v1.35.7") {
		t.Fatalf("输出缺少镜像: %s", out)
	}
	if !strings.Contains(string(out), "共 1 个镜像") {
		t.Fatalf("计数错误: %s", out)
	}
}
