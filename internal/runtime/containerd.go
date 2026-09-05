package runtime

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

func ctrBaseArgs(cfg Config) []string {
	return []string{"--address", cfg.ContainerdAddress, "--namespace", cfg.Namespace}
}

func ctrAvailable(bin, address, namespace string) (bool, string) {
	args := []string{"--address", address, "--namespace", namespace, "version"}
	cmd := exec.Command(bin, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return false, fmt.Sprintf("containerd/ctr 不可用: %s（请确认 containerd 已启动且已安装 ctr；或改用 --runtime docker）", msg)
	}
	return true, ""
}

func parseBind(bind string) (src, dst, options string, err error) {
	parts := strings.Split(bind, ":")
	if len(parts) < 2 {
		return "", "", "", fmt.Errorf("无效 bind %q（期望 src:dst[:ro]）", bind)
	}
	src, dst = parts[0], parts[1]
	options = "rbind:rw"
	if len(parts) >= 3 {
		mode := parts[2]
		if mode == "ro" || strings.Contains(mode, "ro") {
			options = "rbind:ro"
		}
	}
	return src, dst, options, nil
}

func ctrMountFlag(bind string) (string, error) {
	src, dst, options, err := parseBind(bind)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("type=bind,src=%s,dst=%s,options=%s", src, dst, options), nil
}

func ctrRunArgs(cfg Config, opts RunOpts, name string) ([]string, error) {
	args := append([]string{}, ctrBaseArgs(cfg)...)
	args = append(args, "run", "--rm")
	if opts.NetworkHost {
		args = append(args, "--net-host")
	}
	for _, b := range opts.Binds {
		m, err := ctrMountFlag(b)
		if err != nil {
			return nil, err
		}
		args = append(args, "--mount", m)
	}
	if opts.Entrypoint != "" {
		args = append(args, "--entrypoint", opts.Entrypoint)
	}
	args = append(args, opts.Image, name)
	if opts.Shell != "" {
		args = append(args, "sh", "-c", opts.Shell)
	} else {
		args = append(args, opts.Args...)
	}
	return args, nil
}

func ctrEnsureImage(ctx context.Context, cfg Config, image string) error {
	// 始终 pull：已存在时 ctr 会快速完成 / 复用本地内容。
	pull := exec.CommandContext(ctx, cfg.CtrBin, append(ctrBaseArgs(cfg), "images", "pull", image)...)
	out, err := pull.CombinedOutput()
	if err != nil {
		return fmt.Errorf("ctr images pull %s 失败: %v\n输出: %s", image, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func ctrRun(ctx context.Context, cfg Config, opts RunOpts, name string) (string, error) {
	if err := ctrEnsureImage(ctx, cfg, opts.Image); err != nil {
		return "", err
	}
	args, err := ctrRunArgs(cfg, opts, name)
	if err != nil {
		return "", err
	}
	cmd := exec.CommandContext(ctx, cfg.CtrBin, args...)
	desc := cfg.CtrBin + " " + strings.Join(args, " ")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return desc, fmt.Errorf("%w\n输出: %s", err, strings.TrimSpace(string(out)))
	}
	return desc, nil
}

func ctrRunCombined(ctx context.Context, cfg Config, opts RunOpts, name string) (string, []byte, error) {
	if err := ctrEnsureImage(ctx, cfg, opts.Image); err != nil {
		return "", nil, err
	}
	args, err := ctrRunArgs(cfg, opts, name)
	if err != nil {
		return "", nil, err
	}
	cmd := exec.CommandContext(ctx, cfg.CtrBin, args...)
	desc := cfg.CtrBin + " " + strings.Join(args, " ")
	out, err := cmd.CombinedOutput()
	return desc, out, err
}
