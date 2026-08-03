package packages

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildDownloadScriptAPT(t *testing.T) {
	s := BuildDownloadScript(DownloadScriptOpts{
		PkgManager:  "apt",
		Repos:       append(K8sRepos("v1.27"), ContainerdRepos("ubuntu", "jammy", "")...),
		Pkgs:        []string{"kubeadm", "kubelet", "containerd.io", "conntrack"},
		ArchiveDir:  "/out",
		CheckCrictl: true,
	})
	for _, want := range []string{
		"set -e",
		"export DEBIAN_FRONTEND=noninteractive",
		"apt-get update",
		"apt-get install -y --no-install-recommends curl ca-certificates gnupg apt-transport-https",
		"curl -fsSL", "gpg --dearmor",
		"https://pkgs.k8s.io/core:/stable:/v1.27/deb/",
		"https://download.docker.com/linux/ubuntu jammy stable",
		"apt-get install -y --download-only --no-install-recommends",
		"Dir::Cache::archives=/out",
		"apt-get install --dry-run",
		"cri-tools-missing",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("apt 下载脚本缺少 %q:\n%s", want, s)
		}
	}
}

func TestBuildDownloadScriptDNF(t *testing.T) {
	s := BuildDownloadScript(DownloadScriptOpts{
		PkgManager:  "dnf",
		Repos:       append(K8sRepos("v1.28"), ContainerdRepos("", "", "rhel9")...),
		Pkgs:        []string{"kubeadm", "containerd.io", "nfs-utils"},
		ArchiveDir:  "/out",
		CheckCrictl: true,
	})
	for _, want := range []string{
		"set -e",
		"dnf makecache",
		"dnf -y install dnf-plugins-core",
		"[kubernetes]", "[docker-ce-stable]",
		"rpm --import",
		"https://pkgs.k8s.io/core:/stable:/v1.28/rpm/",
		"https://download.docker.com/linux/rhel/9/$basearch/stable",
		"dnf install -y --downloadonly --downloaddir=/out",
		"dnf download --resolve --destdir=/out",
		"dnf install --assumeno",
		"cri-tools-missing",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("dnf 下载脚本缺少 %q:\n%s", want, s)
		}
	}
}

func TestFetchDockerUnavailable(t *testing.T) {
	res, err := Fetch(context.Background(), Options{
		OutDir:     t.TempDir(),
		BuildImage: "ubuntu:22.04",
		PkgManager: "apt",
		K8sMinor:   "v1.27",
		Codename:   "jammy",
		Pkgs:       []string{"kubeadm"},
		DockerBin:  "/nonexistent/docker-binary-xyz",
	})
	if err != nil {
		t.Fatalf("docker 不可用时应返回 skipped 而非错误，实际: %v", err)
	}
	if !res.Skipped {
		t.Error("期望 Skipped=true")
	}
	if res.SkipReason == "" {
		t.Error("期望 SkipReason 非空")
	}
}

func TestFetchDryRun(t *testing.T) {
	res, err := Fetch(context.Background(), Options{
		OutDir:     t.TempDir(),
		BuildImage: "ubuntu:22.04",
		PkgManager: "apt",
		K8sMinor:   "v1.27",
		Codename:   "jammy",
		Pkgs:       []string{"kubeadm"},
		DryRun:     true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.DryRun {
		t.Error("期望 DryRun=true")
	}
	if !strings.HasPrefix(res.Command, "docker run") {
		t.Errorf("命令构造异常: %s", res.Command)
	}
	if !strings.Contains(res.Command, "-v ") || !strings.Contains(res.Command, ":/out") {
		t.Errorf("命令应包含外挂挂载: %s", res.Command)
	}
}

// writeFakeDocker 创建 fake docker：info 成功；run 时在挂载目录写 .deb，
// createMarker=true 时额外写入 cri-tools-missing 标记模拟 cri-tools 包缺失。
func writeFakeDocker(t *testing.T, binPath string, createMarker bool) {
	t.Helper()
	script := `#!/bin/sh
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
`
	if createMarker {
		script += `  touch "$workdir/cri-tools-missing"
`
	}
	script += `  echo "fake docker run ok"
  exit 0
fi
exit 1
`
	if err := os.WriteFile(binPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestFetchWithFakeDocker(t *testing.T) {
	binDir := t.TempDir()
	binPath := filepath.Join(binDir, "docker")
	writeFakeDocker(t, binPath, false)

	outDir := t.TempDir()
	res, err := Fetch(context.Background(), Options{
		OutDir:     outDir,
		BuildImage: "ubuntu:22.04",
		PkgManager: "apt",
		K8sMinor:   "v1.27",
		Codename:   "jammy",
		AptOS:      "ubuntu",
		Pkgs:       []string{"kubeadm", "kubelet", "containerd.io", "conntrack"},
		DockerBin:  binPath,
	})
	if err != nil {
		t.Fatalf("Fetch 失败: %v", err)
	}
	if res.Skipped {
		t.Fatalf("期望正常下载，实际 skipped: %s", res.SkipReason)
	}
	if len(res.Files) != 3 {
		t.Fatalf("期望收集 3 个 .deb，实际 %d: %+v", len(res.Files), res.Files)
	}
	if res.CrictlMissing {
		t.Error("无标记时不应触发 crictl 回退")
	}
	for _, f := range res.Files {
		if f.SHA256 == "" || f.Size == 0 || f.RelPath == "" {
			t.Errorf("文件元数据异常: %+v", f)
		}
	}
}

// writeFakeDockerLog 创建 fake docker：把每次调用的全部参数记录到脚本同目录 args.log；
// info 成功；run 时在挂载目录写 .deb。
func writeFakeDockerLog(t *testing.T, binDir string) {
	t.Helper()
	binPath := filepath.Join(binDir, "docker")
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

func TestFetchMountUsesAbsolutePath(t *testing.T) {
	// 相对 OutDir 独立调用 Fetch 时，docker run 的 -v 挂载路径必须是绝对路径
	// （docker 将相对路径当 volume 名校验，含 "/" 非法）。防御性 Abs 应生效。
	work := t.TempDir()
	t.Chdir(work)
	relOut := filepath.Join("work", "pixiu-offline-ubuntu-22.04-amd64-v1.27.3", "packages")

	binDir := t.TempDir()
	writeFakeDockerLog(t, binDir)
	binPath := filepath.Join(binDir, "docker")

	res, err := Fetch(context.Background(), Options{
		OutDir:     relOut,
		BuildImage: "ubuntu:22.04",
		PkgManager: "apt",
		K8sMinor:   "v1.27",
		Codename:   "jammy",
		AptOS:      "ubuntu",
		Pkgs:       []string{"kubeadm", "kubelet", "containerd.io", "conntrack"},
		DockerBin:  binPath,
	})
	if err != nil {
		t.Fatalf("Fetch 失败: %v", err)
	}
	if res.Skipped {
		t.Fatalf("期望正常下载，实际 skipped: %s", res.SkipReason)
	}
	if len(res.Files) != 3 {
		t.Fatalf("期望收集 3 个 .deb，实际 %d: %+v", len(res.Files), res.Files)
	}

	data, err := os.ReadFile(filepath.Join(binDir, "args.log"))
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
		t.Errorf("-v 挂载宿主机路径应为绝对路径，实际 %q（命令: %s）", hostDir, res.Command)
	}
	if strings.Contains(hostDir, "work/pixiu-offline-") && !filepath.IsAbs(hostDir) {
		t.Errorf("挂载路径仍为相对形式: %q", hostDir)
	}
}

func TestFetchCrictlMissingFallback(t *testing.T) {
	binDir := t.TempDir()
	binPath := filepath.Join(binDir, "docker")
	writeFakeDocker(t, binPath, true)

	tarContent := []byte("fake-crictl-tar-content")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1.27.1/crictl-v1.27.1-linux-amd64.tar.gz" {
			w.Write(tarContent)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	outDir := t.TempDir()
	res, err := Fetch(context.Background(), Options{
		OutDir:        outDir,
		BuildImage:    "ubuntu:22.04",
		PkgManager:    "apt",
		K8sMinor:      "v1.27",
		Codename:      "jammy",
		AptOS:         "ubuntu",
		Pkgs:          []string{"kubeadm", "kubelet", "containerd.io", "conntrack"},
		DockerBin:     binPath,
		CrictlVersion: "1.27.1",
		Arch:          "amd64",
		CrictlBaseURL: srv.URL,
	})
	if err != nil {
		t.Fatalf("Fetch 失败: %v", err)
	}
	if !res.CrictlMissing {
		t.Error("期望 CrictlMissing=true")
	}
	if res.CrictlFallbackFile == nil {
		t.Fatal("期望触发 crictl 静态回退")
	}
	if res.CrictlFallbackFile.Name != "crictl-v1.27.1-linux-amd64.tar.gz" {
		t.Errorf("回退文件名异常: %s", res.CrictlFallbackFile.Name)
	}
	tarPath := filepath.Join(outDir, "runtime", res.CrictlFallbackFile.Name)
	data, err := os.ReadFile(tarPath)
	if err != nil || string(data) != string(tarContent) {
		t.Errorf("crictl 回退文件内容异常: %v", err)
	}
	// 标记文件应被清理
	if _, err := os.Stat(filepath.Join(outDir, "cri-tools-missing")); err == nil {
		t.Error("cri-tools-missing 标记应被清理")
	}
}

func TestFetchCrictlMissingNoVersion(t *testing.T) {
	binDir := t.TempDir()
	binPath := filepath.Join(binDir, "docker")
	writeFakeDocker(t, binPath, true)

	outDir := t.TempDir()
	_, err := Fetch(context.Background(), Options{
		OutDir:     outDir,
		BuildImage: "ubuntu:22.04",
		PkgManager: "apt",
		K8sMinor:   "v1.27",
		Codename:   "jammy",
		Pkgs:       []string{"kubeadm"},
		DockerBin:  binPath,
	})
	if err == nil {
		t.Fatal("cri-tools 缺失且无版本时应报错")
	}
}

func TestCollectIgnoresNonPackages(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "runtime"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "a.deb"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(dir, "sub", "b.rpm"), []byte("y"), 0o644)
	os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("z"), 0o644)
	os.WriteFile(filepath.Join(dir, "cri-tools-missing"), []byte(""), 0o644)
	os.WriteFile(filepath.Join(dir, "runtime", "crictl.tar.gz"), []byte("t"), 0o644)

	files, err := Collect(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 {
		t.Errorf("期望只收集 2 个包文件（含递归子目录），实际 %d", len(files))
	}
	if files[0].RelPath != "a.deb" || files[1].RelPath != "sub/b.rpm" {
		t.Errorf("RelPath 异常: %+v", files)
	}
}
