// Package packages 在目标系统容器内通过包管理器（apt/dnf）递归下载
// k8s 组件、容器运行时与系统依赖包，并生成对应的软件源（pkgs.k8s.io / download.docker.com）。
// 依赖闭包交由包管理器处理；docker 不可用时该步骤标记为 skipped。
package packages

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Options 软件包下载配置。
type Options struct {
	// OutDir 目标目录（bundle 内 packages/），容器内以只读外挂形式挂载到 /out。
	OutDir string
	// BuildImage 容器镜像（如 ubuntu:22.04）。
	BuildImage string
	// PkgManager 包管理器：apt 或 dnf。
	PkgManager string
	// K8sMinor k8s 大版本（v1.27），用于 pkgs.k8s.io 源。
	K8sMinor string
	// Codename apt 版本代号（jammy/bookworm 等），用于 containerd 源。
	Codename string
	// RPMDistro dnf 发行版标识（rhel9/rhel7 等），用于 containerd 源。
	RPMDistro string
	// AptOS apt 发行版家族（ubuntu/debian），用于 containerd 源。
	AptOS string
	// Pkgs 待下载软件包清单（k8s + 运行时 + 系统依赖）。
	Pkgs []string
	// DockerBin docker 命令路径，默认 "docker"。
	DockerBin string
	// DryRun 只打印命令不执行（测试用）。
	DryRun bool
	// CrictlVersion cri-tools 包不可用时，回退下载的 crictl 版本（如 1.27.1）。
	CrictlVersion string
	// Arch 目标架构（amd64/arm64），用于 crictl 回退 tar。
	Arch string
	// CrictlBaseURL 测试注入：crictl 回退下载基地址，默认 GitHub release。
	CrictlBaseURL string
}

// FileInfo 单个软件包文件信息。
type FileInfo struct {
	Path    string `json:"path"`     // 绝对路径
	RelPath string `json:"rel_path"` // 相对 OutDir 的路径（/ 分隔）
	Name    string `json:"name"`
	Size    int64  `json:"size"`
	SHA256  string `json:"sha256"`
}

// Result 软件包下载结果。
type Result struct {
	Files      []FileInfo
	Skipped    bool
	SkipReason string
	Command    string
	DryRun     bool
	// CrictlMissing cri-tools 包在源中不存在，已触发 crictl 静态 tar 回退。
	CrictlMissing bool
	// CrictlFallbackFile 回退下载的 crictl tar 文件（存在时非空）。
	CrictlFallbackFile *FileInfo
}

// DockerAvailable 检查 docker 是否可用，返回 (可用, 提示信息)。
func DockerAvailable(bin string) (bool, string) {
	if bin == "" {
		bin = "docker"
	}
	cmd := exec.Command(bin, "info")
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return false, fmt.Sprintf("docker 不可用: %s（此步骤将被跳过，离线包不包含软件包）", msg)
	}
	return true, ""
}

// containerNamePackages 软件包下载容器名前缀（docker run --name），便于 docker ps 区分阶段。
const containerNamePackages = "builder-packages"

// uniqueContainerName 生成带阶段标识的唯一容器名。
func uniqueContainerName(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
}

// DownloadScriptOpts 容器内下载脚本构造参数。
type DownloadScriptOpts struct {
	PkgManager string
	Repos      []Repo
	Pkgs       []string
	// ArchiveDir 容器内包归档目录，默认 /out。
	ArchiveDir string
	// CheckCrictl 是否在脚本内检测 cri-tools 包可用性并写缺失标记。
	CheckCrictl bool
}

// BuildDownloadScript 构造容器内完整下载脚本：
// 配置源（k8s + containerd）→ 更新缓存 → 递归下载 → 依赖闭包验证 → cri-tools 检测。
func BuildDownloadScript(opts DownloadScriptOpts) string {
	if opts.ArchiveDir == "" {
		opts.ArchiveDir = "/out"
	}
	pkgs := strings.Join(opts.Pkgs, " ")
	var b strings.Builder
	switch opts.PkgManager {
	case "dnf":
		b.WriteString("set -e\n")
		b.WriteString(DnfSourceScript(opts.Repos))
		b.WriteString("dnf makecache\n")
		b.WriteString("dnf -y install dnf-plugins-core\n")
		b.WriteString("mkdir -p " + opts.ArchiveDir + "\n")
		b.WriteString(fmt.Sprintf("if dnf install -y --downloadonly --downloaddir=%s %s 2>/dev/null; then :\n", opts.ArchiveDir, pkgs))
		b.WriteString(fmt.Sprintf("else\n  dnf download --resolve --destdir=%s %s\nfi\n", opts.ArchiveDir, pkgs))
		// 依赖闭包验证：--assumeno 模拟安装，返回 0 表示闭包完整
		b.WriteString(fmt.Sprintf("dnf install --assumeno %s >/dev/null\n", pkgs))
		if opts.CheckCrictl {
			b.WriteString("if ! dnf list --available cri-tools >/dev/null 2>&1; then touch /out/cri-tools-missing; fi\n")
		}
	default: // apt
		b.WriteString("set -e\nexport DEBIAN_FRONTEND=noninteractive\n")
		b.WriteString("apt-get update\n")
		b.WriteString("apt-get install -y --no-install-recommends curl ca-certificates gnupg apt-transport-https\n")
		b.WriteString(AptSourceScript(opts.Repos))
		b.WriteString("apt-get update\n")
		b.WriteString("mkdir -p " + opts.ArchiveDir + "\n")
		b.WriteString(fmt.Sprintf("apt-get install -y --download-only --no-install-recommends %s -o Dir::Cache::archives=%s\n", pkgs, opts.ArchiveDir))
		// 依赖闭包验证：--dry-run 模拟解析，返回 0 表示依赖完整
		b.WriteString(fmt.Sprintf("apt-get install --dry-run %s >/dev/null\n", pkgs))
		if opts.CheckCrictl {
			b.WriteString("if ! apt-cache policy cri-tools 2>/dev/null | grep -Eq 'Candidate: [0-9]'; then touch /out/cri-tools-missing; fi\n")
		}
	}
	return b.String()
}

// Fetch 在容器内下载软件包并收集到 OutDir 下。
// docker 不可用时返回 skipped 结果而非错误。
// 若容器内检测到 cri-tools 包不可用，则回退下载 crictl 静态 tar 到 OutDir/runtime/。
func Fetch(ctx context.Context, opts Options) (*Result, error) {
	if opts.DockerBin == "" {
		opts.DockerBin = "docker"
	}
	if opts.OutDir == "" {
		return nil, fmt.Errorf("packages: OutDir 不能为空")
	}
	// 防御性加固：docker -v 挂载宿主机目录必须是绝对路径，即使 Fetch 被独立调用
	// （绕过 builder.Build 的路径归一化）也能保证挂载正确。
	if abs, err := filepath.Abs(opts.OutDir); err != nil {
		return nil, fmt.Errorf("packages: 解析 OutDir 绝对路径失败: %w", err)
	} else {
		opts.OutDir = abs
	}
	if opts.BuildImage == "" {
		return nil, fmt.Errorf("packages: BuildImage 不能为空")
	}
	if len(opts.Pkgs) == 0 {
		return nil, fmt.Errorf("packages: 软件包清单为空")
	}

	repos := append(K8sRepos(opts.K8sMinor), ContainerdRepos(opts.AptOS, opts.Codename, opts.RPMDistro)...)
	script := BuildDownloadScript(DownloadScriptOpts{
		PkgManager:  opts.PkgManager,
		Repos:       repos,
		Pkgs:        opts.Pkgs,
		ArchiveDir:  "/out",
		CheckCrictl: true,
	})
	cmdArgs := []string{
		"run", "--rm",
		"--name", uniqueContainerName(containerNamePackages),
		"-v", opts.OutDir + ":/out",
		opts.BuildImage,
		"sh", "-c", script,
	}
	cmd := exec.CommandContext(ctx, opts.DockerBin, cmdArgs...)

	res := &Result{Command: opts.DockerBin + " " + strings.Join(cmdArgs, " ")}
	if opts.DryRun {
		res.DryRun = true
		return res, nil
	}

	// 检查 docker 可用性
	if ok, reason := DockerAvailable(opts.DockerBin); !ok {
		return &Result{Skipped: true, SkipReason: reason}, nil
	}

	out, err := cmd.CombinedOutput()
	if err != nil {
		return res, fmt.Errorf("docker 下载软件包失败: %v\n命令: %s\n输出: %s", err, res.Command, strings.TrimSpace(string(out)))
	}

	// cri-tools 缺失标记 → crictl 静态 tar 回退
	marker := filepath.Join(opts.OutDir, "cri-tools-missing")
	if _, statErr := os.Stat(marker); statErr == nil {
		res.CrictlMissing = true
		_ = os.Remove(marker)
		if opts.CrictlVersion == "" {
			return res, fmt.Errorf("cri-tools 包在源中不存在且 CrictlVersion 为空，无法回退下载 crictl")
		}
		rtDir := filepath.Join(opts.OutDir, "runtime")
		fi, err := FetchCrictlFallback(ctx, opts.CrictlVersion, opts.Arch, rtDir, opts.CrictlBaseURL)
		if err != nil {
			return res, fmt.Errorf("cri-tools 包不可用，crictl 静态回退下载失败: %w", err)
		}
		res.CrictlFallbackFile = &fi
		res.Files = append(res.Files, fi)
	}

	files, err := Collect(opts.OutDir)
	if err != nil {
		return res, err
	}
	if len(files) == 0 {
		return res, fmt.Errorf("docker 下载完成但 %s 下没有 .deb/.rpm 文件（检查软件包源是否可用）", opts.OutDir)
	}
	res.Files = files
	return res, nil
}

// Collect 递归收集目录下所有 .deb/.rpm 文件并计算 size/sha256。
func Collect(root string) ([]FileInfo, error) {
	var files []FileInfo
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !isPackageFile(info.Name()) {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		sum, sumErr := fileSHA256(path)
		if sumErr != nil {
			return fmt.Errorf("计算 %s sha256 失败: %w", path, sumErr)
		}
		files = append(files, FileInfo{
			Path:    path,
			RelPath: filepath.ToSlash(rel),
			Name:    info.Name(),
			Size:    info.Size(),
			SHA256:  sum,
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(files, func(i, j int) bool { return files[i].RelPath < files[j].RelPath })
	return files, nil
}

func isPackageFile(name string) bool {
	return strings.HasSuffix(name, ".deb") || strings.HasSuffix(name, ".rpm")
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
