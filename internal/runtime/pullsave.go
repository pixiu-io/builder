package runtime

import (
	"archive/tar"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
)

// SaveJob 单个 pull + 导出为 docker-save tar 的任务。
type SaveJob struct {
	Name    string // tar 文件名（无后缀）
	Image   string // 完整镜像引用
	SubDir  string // core / addons
	OutRoot string // images 输出根目录
}

// PullAndSave 按 runtime 拉取并保存为 docker-save 格式 tar（兼容 serve / docker load）。
// docker：在 PackImage 容器内 docker pull/save（挂 sock）。
// containerd：宿主机 ctr images pull + export（OCI），再转为 docker-save。
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
		return ctrPullAndSave(ctx, cfg, jobs, verbose)
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

func ctrPullAndSave(ctx context.Context, cfg Config, jobs []SaveJob, verbose bool) error {
	total := len(jobs)
	for i, j := range jobs {
		if verbose {
			fmt.Printf("  [images] %d/%d ctr pull %s\n", i+1, total, j.Image)
		}
		if err := ctrEnsureImage(ctx, cfg, j.Image); err != nil {
			return err
		}
		tarPath := filepath.Join(j.OutRoot, j.SubDir, j.Name+".tar")
		if err := os.MkdirAll(filepath.Dir(tarPath), 0o755); err != nil {
			return err
		}
		ociTar, err := os.CreateTemp("", "builder-oci-*.tar")
		if err != nil {
			return err
		}
		ociPath := ociTar.Name()
		_ = ociTar.Close()
		defer os.Remove(ociPath)

		if verbose {
			fmt.Printf("  [images] %d/%d ctr export → docker-save %s\n", i+1, total, tarPath)
		}
		exportArgs := append(ctrBaseArgs(cfg), "images", "export", ociPath, j.Image)
		cmd := exec.CommandContext(ctx, cfg.CtrBin, exportArgs...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("ctr images export %s 失败: %v\n输出: %s", j.Image, err, strings.TrimSpace(string(out)))
		}
		if err := ociExportToDockerTar(ociPath, tarPath, j.Image); err != nil {
			return fmt.Errorf("转换 %s 为 docker-save 失败: %w", j.Image, err)
		}
	}
	if verbose {
		fmt.Printf("  [images] 镜像 pull/export 完成：%d 个 tar\n", total)
	}
	return nil
}

func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// ociExportToDockerTar 将 ctr images export 的 OCI layout tar 转为 docker save 格式。
func ociExportToDockerTar(ociTar, dockerTar, imageRef string) error {
	dir, err := os.MkdirTemp("", "builder-oci-layout-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	if err := untarFile(ociTar, dir); err != nil {
		return err
	}
	idx, err := layout.ImageIndexFromPath(dir)
	if err != nil {
		return fmt.Errorf("读取 OCI layout: %w", err)
	}
	img, err := firstImage(idx)
	if err != nil {
		return err
	}
	ref, err := name.ParseReference(imageRef, name.WeakValidation)
	if err != nil {
		return fmt.Errorf("解析镜像引用 %s: %w", imageRef, err)
	}
	f, err := os.Create(dockerTar)
	if err != nil {
		return err
	}
	defer f.Close()
	return tarball.Write(ref, img, f)
}

func firstImage(idx v1.ImageIndex) (v1.Image, error) {
	mf, err := idx.IndexManifest()
	if err != nil {
		return nil, err
	}
	for _, d := range mf.Manifests {
		if d.MediaType.IsIndex() {
			sub, err := idx.ImageIndex(d.Digest)
			if err != nil {
				continue
			}
			if img, err := firstImage(sub); err == nil {
				return img, nil
			}
			continue
		}
		if d.MediaType.IsImage() {
			return idx.Image(d.Digest)
		}
	}
	return nil, fmt.Errorf("OCI layout 中未找到镜像 manifest")
}

func untarFile(tarPath, dest string) error {
	f, err := os.Open(tarPath)
	if err != nil {
		return err
	}
	defer f.Close()
	tr := tar.NewReader(f)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		target := filepath.Join(dest, hdr.Name)
		if !strings.HasPrefix(filepath.Clean(target), filepath.Clean(dest)+string(os.PathSeparator)) &&
			filepath.Clean(target) != filepath.Clean(dest) {
			return fmt.Errorf("非法 tar 路径: %s", hdr.Name)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode))
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, tr); err != nil {
				out.Close()
				return err
			}
			out.Close()
		}
	}
	return nil
}
