package manifest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// buildSampleBundle 构造一个样例 bundle 目录。
func buildSampleBundle(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"packages/kubeadm.deb":           "fake-kubeadm-pkg",
		"packages/kubelet.deb":           "fake-kubelet-pkg",
		"packages/conntrack.deb":         "fake-deb-content",
		"packages/runtime/crictl.tar.gz": "fake-crictl-fallback",
		"images/core/kube-apiserver.tar": "fake-core-image",
		"images/addons/flannel.tar":      "fake-flannel-image",
		"install/install.sh":             "#!/usr/bin/env bash\nset -eu\n",
		"install/load-images.sh":         "#!/usr/bin/env bash\nset -eu\n",
	}
	for rel, content := range files {
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestGenerateAndVerify(t *testing.T) {
	root := buildSampleBundle(t)

	meta := Meta{OS: "ubuntu", OSVersion: "22.04", Arch: "amd64", K8sVersion: "v1.27.3", Mirror: "official", HostArch: "arm64"}
	m, err := Generate(root, meta)
	if err != nil {
		t.Fatalf("Generate 失败: %v", err)
	}

	if m.SchemaVersion != 1 {
		t.Errorf("schema_version 异常: %d", m.SchemaVersion)
	}
	if len(m.Files) != 6 { // 4 个 packages 文件 + 2 个 scripts
		t.Errorf("期望 6 个 files，实际 %d: %+v", len(m.Files), m.Files)
	}
	if len(m.Images) != 2 {
		t.Errorf("期望 2 个 images，实际 %d", len(m.Images))
	}
	if len(m.Scripts) != 2 {
		t.Errorf("期望 2 个 scripts，实际 %d", len(m.Scripts))
	}
	if m.Files[0].SHA256 == "" {
		t.Error("文件应有 sha256")
	}

	// Verify 应通过
	if err := m.Verify(root); err != nil {
		t.Fatalf("Verify 应通过，实际: %v", err)
	}

	// 写文件再 Load
	mfPath := filepath.Join(root, ManifestFileName)
	if err := m.Write(mfPath); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(mfPath)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Meta.K8sVersion != "v1.27.3" {
		t.Errorf("meta 序列化/反序列化异常: %+v", loaded.Meta)
	}
	if len(loaded.Images) != 2 {
		t.Errorf("重新加载后 images 数量异常: %d", len(loaded.Images))
	}
}

func TestVerifyDetectsTamper(t *testing.T) {
	root := buildSampleBundle(t)
	m, _ := Generate(root, Meta{OS: "ubuntu", K8sVersion: "v1.27.3"})

	// 篡改一个文件内容
	if err := os.WriteFile(filepath.Join(root, "packages/kubeadm.deb"), []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := m.Verify(root); err == nil {
		t.Fatal("篡改后 Verify 应失败")
	}

	// 删除一个文件
	if err := os.Remove(filepath.Join(root, "images/addons/flannel.tar")); err != nil {
		t.Fatal(err)
	}
	if err := m.Verify(root); err == nil {
		t.Fatal("删除文件后 Verify 应失败")
	}
}

func TestVerifyDetectsSizeMismatch(t *testing.T) {
	root := buildSampleBundle(t)
	m, _ := Generate(root, Meta{OS: "ubuntu", K8sVersion: "v1.27.3"})

	// 追加内容改变大小但不影响内容散列对比方式：直接改 manifest 里记录的大小
	m.Files[0].Size += 1
	if err := m.Verify(root); err == nil {
		t.Fatal("大小不匹配时 Verify 应失败")
	}
}

func TestVerifyMissingManifestFile(t *testing.T) {
	root := buildSampleBundle(t)
	m, _ := Generate(root, Meta{OS: "ubuntu", K8sVersion: "v1.27.3"})
	// 将 files 中某路径指向不存在文件
	m.Files[0].Path = "packages/k8s/does-not-exist"
	if err := m.Verify(root); err == nil {
		t.Fatal("文件缺失时 Verify 应失败")
	}
}

func TestGenerateExcludesManifestSelf(t *testing.T) {
	root := buildSampleBundle(t)
	// 预写一个 manifest.yaml
	if err := os.WriteFile(filepath.Join(root, ManifestFileName), []byte("schema_version: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m, _ := Generate(root, Meta{OS: "ubuntu", K8sVersion: "v1.27.3"})
	for _, f := range m.Files {
		if f.Path == ManifestFileName {
			t.Error("manifest 不应包含自身")
		}
	}
}

func TestManifestDeterministic(t *testing.T) {
	root := buildSampleBundle(t)
	m1, _ := Generate(root, Meta{OS: "ubuntu", K8sVersion: "v1.27.3"})
	m2, _ := Generate(root, Meta{OS: "ubuntu", K8sVersion: "v1.27.3"})
	if len(m1.Files) != len(m2.Files) {
		t.Fatal("两次生成 files 数量不一致")
	}
	for i := range m1.Files {
		if m1.Files[i].Path != m2.Files[i].Path {
			t.Fatalf("排序不稳定: %q vs %q", m1.Files[i].Path, m2.Files[i].Path)
		}
	}
	_ = strings.Contains
}
