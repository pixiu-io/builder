package config

import (
	"os"
	"path/filepath"
	"testing"
)

// sampleContent 基础样例 yaml（不含 build 节）。
const sampleContent = `
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
addon_images:
  - name: flannel
    image: "docker.io/flannel/flannel"
    tag: "v0.24.2"
`

// writeSample 将 content 写入临时目录 builder.yaml，返回文件路径。
func writeSample(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("写入样例配置失败: %v", err)
	}
	return path
}

// sampleFile 在临时目录写入样例 yaml（单文件 builder.yaml），返回文件路径。
func sampleFile(t *testing.T) string {
	t.Helper()
	return writeSample(t, sampleContent)
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
	if len(cfg.AddonImages.Addons) != 1 {
		t.Errorf("期望 1 个 addon，实际 %d", len(cfg.AddonImages.Addons))
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

	// centos 7 → 推导为 yum（CentOS 7 无 dnf），RPMDistro 仍为 rhel7
	cent7, err := cfg.ResolveOS("centos", "7")
	if err != nil {
		t.Fatal(err)
	}
	if cent7.FromRegistry || cent7.PkgManager != "yum" || cent7.BuildImage != "centos:7" || cent7.RPMDistro != "rhel7" {
		t.Errorf("centos 7 推导异常: %+v", cent7)
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

func TestInferPkgManager(t *testing.T) {
	cases := []struct {
		os, ver, want string
	}{
		{"centos", "7", "yum"},   // CentOS 7 使用 yum（无 dnf）
		{"centos", "7.9", "yum"}, // 版本带小版本同样归 yum
		{"centos", "8", "dnf"},   // CentOS 8+ 使用 dnf
		{"centos", "9", "dnf"},
		{"rhel", "7", "yum"},
		{"almalinux", "7", "yum"},
		{"rocky", "9", "dnf"}, // rocky 始终 dnf（不受版本影响）
		{"fedora", "39", "dnf"},
		{"openeuler", "22.03", "dnf"},
		{"amazonlinux", "2", "dnf"},
		{"ubuntu", "22.04", "apt"},
		{"debian", "12", "apt"},
		{"unknown", "1", "apt"}, // 未知发行版默认 apt
	}
	for _, c := range cases {
		if got := InferPkgManager(c.os, c.ver); got != c.want {
			t.Errorf("InferPkgManager(%q, %q) = %q, want %q", c.os, c.ver, got, c.want)
		}
	}
}

func TestSystemDepsForOS(t *testing.T) {
	cfg, _ := Load(sampleFile(t))
	aptDeps := cfg.SystemDepsForOS("ubuntu", "22.04")
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

	dnfDeps := cfg.SystemDepsForOS("rocky", "9")
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

	// centos 7 → yum → nfs-utils；centos 9 → dnf → nfs-utils（含通用依赖验证）
	yumDeps := cfg.SystemDepsForOS("centos", "7")
	yumHasNFSUtils, yumHasNFSCommon := false, false
	for _, d := range yumDeps {
		if d == "nfs-utils" {
			yumHasNFSUtils = true
		}
		if d == "nfs-common" {
			yumHasNFSCommon = true
		}
	}
	if !yumHasNFSUtils || yumHasNFSCommon {
		t.Errorf("centos 7（yum）依赖应为 nfs-utils 而非 nfs-common: %v", yumDeps)
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

func TestOSContainerdFieldsParse(t *testing.T) {
	// OS 新增 containerd_pkg / containerd_repo 字段应能被 yaml 解析（openEuler 系统源场景）。
	content := `
oses:
  - name: openEuler
    versions: ["22.03"]
    pkg_manager: dnf
    build_images:
      "22.03": openeuler/openeuler:22.03-lts-sp3
    rpm_distro: rhel7
    containerd_pkg: "containerd"
    containerd_repo: "none"
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
`
	cfg, err := Load(writeSample(t, content))
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	osDef, ok := cfg.FindOS("openEuler")
	if !ok {
		t.Fatal("期望找到 openEuler")
	}
	if osDef.ContainerdPkg != "containerd" {
		t.Errorf("openEuler containerd_pkg = %q, want containerd", osDef.ContainerdPkg)
	}
	if osDef.ContainerdRepo != "none" {
		t.Errorf("openEuler containerd_repo = %q, want none", osDef.ContainerdRepo)
	}
	// 未配置字段的 OS 保持零值
	rocky, ok := cfg.FindOS("rocky")
	if !ok {
		t.Fatal("期望找到 rocky")
	}
	if rocky.ContainerdPkg != "" || rocky.ContainerdRepo != "" {
		t.Errorf("未配置的 rocky containerd 字段应为零值，实际 %q/%q", rocky.ContainerdPkg, rocky.ContainerdRepo)
	}
}

func TestResolveOSContainerdDefaults(t *testing.T) {
	// ResolveOS 透传注册表的 containerd_pkg/containerd_repo；未配置时默认 containerd.io + docker。
	cfg, _ := Load(sampleFile(t))

	reg, err := cfg.ResolveOS("ubuntu", "22.04")
	if err != nil {
		t.Fatal(err)
	}
	if reg.ContainerdPkg != "containerd.io" || reg.ContainerdRepo != "docker" {
		t.Errorf("未配置 OS 默认值异常: %q/%q", reg.ContainerdPkg, reg.ContainerdRepo)
	}
}

func TestResolveOSContainerdFromRegistry(t *testing.T) {
	// 注册表配置 openEuler 系统源后，ResolvedOS 应透传 containerd_pkg=containerd、containerd_repo=none。
	content := `
oses:
  - name: openEuler
    versions: ["22.03"]
    pkg_manager: dnf
    build_images:
      "22.03": openeuler/openeuler:22.03-lts-sp3
    rpm_distro: rhel7
    containerd_pkg: "containerd"
    containerd_repo: "none"
    archs: ["amd64", "arm64"]
versions:
  - version: v1.27.3
`
	cfg, err := Load(writeSample(t, content))
	if err != nil {
		t.Fatal(err)
	}
	reg, err := cfg.ResolveOS("openEuler", "22.03")
	if err != nil {
		t.Fatal(err)
	}
	if reg.ContainerdPkg != "containerd" {
		t.Errorf("openEuler ContainerdPkg = %q, want containerd", reg.ContainerdPkg)
	}
	if reg.ContainerdRepo != "none" {
		t.Errorf("openEuler ContainerdRepo = %q, want none", reg.ContainerdRepo)
	}
	if !reg.FromRegistry {
		t.Error("openEuler 应在注册表中")
	}
}

func TestInferContainerd(t *testing.T) {
	// 未显式配置时按发行版推断：openEuler → containerd + none；其他 → containerd.io + docker。
	cases := []struct {
		os, wantPkg, wantRepo string
	}{
		{"openEuler", "containerd", "none"},
		{"openeuler", "containerd", "none"}, // 大小写不敏感
		{"OpenEuler", "containerd", "none"},
		{"rocky", "containerd.io", "docker"},
		{"ubuntu", "containerd.io", "docker"},
		{"centos", "containerd.io", "docker"}, // 未登记 OS 同样走推断
	}
	for _, c := range cases {
		if got := InferContainerdPkg(c.os); got != c.wantPkg {
			t.Errorf("InferContainerdPkg(%q) = %q, want %q", c.os, got, c.wantPkg)
		}
		if got := InferContainerdRepo(c.os); got != c.wantRepo {
			t.Errorf("InferContainerdRepo(%q) = %q, want %q", c.os, got, c.wantRepo)
		}
	}
}

func TestResolveOSContainerdInferRegisteredOpenEuler(t *testing.T) {
	// 注册表 openEuler 条目未配置 containerd_pkg/containerd_repo 时，ResolveOS 应按发行版
	// 推断为 containerd（系统源包名）+ none（不配置 docker 源），修复旧版 builder.yaml 兼容。
	content := `
oses:
  - name: openEuler
    versions: ["22.03"]
    pkg_manager: dnf
    build_images:
      "22.03": openeuler/openeuler:22.03-lts-sp3
    rpm_distro: rhel7
    archs: ["amd64", "arm64"]
versions:
  - version: v1.35.7
`
	cfg, err := Load(writeSample(t, content))
	if err != nil {
		t.Fatal(err)
	}
	reg, err := cfg.ResolveOS("openEuler", "22.03")
	if err != nil {
		t.Fatal(err)
	}
	if reg.ContainerdPkg != "containerd" {
		t.Errorf("openEuler 未配置 ContainerdPkg = %q, want containerd（推断）", reg.ContainerdPkg)
	}
	if reg.ContainerdRepo != "none" {
		t.Errorf("openEuler 未配置 ContainerdRepo = %q, want none（推断）", reg.ContainerdRepo)
	}
	if !reg.FromRegistry {
		t.Error("openEuler 应在注册表中")
	}
}

func TestResolveOSContainerdInferRegisteredDefault(t *testing.T) {
	// 注册表 rocky 条目未配置 containerd 字段时，ResolveOS 应推断为 containerd.io + docker（默认 docker 源）。
	cfg, _ := Load(sampleFile(t)) // sampleContent 的 rocky 未配置 containerd 字段
	reg, err := cfg.ResolveOS("rocky", "9")
	if err != nil {
		t.Fatal(err)
	}
	if reg.ContainerdPkg != "containerd.io" {
		t.Errorf("rocky 未配置 ContainerdPkg = %q, want containerd.io", reg.ContainerdPkg)
	}
	if reg.ContainerdRepo != "docker" {
		t.Errorf("rocky 未配置 ContainerdRepo = %q, want docker", reg.ContainerdRepo)
	}
}

func TestResolveOSContainerdInferUnregistered(t *testing.T) {
	// 未登记 OS 分支：openEuler 也按发行版推断 containerd + none；其余推断 containerd.io + docker。
	cfg, _ := Load(sampleFile(t))

	oe, err := cfg.ResolveOS("openEuler", "22.03") // sampleContent 未登记 openEuler
	if err != nil {
		t.Fatal(err)
	}
	if oe.ContainerdPkg != "containerd" || oe.ContainerdRepo != "none" {
		t.Errorf("未登记 openEuler ContainerdPkg/Repo = %q/%q, want containerd/none", oe.ContainerdPkg, oe.ContainerdRepo)
	}
	if oe.FromRegistry {
		t.Error("openEuler 不应命中注册表")
	}

	centos, err := cfg.ResolveOS("centos", "9") // sampleContent 未登记 centos
	if err != nil {
		t.Fatal(err)
	}
	if centos.ContainerdPkg != "containerd.io" || centos.ContainerdRepo != "docker" {
		t.Errorf("未登记 centos ContainerdPkg/Repo = %q/%q, want containerd.io/docker", centos.ContainerdPkg, centos.ContainerdRepo)
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

func TestBuildOptionsParse(t *testing.T) {
	content := sampleContent + `
build:
  os: ubuntu
  os_version: "22.04"
  kubernetes_version: v1.27.3
  arch: arm64
  mirror: official
  workdir: ./w
  out: ./o
  mode: packages
  skip_addons: true
  only_addons: true
  dry_run: true
`
	cfg, err := Load(writeSample(t, content))
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}

	want := BuildOptions{
		OS:                "ubuntu",
		OSVersion:         "22.04",
		KubernetesVersion: "v1.27.3",
		Arch:              "arm64",
		Mirror:            "official",
		WorkDir:           "./w",
		OutDir:            "./o",
		Mode:              "packages",
		SkipAddons:        true,
		OnlyAddons:        true,
		DryRun:            true,
	}
	if cfg.Build != want {
		t.Errorf("BuildOptions 解析异常:\n got %+v\nwant %+v", cfg.Build, want)
	}
}

func TestBuildOptionsZeroValue(t *testing.T) {
	// 未配置 build 节时保持零值，不影响其余配置解析。
	cfg, err := Load(sampleFile(t))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Build != (BuildOptions{}) {
		t.Errorf("未配置 build 节时应为零值，实际 %+v", cfg.Build)
	}
}

// TestS3OptionsCredentialsParse 验证 s3 节 access_key/secret_key/session_token 能被 yaml 解析，
// 且未配置时保持零值（走默认 AWS 凭证链）。
func TestS3OptionsCredentialsParse(t *testing.T) {
	content := `
oses:
  - name: ubuntu
    versions: ["22.04"]
    pkg_manager: apt
    build_images:
      "22.04": ubuntu:22.04
    archs: ["amd64"]
versions:
  - version: v1.27.3
s3:
  bucket: pixiu
  region: us-east-1
  endpoint: "http://127.0.0.1:9000"
  prefix: releases/
  force_path_style: true
  access_key: AKIDEXAMPLE
  secret_key: SECRETKEYEXAMPLE
  session_token: SESSIONTOKENEXAMPLE
`
	cfg, err := Load(writeSample(t, content))
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	want := S3Config{
		Bucket:         "pixiu",
		Region:         "us-east-1",
		Endpoint:       "http://127.0.0.1:9000",
		Prefix:         "releases/",
		ForcePathStyle: true,
		AccessKey:      "AKIDEXAMPLE",
		SecretKey:      "SECRETKEYEXAMPLE",
		SessionToken:   "SESSIONTOKENEXAMPLE",
	}
	if cfg.S3 != want {
		t.Errorf("S3Config 解析异常:\n got %+v\nwant %+v", cfg.S3, want)
	}
}

// TestCosOptionsParse 验证 cos 节能被 yaml 解析。
func TestCosOptionsParse(t *testing.T) {
	content := `
oses:
  - name: ubuntu
    versions: ["22.04"]
    pkg_manager: apt
    build_images:
      "22.04": ubuntu:22.04
    archs: ["amd64"]
versions:
  - version: v1.27.3
cos:
  bucket: mybucket-1250000000
  region: ap-guangzhou
  secret_id: AKIDEXAMPLE
  secret_key: SECRETKEYEXAMPLE
  prefix: releases/
`
	cfg, err := Load(writeSample(t, content))
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	want := CosConfig{
		Bucket:    "mybucket-1250000000",
		Region:    "ap-guangzhou",
		SecretID:  "AKIDEXAMPLE",
		SecretKey: "SECRETKEYEXAMPLE",
		Prefix:    "releases/",
	}
	if cfg.Cos != want {
		t.Errorf("CosConfig 解析异常:\n got %+v\nwant %+v", cfg.Cos, want)
	}
}

// TestBuildKeepFilesParse 验证 build 节 keep_files 能被 yaml 解析（默认 false，配置 true 生效）。
func TestBuildKeepFilesParse(t *testing.T) {
	content := `
oses:
  - name: ubuntu
    versions: ["22.04"]
    pkg_manager: apt
    build_images:
      "22.04": ubuntu:22.04
    archs: ["amd64"]
versions:
  - version: v1.27.3
build:
  keep_files: true
`
	cfg, err := Load(writeSample(t, content))
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	if !cfg.Build.KeepFiles {
		t.Error("keep_files 应为 true")
	}
	// 未配置时默认 false
	cfg2, err := Load(sampleFile(t))
	if err != nil {
		t.Fatal(err)
	}
	if cfg2.Build.KeepFiles {
		t.Error("未配置 keep_files 应为 false")
	}
}

func TestS3OptionsCredentialsZeroValue(t *testing.T) {
	// 未配置 s3 凭证字段时保持零值，保证留空走默认 AWS 凭证链行为不变。
	cfg, err := Load(sampleFile(t)) // sampleContent 无 s3 节
	if err != nil {
		t.Fatal(err)
	}
	if cfg.S3.AccessKey != "" || cfg.S3.SecretKey != "" || cfg.S3.SessionToken != "" {
		t.Errorf("未配置 s3 凭证应保持零值，实际 %+v", cfg.S3)
	}
}

// TestAddonImagesNoPackagesField 验证 addon_images 节不再解析 per-addon packages（该字段已移除，
// 附加安装包统一由顶层 addon_packages 提供）。
func TestAddonImagesNoPackagesField(t *testing.T) {
	content := `
oses:
  - name: ubuntu
    versions: ["22.04"]
    pkg_manager: apt
    build_images:
      "22.04": ubuntu:22.04
    archs: ["amd64"]
versions:
  - version: v1.27.3
    crictl: "1.27.1"
addon_images:
  - name: flannel
    image: "docker.io/flannel/flannel"
    tag: "v0.24.2"
`
	cfg, err := Load(writeSample(t, content))
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	flannel, ok := cfg.FindAddon("flannel")
	if !ok {
		t.Fatal("期望找到 flannel")
	}
	if flannel.Image != "docker.io/flannel/flannel" || flannel.Tag != "v0.24.2" {
		t.Errorf("flannel 解析异常: %+v", flannel)
	}
	if len(cfg.AddonImages.Addons) != 1 {
		t.Errorf("addon_images 应解析 1 个 addon，实际 %d", len(cfg.AddonImages.Addons))
	}
	if len(cfg.AddonPackages) != 0 {
		t.Errorf("未配置 addon_packages 应为空，实际 %v", cfg.AddonPackages)
	}
}

// TestAddonPackagesTopLevel 验证顶层 addon_packages 节（对象列表：name + 可选 version）能被 yaml 解析。
func TestAddonPackagesTopLevel(t *testing.T) {
	content := `
oses:
  - name: ubuntu
    versions: ["22.04"]
    pkg_manager: apt
    build_images:
      "22.04": ubuntu:22.04
    archs: ["amd64"]
versions:
  - version: v1.27.3
addon_images:
  - name: flannel
    image: "docker.io/flannel/flannel"
    tag: "v0.24.2"
addon_packages:
  - name: conntrack
  - name: ipset
    version: "9.0"
`
	cfg, err := Load(writeSample(t, content))
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	want := []AddonPackage{
		{Name: "conntrack"},             // version 省略 → 空（不锁版本）
		{Name: "ipset", Version: "9.0"}, // 带版本锁定
	}
	if len(cfg.AddonPackages) != len(want) {
		t.Fatalf("addon_packages = %v, want %v", cfg.AddonPackages, want)
	}
	for i := range want {
		if cfg.AddonPackages[i] != want[i] {
			t.Errorf("addon_packages[%d] = %+v, want %+v", i, cfg.AddonPackages[i], want[i])
		}
	}
}

// TestAddonPackagesExplicitEmptyVersion 验证 version 显式为空（""）时解析为空字符串（不锁版本）。
func TestAddonPackagesExplicitEmptyVersion(t *testing.T) {
	content := `
oses:
  - name: ubuntu
    versions: ["22.04"]
    pkg_manager: apt
    build_images:
      "22.04": ubuntu:22.04
    archs: ["amd64"]
versions:
  - version: v1.27.3
addon_packages:
  - name: conntrack
    version: ""
  - name: vim
    version: "9.0"
`
	cfg, err := Load(writeSample(t, content))
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	if len(cfg.AddonPackages) != 2 {
		t.Fatalf("addon_packages 应解析 2 项，实际 %v", cfg.AddonPackages)
	}
	if cfg.AddonPackages[0] != (AddonPackage{Name: "conntrack"}) {
		t.Errorf("显式空版本应解析为空字符串: %+v", cfg.AddonPackages[0])
	}
	if cfg.AddonPackages[1] != (AddonPackage{Name: "vim", Version: "9.0"}) {
		t.Errorf("带版本解析异常: %+v", cfg.AddonPackages[1])
	}
}
