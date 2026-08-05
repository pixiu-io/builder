package images

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"builder/internal/config"
)

func TestShortName(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"registry.k8s.io/kube-apiserver:v1.27.3", "kube-apiserver"},
		{"registry.k8s.io/kube-proxy:v1.27.3", "kube-proxy"},
		{"docker.io/flannel/flannel:v0.24.2", "flannel"},
		{"registry.k8s.io/pause:3.9", "pause"},
		{"kube-apiserver:v1.27.3", "kube-apiserver"},
		{"registry.k8s.io/coredns/coredns:v1.10.1", "coredns"},
	}
	for _, c := range cases {
		if got := ShortName(c.in); got != c.want {
			t.Errorf("ShortName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestSafeTarName(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"registry.k8s.io/kube-apiserver:v1.27.3", "kube-apiserver"},
		{"registry.k8s.io/coredns/coredns:v1.10.1", "coredns"},
	}
	for _, c := range cases {
		if got := SafeTarName(c.in); got != c.want {
			t.Errorf("SafeTarName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// writeFakeKubeadm 创建假 kubeadm：config images list 输出固定核心镜像清单。
func writeFakeKubeadm(t *testing.T, binPath string) {
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

// writeFakeDocker 创建 fake docker：
// - info 成功
// - run + kubeadm images list → 输出核心镜像清单
// - run + 容器内 pull/save 脚本 → 按 docker save -o 路径在挂载目录写 tar
func writeFakeDocker(t *testing.T, binPath string) {
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
    # 镜像打包容器：解析 docker save -o '/out/...' 并在宿主机挂载目录落盘
    if [ -n "$outdir" ] && [ -n "$script" ]; then
      printf '%s\n' "$script" | while IFS= read -r line; do
        case "$line" in
          *"docker save -o "*)
            # 取 -o 后第一个单引号路径：docker save -o '/out/core/foo.tar' 'img'
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
    exit 0
    ;;
esac
exit 1
`
	if err := os.WriteFile(binPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestFetchSkippedNoDocker(t *testing.T) {
	res, err := Fetch(context.Background(), Options{
		DockerBin:       "/nonexistent/docker-xyz",
		BuildImage:      "ubuntu:22.04",
		PkgManager:      "apt",
		K8sMinor:        "v1.27",
		Codename:        "jammy",
		K8sVersion:      "v1.27.3",
		ImageRepository: "registry.k8s.io",
		Arch:            "amd64",
		ImagesOutDir:    t.TempDir(),
	})
	if err != nil {
		t.Fatalf("docker 不可用应返回 skipped 而非错误: %v", err)
	}
	if !res.Skipped {
		t.Error("期望 Skipped=true")
	}
}

func TestNormalizeArch(t *testing.T) {
	if got := normalizeArch("x86_64"); got != "amd64" {
		t.Errorf("x86_64 → %q", got)
	}
	if got := normalizeArch("aarch64"); got != "arm64" {
		t.Errorf("aarch64 → %q", got)
	}
	if got := normalizeArch(""); got != "amd64" {
		t.Errorf("empty → %q", got)
	}
}

func TestBuildPullSaveScript(t *testing.T) {
	s := buildPullSaveScript([]saveJob{
		{Name: "kube-apiserver", Image: "registry.k8s.io/kube-apiserver:v1.27.3", SubDir: "core"},
		{Name: "flannel", Image: "docker.io/flannel/flannel:v0.24.2", SubDir: "addons"},
	})
	for _, want := range []string{
		"set -e",
		"mkdir -p /out/core /out/addons",
		"docker pull 'registry.k8s.io/kube-apiserver:v1.27.3'",
		"docker save -o '/out/core/kube-apiserver.tar' 'registry.k8s.io/kube-apiserver:v1.27.3'",
		"docker pull 'docker.io/flannel/flannel:v0.24.2'",
		"docker save -o '/out/addons/flannel.tar' 'docker.io/flannel/flannel:v0.24.2'",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("脚本缺少 %q:\n%s", want, s)
		}
	}
}

func TestFilterImageLines(t *testing.T) {
	out := "Get:1 http://archive.ubuntu.com/ubuntu noble InRelease [256 kB]\n" +
		"Get:2 http://archive.ubuntu.com/ubuntu noble-updates InRelease [118 kB]\n" +
		"Selecting previously unselected package kubeadm.\n" +
		"Setting up kubeadm (1.31.0-1.1)...\n" +
		"Fetched 25.5 MB in 3s (8.5 MB/s)\n" +
		"registry.k8s.io/kube-apiserver:v1.31.0\n" +
		"registry.k8s.io/kube-controller-manager:v1.31.0\n" +
		"\n" +
		"   \n" +
		"\t\n" +
		"registry.k8s.io/kube-proxy:v1.31.0\n" +
		"registry.k8s.io/coredns/coredns:v1.31.0\n"
	want := []string{
		"registry.k8s.io/kube-apiserver:v1.31.0",
		"registry.k8s.io/kube-controller-manager:v1.31.0",
		"registry.k8s.io/kube-proxy:v1.31.0",
		"registry.k8s.io/coredns/coredns:v1.31.0",
	}
	got := filterImageLines(out)
	if len(got) != len(want) {
		t.Fatalf("filterImageLines 返回 %d 行，期望 %d：%v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("第 %d 行 = %q，期望 %q", i, got[i], want[i])
		}
	}
}

func TestFetchWithFakes(t *testing.T) {
	binDir := t.TempDir()
	dockerPath := filepath.Join(binDir, "docker")
	writeFakeDocker(t, dockerPath)
	kubeadmPath := filepath.Join(binDir, "kubeadm")
	writeFakeKubeadm(t, kubeadmPath)

	outDir := t.TempDir()
	res, err := Fetch(context.Background(), Options{
		DockerBin:       dockerPath,
		BuildImage:      "ubuntu:22.04",
		PackImage:       "docker:24-cli",
		PkgManager:      "apt",
		K8sMinor:        "v1.27",
		Codename:        "jammy",
		AptOS:           "ubuntu",
		K8sVersion:      "v1.27.3",
		ImageRepository: "registry.k8s.io",
		Arch:            runtime.GOARCH,
		KubeadmBin:      kubeadmPath,
		Addons:          []config.Addon{{Name: "flannel", Image: "docker.io/flannel/flannel", Tag: "v0.24.2"}},
		ImagesOutDir:    outDir,
	})
	if err != nil {
		t.Fatalf("Fetch 失败: %v", err)
	}
	if res.Skipped {
		t.Fatal("不应 skipped")
	}
	if len(res.Core) != 7 {
		t.Errorf("期望 7 个核心镜像，实际 %d", len(res.Core))
	}
	if len(res.Addons) != 1 {
		t.Errorf("期望 1 个 addon，实际 %d", len(res.Addons))
	}
	if _, err := os.Stat(filepath.Join(outDir, "core", "kube-apiserver.tar")); err != nil {
		t.Errorf("核心镜像 tar 缺失: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outDir, "addons", "flannel.tar")); err != nil {
		t.Errorf("addon tar 缺失: %v", err)
	}
	if res.Addons[0].SHA256 == "" {
		t.Error("期望 addon tar 有 sha256")
	}
}

func TestFetchSkipAddons(t *testing.T) {
	binDir := t.TempDir()
	dockerPath := filepath.Join(binDir, "docker")
	writeFakeDocker(t, dockerPath)
	kubeadmPath := filepath.Join(binDir, "kubeadm")
	writeFakeKubeadm(t, kubeadmPath)

	outDir := t.TempDir()
	res, err := Fetch(context.Background(), Options{
		DockerBin:       dockerPath,
		BuildImage:      "ubuntu:22.04",
		PackImage:       "docker:24-cli",
		PkgManager:      "apt",
		K8sMinor:        "v1.27",
		Codename:        "jammy",
		AptOS:           "ubuntu",
		K8sVersion:      "v1.27.3",
		ImageRepository: "registry.k8s.io",
		Arch:            runtime.GOARCH,
		KubeadmBin:      kubeadmPath,
		Addons: []config.Addon{
			{Name: "flannel", Image: "docker.io/flannel/flannel", Tag: "v0.24.2"},
			{Name: "dashboard", Image: "docker.io/kubernetesui/dashboard", Tag: "v2.7.0"},
		},
		ImagesOutDir: outDir,
		SkipAddons:   true,
	})
	if err != nil {
		t.Fatalf("Fetch(SkipAddons) 失败: %v", err)
	}
	if res.Skipped {
		t.Fatal("不应 skipped")
	}
	if !res.SkipAddons {
		t.Error("期望 SkipAddons=true")
	}
	if len(res.Core) != 7 {
		t.Errorf("SkipAddons 时核心镜像仍应拉取，期望 7 个，实际 %d", len(res.Core))
	}
	if len(res.Addons) != 0 {
		t.Errorf("SkipAddons 时不应拉取附加组件，实际 %d 个", len(res.Addons))
	}
	if _, err := os.Stat(filepath.Join(outDir, "core", "kube-apiserver.tar")); err != nil {
		t.Errorf("核心镜像 tar 缺失: %v", err)
	}
	for _, name := range []string{"flannel.tar", "dashboard.tar"} {
		if _, err := os.Stat(filepath.Join(outDir, "addons", name)); err == nil {
			t.Errorf("SkipAddons 时不应生成 %s", name)
		}
	}
}

func TestFetchDryRun(t *testing.T) {
	res, err := Fetch(context.Background(), Options{
		BuildImage:      "ubuntu:22.04",
		PkgManager:      "apt",
		K8sMinor:        "v1.27",
		Codename:        "jammy",
		K8sVersion:      "v1.27.3",
		ImageRepository: "registry.k8s.io",
		Addons:          []config.Addon{{Name: "flannel", Image: "docker.io/flannel/flannel", Tag: "v0.24.2"}},
		ImagesOutDir:    t.TempDir(),
		DryRun:          true,
	})
	if err != nil {
		t.Fatalf("DryRun 不应执行真实命令，实际: %v", err)
	}
	if res.Skipped {
		t.Error("DryRun 不应被 docker 不可用拦截")
	}
}

func TestSafeTarNameAddonFlannel(t *testing.T) {
	if got := SafeTarName("docker.io/flannel/flannel:v0.24.2"); got != "flannel" {
		t.Errorf("flannel tar 名异常: %q", got)
	}
}

func TestListCoreImagesUsesKubeadmBin(t *testing.T) {
	binDir := t.TempDir()
	kubeadmPath := filepath.Join(binDir, "kubeadm")
	writeFakeKubeadm(t, kubeadmPath)
	dockerPath := filepath.Join(binDir, "docker")
	writeFakeDocker(t, dockerPath)

	got, err := listCoreImages(context.Background(), Options{
		DockerBin:       dockerPath,
		BuildImage:      "ubuntu:22.04",
		K8sVersion:      "v1.27.3",
		ImageRepository: "registry.k8s.io",
		Arch:            runtime.GOARCH,
		KubeadmBin:      kubeadmPath,
	})
	if err != nil {
		t.Fatalf("listCoreImages: %v", err)
	}
	if len(got) != 7 {
		t.Fatalf("期望 7 个镜像，实际 %d: %v", len(got), got)
	}
}

func TestFilterCoreImages(t *testing.T) {
	imgs := []string{
		"registry.k8s.io/kube-apiserver:v1.27.3",
		"registry.k8s.io/kube-controller-manager:v1.27.3",
		"registry.k8s.io/pause:3.9",
	}

	// 短名过滤
	got := filterCoreImages(imgs, []string{"kube-apiserver", "pause"})
	if len(got) != 2 || got[0] != imgs[0] || got[1] != imgs[2] {
		t.Errorf("短名过滤 = %v", got)
	}
	// 完整引用精确匹配
	got = filterCoreImages(imgs, []string{"registry.k8s.io/pause:3.9"})
	if len(got) != 1 || got[0] != imgs[2] {
		t.Errorf("完整引用过滤 = %v", got)
	}
	// 空过滤 → 原样
	got = filterCoreImages(imgs, nil)
	if len(got) != 3 {
		t.Errorf("空过滤应原样返回: %v", got)
	}
	// 无匹配 → 空
	got = filterCoreImages(imgs, []string{"nonexistent"})
	if len(got) != 0 {
		t.Errorf("无匹配应为空: %v", got)
	}
}

func TestFetchWithExternalCoreImages(t *testing.T) {
	// CoreImages 非空：直接使用，不再走 kubeadm（不提供 KubeadmBin 也应成功）。
	binDir := t.TempDir()
	dockerPath := filepath.Join(binDir, "docker")
	writeFakeDocker(t, dockerPath)

	outDir := t.TempDir()
	res, err := Fetch(context.Background(), Options{
		DockerBin:       dockerPath,
		BuildImage:      "ubuntu:22.04",
		PackImage:       "docker:24-cli",
		K8sVersion:      "v1.27.3",
		ImageRepository: "registry.k8s.io",
		Arch:            runtime.GOARCH,
		CoreImages:      []string{"registry.k8s.io/kube-apiserver:v1.27.3"},
		Addons:          []config.Addon{{Name: "flannel", Image: "docker.io/flannel/flannel", Tag: "v0.24.2"}},
		ImagesOutDir:    outDir,
	})
	if err != nil {
		t.Fatalf("Fetch(CoreImages) 失败: %v", err)
	}
	if res.Skipped {
		t.Fatal("不应 skipped")
	}
	if len(res.Core) != 1 || res.Core[0].SourceImage != "registry.k8s.io/kube-apiserver:v1.27.3" {
		t.Errorf("核心镜像 = %+v", res.Core)
	}
	if len(res.Addons) != 1 {
		t.Errorf("addons = %+v", res.Addons)
	}
	if _, err := os.Stat(filepath.Join(outDir, "core", "kube-apiserver.tar")); err != nil {
		t.Errorf("核心 tar 缺失: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outDir, "addons", "flannel.tar")); err != nil {
		t.Errorf("addon tar 缺失: %v", err)
	}
}

func TestFetchWithEmptyCoreImagesNoKubeadm(t *testing.T) {
	// CoreImages 空非 nil：不拉核心镜像，只拉 addons；不应调用 kubeadm（无 KubeadmBin 也应成功）。
	// 这正是 --only-addons 场景下 builder 传给 images.Fetch 的形态（核心全去、仅附加镜像）。
	binDir := t.TempDir()
	dockerPath := filepath.Join(binDir, "docker")
	writeFakeDocker(t, dockerPath)

	outDir := t.TempDir()
	res, err := Fetch(context.Background(), Options{
		DockerBin:       dockerPath,
		BuildImage:      "ubuntu:22.04",
		PackImage:       "docker:24-cli",
		K8sVersion:      "v1.27.3",
		ImageRepository: "registry.k8s.io",
		Arch:            runtime.GOARCH,
		CoreImages:      []string{},
		Addons:          []config.Addon{{Name: "flannel", Image: "docker.io/flannel/flannel", Tag: "v0.24.2"}},
		ImagesOutDir:    outDir,
	})
	if err != nil {
		t.Fatalf("Fetch(空 CoreImages) 失败: %v", err)
	}
	if len(res.Core) != 0 {
		t.Errorf("空 CoreImages 不应拉核心镜像: %+v", res.Core)
	}
	if len(res.Addons) != 1 {
		t.Errorf("addons = %+v", res.Addons)
	}
}

// TestFetchOnlyAddons 显式覆盖 --only-addons 场景：核心镜像清单为空（置空非 nil），
// 仅拉取 addon_images；不触发 kubeadm 核心清单生成。
func TestFetchOnlyAddons(t *testing.T) {
	binDir := t.TempDir()
	dockerPath := filepath.Join(binDir, "docker")
	writeFakeDocker(t, dockerPath)

	outDir := t.TempDir()
	res, err := Fetch(context.Background(), Options{
		DockerBin:       dockerPath,
		BuildImage:      "ubuntu:22.04",
		PackImage:       "docker:24-cli",
		K8sVersion:      "v1.27.3",
		ImageRepository: "registry.k8s.io",
		Arch:            runtime.GOARCH,
		CoreImages:      []string{}, // only-addons：核心镜像置空，不拉核心
		Addons: []config.Addon{
			{Name: "flannel", Image: "docker.io/flannel/flannel", Tag: "v0.24.2"},
			{Name: "metrics-server", Image: "registry.k8s.io/metrics-server/metrics-server", Tag: "v0.6.4"},
		},
		ImagesOutDir: outDir,
	})
	if err != nil {
		t.Fatalf("Fetch(only-addons) 失败: %v", err)
	}
	if res.Skipped {
		t.Fatal("不应 skipped")
	}
	if len(res.Core) != 0 {
		t.Errorf("only-addons 不应拉核心镜像: %+v", res.Core)
	}
	if len(res.Addons) != 2 {
		t.Errorf("only-addons 应拉取全部 addon_images，实际 %d: %+v", len(res.Addons), res.Addons)
	}
	if _, err := os.Stat(filepath.Join(outDir, "core", "kube-apiserver.tar")); err == nil {
		t.Error("only-addons 不应生成核心镜像 tar")
	}
	if _, err := os.Stat(filepath.Join(outDir, "addons", "flannel.tar")); err != nil {
		t.Errorf("only-addons 应生成 flannel.tar: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outDir, "addons", "metrics-server.tar")); err != nil {
		t.Errorf("only-addons 应生成 metrics-server.tar: %v", err)
	}
}

func TestFetchWithCoreFilter(t *testing.T) {
	// CoreFilter：走 kubeadm 生成后按短名过滤；Addons 空列表时不拉附加组件。
	binDir := t.TempDir()
	dockerPath := filepath.Join(binDir, "docker")
	writeFakeDocker(t, dockerPath)
	kubeadmPath := filepath.Join(binDir, "kubeadm")
	writeFakeKubeadm(t, kubeadmPath)

	outDir := t.TempDir()
	res, err := Fetch(context.Background(), Options{
		DockerBin:       dockerPath,
		BuildImage:      "ubuntu:22.04",
		PackImage:       "docker:24-cli",
		K8sVersion:      "v1.27.3",
		ImageRepository: "registry.k8s.io",
		Arch:            runtime.GOARCH,
		KubeadmBin:      kubeadmPath,
		CoreFilter:      []string{"kube-apiserver"},
		Addons:          []config.Addon{},
		ImagesOutDir:    outDir,
	})
	if err != nil {
		t.Fatalf("Fetch(CoreFilter) 失败: %v", err)
	}
	if len(res.Core) != 1 || res.Core[0].SourceImage != "registry.k8s.io/kube-apiserver:v1.27.3" {
		t.Errorf("CoreFilter 结果 = %+v", res.Core)
	}
	if len(res.Addons) != 0 {
		t.Errorf("空 addons 不应拉取: %+v", res.Addons)
	}
	if _, err := os.Stat(filepath.Join(outDir, "core", "kube-apiserver.tar")); err != nil {
		t.Errorf("核心 tar 缺失: %v", err)
	}
}
