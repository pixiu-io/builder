package config

import (
	"os"
	"path/filepath"
	"testing"
)

// sampleFile 在临时目录写入样例 yaml（单文件 builder.yaml），返回文件路径。
func sampleFile(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	content := `
oses:
  - name: ubuntu
    versions: ["20.04", "22.04", "24.04"]
    pkg_manager: apt
    build_images:
      "20.04": ubuntu:20.04
      "22.04": ubuntu:22.04
      "24.04": ubuntu:24.04
    codenames:
      "20.04": focal
      "22.04": jammy
      "24.04": noble
    archs: ["amd64", "arm64"]
  - name: rocky
    versions: ["9"]
    pkg_manager: dnf
    build_images:
      "9": rockylinux:9
    rpm_distro: rhel9
    archs: ["amd64", "arm64"]
versions:
  - version: v1.27.3
    containerd: "1.7.13"
    crictl: "1.27.1"
    runc: "1.1.7"
  - version: v1.28.2
    containerd: "1.7.13"
    crictl: "1.28.0"
    runc: "1.1.7"
addons:
  - name: flannel
    image: "docker.io/flannel/flannel"
    tag: "v0.24.2"
`
	path := filepath.Join(dir, FileName)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("写入样例配置失败: %v", err)
	}
	return path
}

func TestLoad(t *testing.T) {
	path := sampleFile(t)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}

	if len(cfg.OSRegistry.OSes) != 2 {
		t.Errorf("期望 2 个 OS，实际 %d", len(cfg.OSRegistry.OSes))
	}
	if len(cfg.K8sVersions.Versions) != 2 {
		t.Errorf("期望 2 个 k8s 版本，实际 %d", len(cfg.K8sVersions.Versions))
	}
	if len(cfg.Addons.Addons) != 1 {
		t.Errorf("期望 1 个 addon，实际 %d", len(cfg.Addons.Addons))
	}
	if got := cfg.OSRegistry.OSes[0].BuildImages["22.04"]; got != "ubuntu:22.04" {
		t.Errorf("BuildImages[22.04] 解析错误: %q", got)
	}
	if img, err := cfg.OSRegistry.OSes[0].ImageFor("24.04"); err != nil || img != "ubuntu:24.04" {
		t.Errorf("ImageFor(24.04) = %q, err=%v; want ubuntu:24.04", img, err)
	}
}

func TestLoadMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	if _, err := Load(path); err == nil {
		t.Fatal("期望读取缺失配置时报错，实际未报错")
	}
}

func TestFindOS(t *testing.T) {
	cfg, err := Load(sampleFile(t))
	if err != nil {
		t.Fatal(err)
	}
	osDef, ok := cfg.FindOS("ubuntu")
	if !ok {
		t.Fatal("期望找到 ubuntu")
	}
	if osDef.PkgManager != "apt" {
		t.Errorf("ubuntu 包管理器应为 apt，实际 %q", osDef.PkgManager)
	}
	if _, ok := cfg.FindOS("centos"); ok {
		t.Error("不应找到 centos")
	}
}

func TestValidOS(t *testing.T) {
	cfg, _ := Load(sampleFile(t))
	cases := []struct {
		os, ver string
		want    bool
	}{
		{"ubuntu", "22.04", true},
		{"ubuntu", "18.04", false},
		{"rocky", "9", true},
		{"rocky", "8", false},
		{"centos", "9", false},
	}
	for _, c := range cases {
		if got := cfg.ValidOS(c.os, c.ver); got != c.want {
			t.Errorf("ValidOS(%s, %s) = %v, want %v", c.os, c.ver, got, c.want)
		}
	}
}

func TestValidOSArch(t *testing.T) {
	cfg, _ := Load(sampleFile(t))
	if !cfg.ValidOSArch("ubuntu", "amd64") {
		t.Error("期望 ubuntu/amd64 合法")
	}
	if !cfg.ValidOSArch("centos", "arm64") {
		t.Error("未注册 OS 仍应允许 amd64/arm64")
	}
	if cfg.ValidOSArch("ubuntu", "s390x") {
		t.Error("s390x 不应合法")
	}
}

func TestResolveOS(t *testing.T) {
	cfg, _ := Load(sampleFile(t))

	reg, err := cfg.ResolveOS("ubuntu", "22.04")
	if err != nil {
		t.Fatal(err)
	}
	if !reg.FromRegistry || reg.BuildImage != "ubuntu:22.04" || reg.PkgManager != "apt" || reg.Codename != "jammy" {
		t.Errorf("注册表命中异常: %+v", reg)
	}

	// 注册表有 OS、版本未登记 → 约定回退镜像
	ver, err := cfg.ResolveOS("ubuntu", "18.04")
	if err != nil {
		t.Fatal(err)
	}
	if ver.BuildImage != "ubuntu:18.04" || ver.Codename != "bionic" || ver.PkgManager != "apt" {
		t.Errorf("未登记版本回退异常: %+v", ver)
	}

	// 完全未登记 OS
	unk, err := cfg.ResolveOS("centos", "9")
	if err != nil {
		t.Fatal(err)
	}
	if unk.FromRegistry || unk.PkgManager != "dnf" || unk.BuildImage != "centos:9" || unk.RPMDistro != "rhel9" {
		t.Errorf("未登记 OS 推导异常: %+v", unk)
	}
}

func TestFindK8s(t *testing.T) {
	cfg, _ := Load(sampleFile(t))
	ks, ok := cfg.FindK8s("v1.27.3")
	if !ok {
		t.Fatal("期望找到 v1.27.3")
	}
	if ks.Containerd != "1.7.13" {
		t.Errorf("containerd 版本错误: %q", ks.Containerd)
	}
	if _, ok := cfg.FindK8s("v1.26.0"); ok {
		t.Error("不应找到 v1.26.0")
	}
	if !cfg.ValidK8s("v1.28.2") {
		t.Error("v1.28.2 应合法")
	}
}

func TestValidK8s(t *testing.T) {
	cfg, _ := Load(sampleFile(t))
	cases := []struct {
		ver  string
		want bool
	}{
		{"v1.31.0", true},    // 合法格式，未注册也能通过
		{"v1.29.5", true},    // 合法格式，未注册也能通过
		{"v1.27.3", true},    // 合法格式，注册版本
		{"v1.28.2", true},    // 合法格式，注册版本
		{"1.31", false},      // 缺少 v 前缀且缺少第三段
		{"v1.31", false},     // 缺少第三段
		{"v1.31.0.1", false}, // 多余一段
		{"abc", false},       // 非版本
		{"", false},          // 空
	}
	for _, c := range cases {
		if got := cfg.ValidK8s(c.ver); got != c.want {
			t.Errorf("ValidK8s(%q) = %v, want %v", c.ver, got, c.want)
		}
	}
}

func TestCrictlVersionFor(t *testing.T) {
	cfg, _ := Load(sampleFile(t))
	cases := []struct {
		k8sVer string
		want   string
	}{
		{"v1.27.3", "1.27.1"}, // 清单内：返回清单 crictl 值
		{"v1.28.2", "1.28.0"}, // 清单内：返回清单 crictl 值
		{"v1.29.5", "1.29.5"}, // 清单外：推导（对齐 k8s 版本）
		{"v1.30.2", "1.30.2"}, // 清单外：推导
		{"v1.31.0", "1.31.0"}, // 清单外（样例配置无此版本）：推导
	}
	for _, c := range cases {
		if got := cfg.CrictlVersionFor(c.k8sVer); got != c.want {
			t.Errorf("CrictlVersionFor(%q) = %q, want %q", c.k8sVer, got, c.want)
		}
	}
}

func TestSystemDepsForOS(t *testing.T) {
	cfg, _ := Load(sampleFile(t))
	aptDeps := cfg.SystemDepsForOS("ubuntu")
	foundNFSCommon := false
	for _, d := range aptDeps {
		if d == "nfs-common" {
			foundNFSCommon = true
		}
		if d == "nfs-utils" {
			t.Error("apt 系不应包含 nfs-utils")
		}
	}
	if !foundNFSCommon {
		t.Error("apt 系应包含 nfs-common")
	}

	dnfDeps := cfg.SystemDepsForOS("rocky")
	foundNFSUtils := false
	for _, d := range dnfDeps {
		if d == "nfs-utils" {
			foundNFSUtils = true
		}
		if d == "nfs-common" {
			t.Error("dnf 系不应包含 nfs-common")
		}
	}
	if !foundNFSUtils {
		t.Error("dnf 系应包含 nfs-utils")
	}
}

func TestDefaultConfigFile(t *testing.T) {
	t.Setenv("BUILDER_CONFIG_FILE", "")
	if got := DefaultConfigFile(); got != "/etc/pixiu/builder.yaml" {
		t.Errorf("期望默认配置文件 /etc/pixiu/builder.yaml，实际 %q", got)
	}
}

func TestDefaultConfigFileEnv(t *testing.T) {
	t.Setenv("BUILDER_CONFIG_FILE", "/tmp/custom/builder.yaml")
	if got := DefaultConfigFile(); got != "/tmp/custom/builder.yaml" {
		t.Errorf("期望环境变量生效，实际 %q", got)
	}
}

func TestCodenameFor(t *testing.T) {
	cfg, _ := Load(sampleFile(t))
	cases := []struct {
		os, ver, want string
	}{
		{"ubuntu", "20.04", "focal"},
		{"ubuntu", "22.04", "jammy"},
		{"ubuntu", "24.04", "noble"},
		{"ubuntu", "18.04", "bionic"}, // 未在 codenames 声明时按启发式推导
	}
	for _, c := range cases {
		if got := cfg.CodenameFor(c.os, c.ver); got != c.want {
			t.Errorf("CodenameFor(%s, %s) = %q, want %q", c.os, c.ver, got, c.want)
		}
	}
}

func TestCodenameScalarFallback(t *testing.T) {
	o := &OS{Name: "debian", Codename: "bookworm"}
	if got := o.CodenameFor("12"); got != "bookworm" {
		t.Errorf("标量 codename 回退异常: %q", got)
	}
}

func TestImageFor(t *testing.T) {
	cfg, _ := Load(sampleFile(t))
	osDef, ok := cfg.FindOS("ubuntu")
	if !ok {
		t.Fatal("期望找到 ubuntu")
	}
	cases := []struct {
		ver, want string
	}{
		{"20.04", "ubuntu:20.04"},
		{"22.04", "ubuntu:22.04"},
		{"24.04", "ubuntu:24.04"},
		{"18.04", "ubuntu:18.04"}, // 未在 build_images 声明时按约定回退
	}
	for _, c := range cases {
		got, err := osDef.ImageFor(c.ver)
		if err != nil || got != c.want {
			t.Errorf("ImageFor(%s) = %q, err=%v; want %q", c.ver, got, err, c.want)
		}
	}
}

func TestRPMDistroFor(t *testing.T) {
	cfg, _ := Load(sampleFile(t))
	if got := cfg.RPMDistroFor("rocky"); got != "rhel9" {
		t.Errorf("rocky rpm_distro = %q, want rhel9", got)
	}
	if got := cfg.RPMDistroFor("ubuntu"); got != "" {
		t.Errorf("apt 系不应有 rpm_distro，实际 %q", got)
	}
}

func TestK8sMinor(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"v1.27.3", "v1.27", false},
		{"v1.28.2", "v1.28", false},
		{"1.27.3", "v1.27", false},
		{"v1.27", "", true},
		{"v2", "", true},
		{"", "", true},
	}
	for _, c := range cases {
		got, err := K8sMinor(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("K8sMinor(%q) 期望错误，实际 %q", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("K8sMinor(%q) 意外错误: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("K8sMinor(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
