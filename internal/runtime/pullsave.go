package runtime

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
)

// SaveJob 单个 pull + 导出为 docker-save tar 的任务。
type SaveJob struct {
	Name    string // tar 文件名（无后缀）
	Image   string // 完整镜像引用
	SubDir  string // core / addons
	OutRoot string // images 输出根目录
	// Arch 目标架构（amd64/arm64）；containerd 模式下按平台拉取 multi-arch 清单。
	Arch string
}

// PullAndSave 按 runtime 拉取并保存为 docker-save 格式 tar（兼容 serve / docker load）。
// docker：在 PackImage 容器内 docker pull/save（挂 sock）。
// containerd：用 go-containerregistry 直拉 registry 并写成 docker-save（不依赖 ctr export，
// 避免旧版 containerd / 不完整 OCI export 导致缺 blob）。
func PullAndSave(ctx context.Context, c Config, jobs []SaveJob, verbose bool) error {
	if len(jobs) == 0 {
		return nil
	}
	cfg, err := c.withDefaults()
	if err != nil {
		return err
	}
	switch cfg.Runtime {
	case Docker:
		return dockerPullAndSave(ctx, cfg, jobs, verbose)
	case Containerd:
		return remotePullAndSave(ctx, jobs, verbose)
	default:
		return fmt.Errorf("未知 runtime %s", cfg.Runtime)
	}
}

func dockerPullAndSave(ctx context.Context, cfg Config, jobs []SaveJob, verbose bool) error {
	if cfg.PackImage == "" {
		return fmt.Errorf("docker runtime 需要 PackImage")
	}
	outRoot := jobs[0].OutRoot
	var b strings.Builder
	b.WriteString("set -e\n")
	b.WriteString("mkdir -p /out/core /out/addons\n")
	total := len(jobs)
	for i, j := range jobs {
		img := shellSingleQuote(j.Image)
		tarPath := shellSingleQuote("/out/" + j.SubDir + "/" + j.Name + ".tar")
		b.WriteString(fmt.Sprintf("echo \"[images] %d/%d pull %s\"\n", i+1, total, j.Image))
		b.WriteString("docker pull " + img + "\n")
		b.WriteString(fmt.Sprintf("echo \"[images] %d/%d save %s\"\n", i+1, total, j.Image))
		b.WriteString("docker save -o " + tarPath + " " + img + "\n")
	}
	name := fmt.Sprintf("builder-images-%d", os.Getpid())
	opts := RunOpts{
		Image: cfg.PackImage,
		Name:  name,
		Binds: []string{
			cfg.DockerSock + ":/var/run/docker.sock",
			outRoot + ":/out",
		},
		Shell: b.String(),
	}
	if verbose {
		fmt.Printf("  [images] 批量拉取并保存 %d 个镜像（容器 %s 内 docker pull + save）...\n", len(jobs), cfg.PackImage)
		_, err := dockerRunStreaming(ctx, cfg, opts, name, os.Stdout, os.Stderr)
		return err
	}
	_, err := dockerRun(ctx, cfg, opts, name)
	return err
}

func remotePullAndSave(ctx context.Context, jobs []SaveJob, verbose bool) error {
	total := len(jobs)
	for i, j := range jobs {
		tarPath := filepath.Join(j.OutRoot, j.SubDir, j.Name+".tar")
		if err := os.MkdirAll(filepath.Dir(tarPath), 0o755); err != nil {
			return err
		}
		if verbose {
			fmt.Printf("  [images] %d/%d pull+save %s → %s\n", i+1, total, j.Image, tarPath)
		}
		if err := pullImageToDockerTar(ctx, j.Image, tarPath, j.Arch); err != nil {
			return fmt.Errorf("拉取并保存 %s 失败: %w", j.Image, err)
		}
	}
	if verbose {
		fmt.Printf("  [images] 镜像 pull/save 完成：%d 个 tar\n", total)
	}
	return nil
}

func pullImageToDockerTar(ctx context.Context, imageRef, dockerTar, arch string) error {
	ref, err := name.ParseReference(imageRef, name.WeakValidation)
	if err != nil {
		return fmt.Errorf("解析镜像引用 %s: %w", imageRef, err)
	}
	opts := []remote.Option{remote.WithContext(ctx)}
	if a := normalizePullArch(arch); a != "" {
		opts = append(opts, remote.WithPlatform(v1.Platform{OS: "linux", Architecture: a}))
	}
	img, err := remote.Image(ref, opts...)
	if err != nil {
		return err
	}
	f, err := os.Create(dockerTar)
	if err != nil {
		return err
	}
	defer f.Close()
	return tarball.Write(ref, img, f)
}

func normalizePullArch(arch string) string {
	switch strings.ToLower(strings.TrimSpace(arch)) {
	case "", "amd64", "x86_64":
		if arch == "" {
			return ""
		}
		return "amd64"
	case "arm64", "aarch64":
		return "arm64"
	default:
		return strings.ToLower(strings.TrimSpace(arch))
	}
}

func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
