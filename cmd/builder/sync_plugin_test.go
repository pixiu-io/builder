package main

import (
	"archive/tar"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPluginGitHubTag(t *testing.T) {
	old := githubTag
	t.Cleanup(func() { githubTag = old })

	githubTag = ""
	if got := pluginGitHubTag("v0.2.0"); got != "plugin-v0.2.0" {
		t.Fatalf("default tag = %q, want plugin-v0.2.0", got)
	}
	if got := pluginGitHubTag(""); got != "plugin-" {
		t.Fatalf("empty version tag = %q, want plugin-", got)
	}

	githubTag = "custom-tag"
	if got := pluginGitHubTag("v0.2.0"); got != "custom-tag" {
		t.Fatalf("override tag = %q, want custom-tag", got)
	}
}

func TestParsePluginVersion(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"v1.0.1\n", "v1.0.1", false},
		{"v1.0.1", "v1.0.1", false},
		{"  v1.0.1  \n", "v1.0.1", false},
		{"plugin version v1.0.1\n", "v1.0.1", false},
		{"unknown\n", "", true},
		{"\n", "", true},
		{"", "", true},
	}
	for _, tc := range cases {
		got, err := parsePluginVersion(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parsePluginVersion(%q) err=nil, want error", tc.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("parsePluginVersion(%q) err=%v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("parsePluginVersion(%q)=%q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestPluginConfigFileContent(t *testing.T) {
	// 固定内容的关键字段核对（防止无意改动打包进去的配置语义）
	for _, want := range []string{
		"push_kubernetes: false",
		"push_images: false",
		"kubernetes:",
		"version: \"\"",
		"callback: 127.0.0.1:8090",
		"repository: test.io",
		"namespace: test",
		"username: test",
		"password: test",
		"name: nginx",
		"id: 12345678",
		"path: docker.io/nginx",
		"- 1.19.1",
	} {
		if !strings.Contains(pluginConfigFileContent, want) {
			t.Errorf("config.yaml 内容缺少 %q", want)
		}
	}
	if !strings.HasPrefix(pluginConfigFileContent, "default:\n") {
		t.Error("config.yaml 应以 default: 开头")
	}
}

func TestPackFilesTarGz(t *testing.T) {
	dir := t.TempDir()
	f1 := filepath.Join(dir, "plugin")
	f2 := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(f1, []byte("#!/bin/sh\necho plugin\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f2, []byte(pluginConfigFileContent), 0o644); err != nil {
		t.Fatal(err)
	}

	tarPath := filepath.Join(dir, "plugin-v0.0.1.tar.gz")
	if err := packFilesTarGz(tarPath, [][2]string{
		{"plugin", f1},
		{"config.yaml", f2},
	}); err != nil {
		t.Fatalf("打包失败: %v", err)
	}

	// 读回校验：顶层名与内容一致
	f, err := os.Open(tarPath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)

	want := map[string]string{
		"plugin":      "#!/bin/sh\necho plugin\n",
		"config.yaml": pluginConfigFileContent,
	}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("读取 tar 条目失败: %v", err)
		}
		content, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		if want[hdr.Name] != string(content) {
			t.Errorf("条目 %s 内容不匹配 (size=%d)", hdr.Name, hdr.Size)
		}
		delete(want, hdr.Name)
	}
	if len(want) != 0 {
		t.Errorf("tar 缺少条目: %v", want)
	}
}
