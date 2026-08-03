package builder

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"builder/internal/config"
	"builder/internal/manifest"
	"builder/internal/mirror"
)

// loadSampleConfig 构造一份样例配置（单文件 builder.yaml）。
func loadSampleConfig(t *testing.T) *config.Config {
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
      "22.04": jammy
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
	if err := os.WriteFile(filepath.Join(dir, config.FileName), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(filepath.Join(dir, config.FileName))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestBundleName(t *testing.T) {
	if got := BundleName("ubuntu", "22.04", "amd64", "v1.27.3"); got != "pixiu-offline-ubuntu-22.04-amd64-v1.27.3" {
		t.Errorf("BundleName = %q", got)
	}
	if got := ImagesBundleName("amd64", "v1.27.3"); got != "pixiu-offline-images-amd64-v1.27.3" {
		t.Errorf("ImagesBundleName = %q", got)
	}
	if got := PackagesBundleName("ubuntu", "22.04", "amd64", "v1.27.3"); got != "pixiu-offline-ubuntu-22.04-amd64-v1.27.3-packages" {
		t.Errorf("PackagesBundleName = %q", got)
	}
	if got := ImagesOfflineBundleName("ubuntu", "22.04", "amd64", "v1.27.3"); got != "pixiu-offline-ubuntu-22.04-amd64-v1.27.3-images" {
		t.Errorf("ImagesOfflineBundleName = %q", got)
	}
}

func TestAptFamily(t *testing.T) {
	if got := aptFamily("ubuntu"); got != "ubuntu" {
		t.Errorf("ubuntu 家族 = %q", got)
	}
	if got := aptFamily("debian"); got != "debian" {
		t.Errorf("debian 家族 = %q", got)
	}
}

func TestValidateOptions(t *testing.T) {
	cfg := loadSampleConfig(t)
	base := Options{
		Config: cfg, OS: "ubuntu", OSVersion: "22.04", Arch: "amd64",
		K8sVersion: "v1.27.3", Mirror: mirror.Official,
	}

	cases := []struct {
		name    string
		mutate  func(*Options)
		wantErr string
	}{
		{"合法", func(o *Options) {}, ""},
		{"任意未注册 k8s 版本", func(o *Options) { o.K8sVersion = "v1.30.2" }, ""},
		{"任意未注册 OS", func(o *Options) { o.OS = "centos" }, ""},
		{"任意未注册 OS 版本", func(o *Options) { o.OSVersion = "18.04" }, ""},
		{"非法 arch", func(o *Options) { o.Arch = "s390x" }, "不支持的架构"},
		{"非法 k8s 版本格式", func(o *Options) { o.K8sVersion = "v1.31" }, "非法的 k8s 版本格式"},
		{"未实现镜像源", func(o *Options) { o.Mirror = mirror.Aliyun }, "尚未完整实现"},
		{"非法构建模式", func(o *Options) { o.Mode = "bogus" }, "非法的构建模式"},
		{"仅软件包模式", func(o *Options) { o.Mode = "packages" }, ""},
		{"仅镜像模式", func(o *Options) { o.Mode = "images" }, ""},
		{"packages 缺 OS", func(o *Options) { o.Mode = "packages"; o.OS = ""; o.OSVersion = "" }, "缺少 OS/版本"},
		{"images 缺 OS（Build 会补默认；validate 单独调用时也允许空）", func(o *Options) { o.Mode = "images"; o.OS = ""; o.OSVersion = "" }, ""},
	}
	for _, c := range cases {
		o := base
		c.mutate(&o)
		err := validateOptions(o)
		if c.wantErr == "" {
			if err != nil {
				t.Errorf("%s: 期望通过，实际 %v", c.name, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), c.wantErr) {
			t.Errorf("%s: 期望错误含 %q，实际 %v", c.name, c.wantErr, err)
		}
	}
}

func TestBuildDryRun(t *testing.T) {
	cfg := loadSampleConfig(t)
	workDir := filepath.Join(t.TempDir(), "work")
	outDir := filepath.Join(t.TempDir(), "dist")

	res, err := Build(context.Background(), Options{
		Config:     cfg,
		OS:         "ubuntu",
		OSVersion:  "22.04",
		Arch:       "amd64",
		K8sVersion: "v1.27.3",
		Mirror:     mirror.Official,
		WorkDir:    workDir,
		OutDir:     outDir,
		DryRun:     true,
	})
	if err != nil {
		t.Fatalf("Build dry-run 失败: %v", err)
	}

	// 验证目录结构
	for _, rel := range []string{
		"packages", "packages/runtime",
		"images/core", "images/addons", "install",
		"manifest.yaml",
	} {
		if _, err := os.Stat(filepath.Join(res.BundleDir, rel)); err != nil {
			t.Errorf("bundle 缺少 %s: %v", rel, err)
		}
	}
	// 脚本应渲染
	for _, s := range []string{"install/install.sh", "install/load-images.sh"} {
		if _, err := os.Stat(filepath.Join(res.BundleDir, s)); err != nil {
			t.Errorf("脚本缺失 %s: %v", s, err)
		}
	}
	// tar.gz 应打包
	if _, err := os.Stat(res.TarPath); err != nil {
		t.Errorf("tar.gz 未生成: %v", err)
	}
	if len(res.TarPaths) != 2 {
		t.Fatalf("--mode all（默认）应产出 2 个独立 tar，实际 %d: %v", len(res.TarPaths), res.TarPaths)
	}
	for _, p := range res.TarPaths {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("独立 tar 缺失: %s (%v)", p, err)
		}
	}
	wantPkg := filepath.Join(outDir, "pixiu-offline-ubuntu-22.04-amd64-v1.27.3-packages.tar.gz")
	wantImg := filepath.Join(outDir, "pixiu-offline-ubuntu-22.04-amd64-v1.27.3-images.tar.gz")
	if res.TarPaths[0] != wantPkg || res.TarPaths[1] != wantImg {
		t.Errorf("TarPaths = %v, want [%s %s]", res.TarPaths, wantPkg, wantImg)
	}
	if len(res.Steps) != 5 {
		t.Errorf("期望 5 个步骤，实际 %d", len(res.Steps))
	}
}

func TestBuildDryRunLogsToOut(t *testing.T) {
	// 注入 Out buffer，验证 build 执行时逐步输出 [builder] 前缀日志（步骤开始/完成 + dry-run 标记）。
	cfg := loadSampleConfig(t)
	var buf bytes.Buffer

	res, err := Build(context.Background(), Options{
		Config:     cfg,
		OS:         "ubuntu",
		OSVersion:  "22.04",
		Arch:       "amd64",
		K8sVersion: "v1.27.3",
		Mirror:     mirror.Official,
		WorkDir:    filepath.Join(t.TempDir(), "work"),
		OutDir:     filepath.Join(t.TempDir(), "dist"),
		DryRun:     true,
		Out:        &buf,
	})
	if err != nil {
		t.Fatalf("Build dry-run 失败: %v", err)
	}
	if len(res.Steps) != 5 {
		t.Fatalf("期望 5 个步骤，实际 %d", len(res.Steps))
	}

	logs := buf.String()
	for _, want := range []string{
		"[builder] 步骤 1/5: 容器内软件包下载",
		"[builder] 步骤 1/5: 完成（dry-run）",
		"[builder] 步骤 2/5: 镜像清单与保存",
		"[builder] 步骤 2/5: 完成（dry-run）",
		"[builder] 步骤 3/5: 渲染脚本",
		"[builder] 步骤 3/5: 完成（install.sh + load-images.sh）",
		"[builder] 步骤 4/5: 生成 manifest",
		"[builder] 步骤 4/5: 完成（",
		"[builder] 步骤 5/5: 打包 tar.gz",
		"[builder] 步骤 5/5: 完成（",
	} {
		if !strings.Contains(logs, want) {
			t.Errorf("日志缺少 %q\n实际日志:\n%s", want, logs)
		}
	}
}

func TestBuildDryRunArbitraryK8sVersion(t *testing.T) {
	// 未注册在 builder.yaml versions 节中的合法版本，应能正常 dry-run（ksDef 为 nil 不 panic）。
	cfg := loadSampleConfig(t)
	res, err := Build(context.Background(), Options{
		Config:     cfg,
		OS:         "ubuntu",
		OSVersion:  "22.04",
		Arch:       "amd64",
		K8sVersion: "v1.29.5",
		Mirror:     mirror.Official,
		WorkDir:    filepath.Join(t.TempDir(), "work"),
		OutDir:     filepath.Join(t.TempDir(), "dist"),
		DryRun:     true,
	})
	if err != nil {
		t.Fatalf("Build dry-run（任意版本）失败: %v", err)
	}
	if res.BundleName != "pixiu-offline-ubuntu-22.04-amd64-v1.29.5" {
		t.Errorf("BundleName = %q", res.BundleName)
	}
	if len(res.Steps) != 5 {
		t.Errorf("期望 5 个步骤，实际 %d", len(res.Steps))
	}
}

func TestBuildInvalidParams(t *testing.T) {
	cfg := loadSampleConfig(t)
	_, err := Build(context.Background(), Options{
		Config: cfg, OS: "ubuntu", OSVersion: "22.04", Arch: "s390x",
		K8sVersion: "v1.27.3", Mirror: mirror.Official,
		WorkDir: t.TempDir(), OutDir: t.TempDir(), DryRun: true,
	})
	if err == nil {
		t.Fatal("非法架构应报错")
	}
}

func TestBuildArbitraryOSDryRun(t *testing.T) {
	// 未在 oses 注册表中的 OS/版本应可 dry-run 构建（约定构建镜像 centos:7）
	cfg := loadSampleConfig(t)
	res, err := Build(context.Background(), Options{
		Config: cfg, OS: "centos", OSVersion: "7", Arch: "amd64",
		K8sVersion: "v1.27.3", Mirror: mirror.Official,
		WorkDir: filepath.Join(t.TempDir(), "work"), OutDir: filepath.Join(t.TempDir(), "dist"),
		DryRun: true,
	})
	if err != nil {
		t.Fatalf("任意 OS dry-run 应成功: %v", err)
	}
	if res.BundleName != "pixiu-offline-centos-7-amd64-v1.27.3" {
		t.Errorf("BundleName = %q", res.BundleName)
	}
}

func TestBuildAbortsWhenDockerUnavailable(t *testing.T) {
	// docker 不可用（未显式 --skip-images）→ 构建应在软件包步骤中断，不产出后续步骤与 tar.gz。
	cfg := loadSampleConfig(t)
	res, err := Build(context.Background(), Options{
		Config:     cfg,
		OS:         "ubuntu",
		OSVersion:  "22.04",
		Arch:       "amd64",
		K8sVersion: "v1.27.3",
		Mirror:     mirror.Official,
		WorkDir:    filepath.Join(t.TempDir(), "work"),
		OutDir:     filepath.Join(t.TempDir(), "dist"),
		DockerBin:  "/nonexistent/docker-binary-xyz",
	})
	if err == nil {
		t.Fatal("docker 不可用时 Build 应中断并返回错误")
	}
	if !strings.Contains(err.Error(), "[容器内软件包下载] 中断") {
		t.Errorf("错误信息异常: %v", err)
	}
	// 中断后不应出现后续步骤（镜像清单与保存、渲染脚本、manifest、tar.gz）
	for _, s := range res.Steps {
		switch s.Name {
		case "镜像清单与保存", "渲染脚本", "生成 manifest", "打包 tar.gz":
			t.Errorf("中断后不应出现后续步骤 %q", s.Name)
		}
	}
	// 中断时不应生成 tar.gz
	if res.TarPath != "" {
		t.Errorf("中断时不应生成 tar.gz: %s", res.TarPath)
	}
}

func TestBuildSkipImagesWithFakeDocker(t *testing.T) {
	// --skip-images 显式跳过镜像阶段：docker 可用时构建成功，镜像步骤为 skipped 而非中断。
	cfg := loadSampleConfig(t)
	binDir := t.TempDir()
	binPath := filepath.Join(binDir, "docker")
	writeBuilderFakeDocker(t, binPath)

	res, err := Build(context.Background(), Options{
		Config:     cfg,
		OS:         "ubuntu",
		OSVersion:  "22.04",
		Arch:       "amd64",
		K8sVersion: "v1.27.3",
		Mirror:     mirror.Official,
		WorkDir:    filepath.Join(t.TempDir(), "work"),
		OutDir:     filepath.Join(t.TempDir(), "dist"),
		DockerBin:  binPath,
		SkipImages: true,
	})
	if err != nil {
		t.Fatalf("--skip-images + docker 可用时应成功: %v", err)
	}
	if _, err := os.Stat(res.TarPath); err != nil {
		t.Errorf("tar.gz 未生成: %v", err)
	}
	foundImg := false
	for _, s := range res.Steps {
		if s.Name == "镜像清单与保存" {
			foundImg = true
			if s.Status != "skipped" {
				t.Errorf("--skip-images 时镜像步骤应为 skipped，实际 %s", s.Status)
			}
		}
	}
	if !foundImg {
		t.Error("Steps 中缺少镜像清单与保存步骤")
	}
}

func TestBuildSkipAddons(t *testing.T) {
	// --skip-addons 显式跳过附加组件镜像：核心镜像仍拉取/保存，附加组件不拉取、不生成 tar。
	cfg := loadSampleConfig(t)
	binDir := t.TempDir()
	binPath := filepath.Join(binDir, "docker")
	writeBuilderImagesFakeDocker(t, binPath)
	kubeadmPath := filepath.Join(binDir, "kubeadm")
	writeBuilderFakeKubeadm(t, kubeadmPath)

	res, err := Build(context.Background(), Options{
		Config:     cfg,
		OS:         "ubuntu",
		OSVersion:  "22.04",
		Arch:       "amd64",
		K8sVersion: "v1.27.3",
		Mirror:     mirror.Official,
		WorkDir:    filepath.Join(t.TempDir(), "work"),
		OutDir:     filepath.Join(t.TempDir(), "dist"),
		DockerBin:  binPath,
		KubeadmBin: kubeadmPath,
		SkipAddons: true,
	})
	if err != nil {
		t.Fatalf("--skip-addons 构建失败: %v", err)
	}
	if _, err := os.Stat(res.TarPath); err != nil {
		t.Errorf("tar.gz 未生成: %v", err)
	}
	// 镜像步骤应为 ok，且消息说明按 --skip-addons 跳过附加组件
	foundImg := false
	for _, s := range res.Steps {
		if s.Name == "镜像清单与保存" {
			foundImg = true
			if s.Status != "ok" {
				t.Errorf("--skip-addons 时镜像步骤应为 ok，实际 %s", s.Status)
			}
			if !strings.Contains(s.Message, "跳过附加组件") {
				t.Errorf("镜像步骤消息应说明跳过附加组件，实际 %q", s.Message)
			}
		}
	}
	if !foundImg {
		t.Fatal("Steps 中缺少镜像清单与保存步骤")
	}
	// 核心镜像 tar 应存在；addons 目录不应生成 flannel.tar
	if _, err := os.Stat(filepath.Join(res.BundleDir, "images", "core", "kube-apiserver.tar")); err != nil {
		t.Errorf("核心镜像 tar 缺失: %v", err)
	}
	if _, err := os.Stat(filepath.Join(res.BundleDir, "images", "addons", "flannel.tar")); err == nil {
		t.Error("--skip-addons 时不应生成附加组件 flannel.tar")
	}
}

func TestBuildModePackages(t *testing.T) {
	// --mode packages 仅构建软件包：软件包步骤执行，镜像步骤跳过（非失败），tar 正常生成。
	cfg := loadSampleConfig(t)
	binDir := t.TempDir()
	binPath := filepath.Join(binDir, "docker")
	writeBuilderFakeDocker(t, binPath)

	res, err := Build(context.Background(), Options{
		Config:     cfg,
		OS:         "ubuntu",
		OSVersion:  "22.04",
		Arch:       "amd64",
		K8sVersion: "v1.27.3",
		Mirror:     mirror.Official,
		WorkDir:    filepath.Join(t.TempDir(), "work"),
		OutDir:     filepath.Join(t.TempDir(), "dist"),
		DockerBin:  binPath,
		Mode:       "packages",
	})
	if err != nil {
		t.Fatalf("--mode packages 构建失败: %v", err)
	}
	if _, err := os.Stat(res.TarPath); err != nil {
		t.Errorf("tar.gz 未生成: %v", err)
	}
	checkStep(t, res, "容器内软件包下载", "ok", "")
	checkStep(t, res, "镜像清单与保存", "skipped", "按 --mode packages 跳过镜像")
	// packages 模式不调用 images.Fetch：addons 目录不应生成 flannel.tar
	if _, err := os.Stat(filepath.Join(res.BundleDir, "images", "addons", "flannel.tar")); err == nil {
		t.Error("--mode packages 时不应生成附加组件镜像 tar")
	}
}

func TestBuildModeImages(t *testing.T) {
	// --mode images 仅构建镜像：软件包步骤跳过（非失败），镜像步骤执行，tar 正常生成。
	cfg := loadSampleConfig(t)
	binDir := t.TempDir()
	binPath := filepath.Join(binDir, "docker")
	writeBuilderImagesFakeDocker(t, binPath)
	kubeadmPath := filepath.Join(binDir, "kubeadm")
	writeBuilderFakeKubeadm(t, kubeadmPath)

	res, err := Build(context.Background(), Options{
		Config:     cfg,
		OS:         "ubuntu",
		OSVersion:  "22.04",
		Arch:       "amd64",
		K8sVersion: "v1.27.3",
		Mirror:     mirror.Official,
		WorkDir:    filepath.Join(t.TempDir(), "work"),
		OutDir:     filepath.Join(t.TempDir(), "dist"),
		DockerBin:  binPath,
		KubeadmBin: kubeadmPath,
		Mode:       "images",
	})
	if err != nil {
		t.Fatalf("--mode images 构建失败: %v", err)
	}
	if res.BundleName != "pixiu-offline-ubuntu-22.04-amd64-v1.27.3" {
		t.Errorf("指定 OS 时 BundleName = %q", res.BundleName)
	}
	if _, err := os.Stat(res.TarPath); err != nil {
		t.Errorf("tar.gz 未生成: %v", err)
	}
	checkStep(t, res, "容器内软件包下载", "skipped", "按 --mode images 跳过软件包")
	checkStep(t, res, "镜像清单与保存", "ok", "")
	// 核心镜像 tar 应存在
	if _, err := os.Stat(filepath.Join(res.BundleDir, "images", "core", "kube-apiserver.tar")); err != nil {
		t.Errorf("核心镜像 tar 缺失: %v", err)
	}
}

func TestBuildModeImagesWithoutOS(t *testing.T) {
	// --mode images 未指定 OS：使用默认构建容器，产物名为 pixiu-offline-images-{arch}-{k8s}。
	cfg := loadSampleConfig(t)
	binDir := t.TempDir()
	binPath := filepath.Join(binDir, "docker")
	writeBuilderImagesFakeDocker(t, binPath)
	kubeadmPath := filepath.Join(binDir, "kubeadm")
	writeBuilderFakeKubeadm(t, kubeadmPath)

	res, err := Build(context.Background(), Options{
		Config:     cfg,
		Arch:       "amd64",
		K8sVersion: "v1.27.3",
		Mirror:     mirror.Official,
		WorkDir:    filepath.Join(t.TempDir(), "work"),
		OutDir:     filepath.Join(t.TempDir(), "dist"),
		DockerBin:  binPath,
		KubeadmBin: kubeadmPath,
		Mode:       "images",
	})
	if err != nil {
		t.Fatalf("--mode images 无 OS 构建失败: %v", err)
	}
	if res.BundleName != "pixiu-offline-images-amd64-v1.27.3" {
		t.Errorf("未指定 OS 时 BundleName = %q", res.BundleName)
	}
	if _, err := os.Stat(res.TarPath); err != nil {
		t.Errorf("tar.gz 未生成: %v", err)
	}
	checkStep(t, res, "容器内软件包下载", "skipped", "按 --mode images 跳过软件包")
	checkStep(t, res, "镜像清单与保存", "ok", "")
}

func TestBuildDryRunModes(t *testing.T) {
	// dry-run 下三种模式的步骤状态：packages=镜像跳过；images=软件包跳过；all=两步骤均 ok。
	cases := []struct {
		mode          string
		wantPkgStatus string
		wantImgStatus string
	}{
		{"packages", "ok", "skipped"},
		{"images", "skipped", "ok"},
		{"all", "ok", "ok"},
	}
	for _, c := range cases {
		t.Run(c.mode, func(t *testing.T) {
			cfg := loadSampleConfig(t)
			res, err := Build(context.Background(), Options{
				Config:     cfg,
				OS:         "ubuntu",
				OSVersion:  "22.04",
				Arch:       "amd64",
				K8sVersion: "v1.27.3",
				Mirror:     mirror.Official,
				WorkDir:    filepath.Join(t.TempDir(), "work"),
				OutDir:     filepath.Join(t.TempDir(), "dist"),
				Mode:       c.mode,
				DryRun:     true,
			})
			if err != nil {
				t.Fatalf("dry-run %s 失败: %v", c.mode, err)
			}
			checkStep(t, res, "容器内软件包下载", c.wantPkgStatus, "")
			checkStep(t, res, "镜像清单与保存", c.wantImgStatus, "")
			if _, err := os.Stat(res.TarPath); err != nil {
				t.Errorf("tar.gz 未生成: %v", err)
			}
		})
	}
}

func TestBuildDefaultWorkDirUsesAbsoluteMountPath(t *testing.T) {
	// 默认相对 WorkDir（"./work"）时，Build 应把 WorkDir/OutDir 归一化为绝对路径，
	// 从而 docker run 的 -v 挂载宿主机路径为绝对路径（否则 docker 会按 volume 名校验失败）。
	cfg := loadSampleConfig(t)
	work := t.TempDir()
	t.Chdir(work)

	binDir := t.TempDir()
	binPath := filepath.Join(binDir, "docker")
	writeBuilderFakeDocker(t, binPath)

	res, err := Build(context.Background(), Options{
		Config:     cfg,
		OS:         "ubuntu",
		OSVersion:  "22.04",
		Arch:       "amd64",
		K8sVersion: "v1.27.3",
		Mirror:     mirror.Official,
		DockerBin:  binPath,
		SkipImages: true,
	})
	if err != nil {
		t.Fatalf("Build 失败: %v", err)
	}

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	wantBundle := filepath.Join(cwd, "work", res.BundleName)
	if res.BundleDir != wantBundle {
		t.Errorf("BundleDir 应归一化为 %q，实际 %q", wantBundle, res.BundleDir)
	}
	if !filepath.IsAbs(res.BundleDir) {
		t.Errorf("BundleDir 应为绝对路径: %q", res.BundleDir)
	}

	// 断言实际传给 docker 的挂载路径是绝对路径
	logPath := filepath.Join(binDir, "args.log")
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("读取 fake docker args.log 失败: %v", err)
	}
	args := strings.Fields(string(data))
	var mount string
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "-v" {
			mount = args[i+1]
			break
		}
	}
	if mount == "" {
		t.Fatalf("docker run args 中缺少 -v 挂载: %v", args)
	}
	hostDir := strings.TrimSuffix(mount, ":/out")
	if !filepath.IsAbs(hostDir) {
		t.Errorf("-v 挂载宿主机路径应为绝对路径，实际 %q（命令: %s）", hostDir, res.Steps[0].Message)
	}
	if strings.Contains(hostDir, "work/pixiu-offline-") && !filepath.IsAbs(hostDir) {
		t.Errorf("挂载路径仍为相对形式: %q", hostDir)
	}
	if !strings.HasPrefix(hostDir, cwd+string(os.PathSeparator)) {
		t.Errorf("挂载路径应以 cwd 为前缀，实际 %q（cwd=%s）", hostDir, cwd)
	}
}

func TestTarGzRoundTrip(t *testing.T) {
	srcDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(srcDir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "a.txt"), []byte("aaa"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "sub", "b.txt"), []byte("bbb"), 0o644); err != nil {
		t.Fatal(err)
	}

	tarPath := filepath.Join(t.TempDir(), "bundle.tar.gz")
	if err := TarGz(srcDir, tarPath); err != nil {
		t.Fatalf("TarGz 失败: %v", err)
	}

	destDir := t.TempDir()
	if err := UntarGz(tarPath, destDir); err != nil {
		t.Fatalf("UntarGz 失败: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(destDir, filepath.Base(srcDir), "a.txt"))
	if err != nil || string(data) != "aaa" {
		t.Errorf("解压后 a.txt 异常: %v", err)
	}
	data, err = os.ReadFile(filepath.Join(destDir, filepath.Base(srcDir), "sub", "b.txt"))
	if err != nil || string(data) != "bbb" {
		t.Errorf("解压后 sub/b.txt 异常: %v", err)
	}
}

func TestUntarGzPathTraversal(t *testing.T) {
	// 构造恶意 tar：路径 ../evil
	srcDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(srcDir, "x"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 直接构造恶意 tar.gz
	evilTar := filepath.Join(t.TempDir(), "evil.tar.gz")
	if err := writeEvilTar(evilTar); err != nil {
		t.Fatal(err)
	}
	if err := UntarGz(evilTar, t.TempDir()); err == nil {
		t.Fatal("目录穿越 tar 应被拒绝")
	}
}

func TestVerifyDirectory(t *testing.T) {
	bundleDir := t.TempDir()
	writeSampleBundleFiles(t, bundleDir)

	m, err := manifest.Generate(bundleDir, manifest.Meta{OS: "ubuntu", OSVersion: "22.04", Arch: "amd64", K8sVersion: "v1.27.3"})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Write(filepath.Join(bundleDir, manifest.ManifestFileName)); err != nil {
		t.Fatal(err)
	}

	got, err := Verify(bundleDir)
	if err != nil {
		t.Fatalf("Verify 目录失败: %v", err)
	}
	if got.Meta.K8sVersion != "v1.27.3" {
		t.Errorf("manifest 解析异常: %+v", got.Meta)
	}
}

func TestVerifyTarGz(t *testing.T) {
	bundleDir := t.TempDir()
	writeSampleBundleFiles(t, bundleDir)
	m, _ := manifest.Generate(bundleDir, manifest.Meta{OS: "ubuntu", OSVersion: "22.04", Arch: "amd64", K8sVersion: "v1.27.3"})
	if err := m.Write(filepath.Join(bundleDir, manifest.ManifestFileName)); err != nil {
		t.Fatal(err)
	}

	tarPath := filepath.Join(t.TempDir(), "bundle.tar.gz")
	if err := TarGz(bundleDir, tarPath); err != nil {
		t.Fatal(err)
	}

	if _, err := Verify(tarPath); err != nil {
		t.Fatalf("Verify tar.gz 失败: %v", err)
	}
}

func TestVerifyDetectsTamperedTarGz(t *testing.T) {
	bundleDir := t.TempDir()
	writeSampleBundleFiles(t, bundleDir)
	m, _ := manifest.Generate(bundleDir, manifest.Meta{OS: "ubuntu", OSVersion: "22.04", Arch: "amd64", K8sVersion: "v1.27.3"})
	if err := m.Write(filepath.Join(bundleDir, manifest.ManifestFileName)); err != nil {
		t.Fatal(err)
	}

	tarPath := filepath.Join(t.TempDir(), "bundle.tar.gz")
	if err := TarGz(bundleDir, tarPath); err != nil {
		t.Fatal(err)
	}

	// 篡改打包前的源文件后重新打包，模拟坏 bundle
	if err := os.WriteFile(filepath.Join(bundleDir, "packages", "kubeadm.deb"), []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}
	tarPathBad := filepath.Join(t.TempDir(), "bad.tar.gz")
	if err := TarGz(bundleDir, tarPathBad); err != nil {
		t.Fatal(err)
	}

	if _, err := Verify(tarPathBad); err == nil {
		t.Fatal("篡改 bundle 的 Verify 应失败")
	}
}

// writeSampleBundleFiles 写入样例 bundle 内容。
func writeSampleBundleFiles(t *testing.T, root string) {
	t.Helper()
	files := map[string]string{
		"packages/kubeadm.deb":           "kubeadm-pkg",
		"packages/kubelet.deb":           "kubelet-pkg",
		"packages/runc.amd64":            "runc-pkg", // 非 .deb/.rpm，应作为普通文件被 manifest 收录
		"packages/runtime/crictl.tar.gz": "crictl-fallback-tar",
		"packages/conntrack.deb":         "conntrack-pkg",
		"images/core/kube-apiserver.tar": "core-image",
		"images/addons/flannel.tar":      "flannel-image",
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
}

// checkStep 断言 Steps 中某步骤的状态与消息子串。
func checkStep(t *testing.T, res *Result, name, wantStatus, wantMsgPart string) {
	t.Helper()
	for _, s := range res.Steps {
		if s.Name == name {
			if s.Status != wantStatus {
				t.Errorf("步骤 %q 状态应为 %s，实际 %s", name, wantStatus, s.Status)
			}
			if wantMsgPart != "" && !strings.Contains(s.Message, wantMsgPart) {
				t.Errorf("步骤 %q 消息应含 %q，实际 %q", name, wantMsgPart, s.Message)
			}
			return
		}
	}
	t.Errorf("Steps 中缺少步骤 %q", name)
}

// writeBuilderFakeDocker 创建 fake docker：info 成功；run 时在挂载目录写 .deb（供软件包阶段收集）。
// 每次调用的全部参数会记录到脚本同目录 args.log，供测试断言 docker 命令构造。
func writeBuilderFakeDocker(t *testing.T, binPath string) {
	t.Helper()
	script := `#!/bin/sh
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
echo "$@" > "$SCRIPT_DIR/args.log"
if [ "$1" = "info" ]; then
  echo "Server Version: fake"
  exit 0
fi
if [ "$1" = "run" ]; then
  for arg in "$@"; do
    case "$arg" in
      *:/out) workdir="${arg%:/out}"; break ;;
    esac
  done
  mkdir -p "$workdir"
  printf 'fake-kubeadm' > "$workdir/kubeadm.deb"
  printf 'fake-containerd' > "$workdir/containerd.io.deb"
  printf 'fake-deps' > "$workdir/conntrack.deb"
  echo "fake docker run ok"
  exit 0
fi
exit 1
`
	if err := os.WriteFile(binPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

// writeBuilderImagesFakeDocker 创建支持完整镜像阶段的 fake docker：
// info 成功；run 在 kubeadm images list 时输出核心镜像；
// run 在镜像打包脚本（docker save）时按 -o 路径写 tar；
// 软件包阶段写 .deb。
func writeBuilderImagesFakeDocker(t *testing.T, binPath string) {
	t.Helper()
	script := `#!/bin/sh
case "$1" in
  info)
    echo "Server Version: fake"
    exit 0
    ;;
  run)
    saw_images=
    saw_list=
    outdir=
    script=
    prev=
    for arg in "$@"; do
      [ "$arg" = "images" ] && saw_images=1
      [ "$arg" = "list" ] && saw_list=1
      case "$arg" in
        *:/out) outdir="${arg%:/out}" ;;
      esac
      if [ "$prev" = "-c" ]; then script="$arg"; fi
      prev="$arg"
    done
    if [ -n "$saw_images" ] && [ -n "$saw_list" ]; then
      echo "registry.k8s.io/kube-apiserver:v1.27.3"
      echo "registry.k8s.io/kube-controller-manager:v1.27.3"
      echo "registry.k8s.io/kube-scheduler:v1.27.3"
      echo "registry.k8s.io/kube-proxy:v1.27.3"
      echo "registry.k8s.io/pause:3.9"
      echo "registry.k8s.io/etcd:3.5.7"
      echo "registry.k8s.io/coredns/coredns:v1.10.1"
      exit 0
    fi
    # 镜像打包容器：解析 docker save -o 并落盘
    if [ -n "$outdir" ] && [ -n "$script" ] && echo "$script" | grep -q "docker save"; then
      printf '%s\n' "$script" | while IFS= read -r line; do
        case "$line" in
          *"docker save -o "*)
            rest="${line#*docker save -o }"
            path="${rest#\'}"
            path="${path%%\'*}"
            path="${path#/out/}"
            if [ -n "$path" ]; then
              mkdir -p "$outdir/$(dirname "$path")"
              printf 'fake-image-%s' "$path" > "$outdir/$path"
            fi
            ;;
        esac
      done
      exit 0
    fi
    # 软件包下载阶段：在挂载目录写 .deb
    if [ -n "$outdir" ]; then
      mkdir -p "$outdir"
      printf 'fake-kubeadm' > "$outdir/kubeadm.deb"
      printf 'fake-containerd' > "$outdir/containerd.io.deb"
      printf 'fake-deps' > "$outdir/conntrack.deb"
      echo "fake docker run ok"
      exit 0
    fi
    exit 0
    ;;
esac
exit 1
`
	if err := os.WriteFile(binPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

// writeBuilderFakeKubeadm 创建假 kubeadm，供 Linux 宿主机直跑 images list 的测试路径使用。
func writeBuilderFakeKubeadm(t *testing.T, binPath string) {
	t.Helper()
	script := `#!/bin/sh
if [ "$1" = "config" ] && [ "$2" = "images" ] && [ "$3" = "list" ]; then
  echo "registry.k8s.io/kube-apiserver:v1.27.3"
  echo "registry.k8s.io/kube-controller-manager:v1.27.3"
  echo "registry.k8s.io/kube-scheduler:v1.27.3"
  echo "registry.k8s.io/kube-proxy:v1.27.3"
  echo "registry.k8s.io/pause:3.9"
  echo "registry.k8s.io/etcd:3.5.7"
  echo "registry.k8s.io/coredns/coredns:v1.10.1"
  exit 0
fi
echo "unexpected kubeadm args: $*" >&2
exit 1
`
	if err := os.WriteFile(binPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

// writeEvilTar 写一个包含 ../evil 路径的恶意 tar.gz。
func writeEvilTar(path string) error {
	out, err := os.Create(path)
	if err != nil {
		return err
	}
	defer out.Close()
	gz := gzip.NewWriter(out)
	defer gz.Close()
	tw := tar.NewWriter(gz)
	defer tw.Close()

	hdr := &tar.Header{Name: "../evil", Typeflag: tar.TypeReg, Size: 1}
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	_, err = tw.Write([]byte("x"))
	return err
}
