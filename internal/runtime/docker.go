package runtime

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

func dockerAvailable(bin string) (bool, string) {
	cmd := exec.Command(bin, "info")
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return false, fmt.Sprintf("docker 不可用: %s", msg)
	}
	return true, ""
}

// RunOpts 启动一次性容器执行命令。
type RunOpts struct {
	Image string
	// Name 容器名（docker --name / ctr 任务 id）；空则自动生成。
	Name string
	// NamePrefix 自动生成名的前缀。
	NamePrefix string
	// Binds docker 风格 src:dst[:ro]；containerd 转为 --mount。
	Binds []string
	// NetworkHost containerd 下建议 true（ctr 默认无网络）。
	NetworkHost bool
	// Entrypoint 覆盖镜像入口（如 /kubeadm）；空则用镜像默认。
	Entrypoint string
	// Args 入口参数；与 Shell 互斥（Shell 非空时忽略 Args，改为 sh -c Shell）。
	Args []string
	// Shell 非空时以 sh -c Shell 运行。
	Shell string
}

// Run 用当前 runtime 拉起一次性容器。
func Run(ctx context.Context, c Config, opts RunOpts) (command string, err error) {
	cfg, err := c.withDefaults()
	if err != nil {
		return "", err
	}
	name := opts.Name
	if name == "" {
		prefix := opts.NamePrefix
		if prefix == "" {
			prefix = "builder"
		}
		name = fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
	}
	switch cfg.Runtime {
	case Docker:
		return dockerRun(ctx, cfg, opts, name)
	case Containerd:
		return ctrRun(ctx, cfg, opts, name)
	default:
		return "", fmt.Errorf("未知 runtime %s", cfg.Runtime)
	}
}

func dockerRun(ctx context.Context, cfg Config, opts RunOpts, name string) (string, error) {
	args := []string{"run", "--rm", "--name", name}
	for _, b := range opts.Binds {
		args = append(args, "-v", b)
	}
	if opts.Entrypoint != "" {
		args = append(args, "--entrypoint", opts.Entrypoint)
	}
	args = append(args, opts.Image)
	if opts.Shell != "" {
		args = append(args, "sh", "-c", opts.Shell)
	} else {
		args = append(args, opts.Args...)
	}
	cmd := exec.CommandContext(ctx, cfg.DockerBin, args...)
	desc := cfg.DockerBin + " " + strings.Join(args, " ")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return desc, fmt.Errorf("%w\n输出: %s", err, strings.TrimSpace(string(out)))
	}
	return desc, nil
}

func dockerRunStreaming(ctx context.Context, cfg Config, opts RunOpts, name string, stdout, stderr io.Writer) (string, error) {
	args := []string{"run", "--rm", "--name", name}
	for _, b := range opts.Binds {
		args = append(args, "-v", b)
	}
	if opts.Entrypoint != "" {
		args = append(args, "--entrypoint", opts.Entrypoint)
	}
	args = append(args, opts.Image)
	if opts.Shell != "" {
		args = append(args, "sh", "-c", opts.Shell)
	} else {
		args = append(args, opts.Args...)
	}
	cmd := exec.CommandContext(ctx, cfg.DockerBin, args...)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	desc := cfg.DockerBin + " " + strings.Join(args, " ")
	if err := cmd.Run(); err != nil {
		return desc, err
	}
	return desc, nil
}

// RunCombined 执行 Run，返回合并输出（docker 非 streaming 路径已含在 error 中；此处供需要 stdout 的调用方）。
func RunCombined(ctx context.Context, c Config, opts RunOpts) (command string, output []byte, err error) {
	cfg, err := c.withDefaults()
	if err != nil {
		return "", nil, err
	}
	name := opts.Name
	if name == "" {
		prefix := opts.NamePrefix
		if prefix == "" {
			prefix = "builder"
		}
		name = fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
	}
	switch cfg.Runtime {
	case Docker:
		args := []string{"run", "--rm", "--name", name}
		for _, b := range opts.Binds {
			args = append(args, "-v", b)
		}
		if opts.Entrypoint != "" {
			args = append(args, "--entrypoint", opts.Entrypoint)
		}
		args = append(args, opts.Image)
		if opts.Shell != "" {
			args = append(args, "sh", "-c", opts.Shell)
		} else {
			args = append(args, opts.Args...)
		}
		cmd := exec.CommandContext(ctx, cfg.DockerBin, args...)
		desc := cfg.DockerBin + " " + strings.Join(args, " ")
		out, err := cmd.CombinedOutput()
		return desc, out, err
	case Containerd:
		desc, out, err := ctrRunCombined(ctx, cfg, opts, name)
		return desc, out, err
	default:
		return "", nil, fmt.Errorf("未知 runtime %s", cfg.Runtime)
	}
}

// RemoveImages 清理中间镜像（docker rmi / ctr images rm）。
func RemoveImages(c Config, images []string, out io.Writer) int {
	cfg, err := c.withDefaults()
	if err != nil || len(images) == 0 {
		return 0
	}
	removed := 0
	for _, img := range images {
		img = strings.TrimSpace(img)
		if img == "" {
			continue
		}
		var cmd *exec.Cmd
		switch cfg.Runtime {
		case Docker:
			cmd = exec.Command(cfg.DockerBin, "rmi", img)
		case Containerd:
			// images 由 registry 直拉写成 docker-save，未进入 containerd 本地 store。
			continue
		default:
			continue
		}
		if err := cmd.Run(); err != nil {
			if out != nil {
				fmt.Fprintf(out, "[清理] 删除镜像 %s 失败: %v\n", img, err)
			}
		} else {
			removed++
		}
	}
	if out != nil && removed > 0 {
		fmt.Fprintf(out, "[清理] 已删除 %d 个中间镜像（--keep-files 可保留）\n", removed)
	}
	return removed
}

// EnsureDir 确保目录存在（供调用方使用）。
func EnsureDir(path string) error {
	return os.MkdirAll(path, 0o755)
}
