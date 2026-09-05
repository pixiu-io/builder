package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"builder/internal/config"
	"builder/internal/ghupload"
)

const (
	defaultRainbowRepoURL = "https://github.com/caoyingjunz/rainbow.git"
	defaultRainbowRef     = "master"
)

// sync-client 子命令 flags
var (
	syncClientRepoURL string
	syncClientRef     string
	syncClientWorkDir string
	syncClientOutDir  string
)

// pixiuctlBuildTarget 单个交叉编译目标。
type pixiuctlBuildTarget struct {
	GOOS   string
	GOARCH string
}

// defaultPixiuctlTargets 与用户指定的 6 个平台一致。
var defaultPixiuctlTargets = []pixiuctlBuildTarget{
	{GOOS: "linux", GOARCH: "amd64"},
	{GOOS: "linux", GOARCH: "arm64"},
	{GOOS: "windows", GOARCH: "amd64"},
	{GOOS: "windows", GOARCH: "arm64"},
	{GOOS: "darwin", GOARCH: "amd64"},
	{GOOS: "darwin", GOARCH: "arm64"},
}

func newSyncClientCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "sync-client",
		Short: "拉取 rainbow、交叉编译 pixiuctl 并上传到 GitHub Release",
		Long: `从 GitHub 拉取 caoyingjunz/rainbow 源码，执行 pixiuctl version 读取版本号，
交叉编译多平台 pixiuctl 二进制（pixiuctl-{version}-{os}-{arch}），并上传到目标仓库 Release。

默认 Release tag 为 pixiuctl-{version}（如 pixiuctl-0.2.4），可用 --github-tag 覆盖。`,
		Example: `  builder sync-client --github-owner acme --github-repo builder
  go run cmd/builder/main.go sync-client \
    --configFile builder.yaml \
    --github-owner acme --github-repo builder --github-token "$TOKEN"`,
		RunE: runSyncClient,
	}
	cmd.Flags().StringVar(&syncClientRepoURL, "repo-url", defaultRainbowRepoURL, "rainbow 仓库 URL")
	cmd.Flags().StringVar(&syncClientRef, "ref", defaultRainbowRef, "git 分支或 tag")
	cmd.Flags().StringVar(&syncClientWorkDir, "workdir", "./work/rainbow-src", "rainbow 源码工作目录")
	cmd.Flags().StringVar(&syncClientOutDir, "out-dir", "./dist", "pixiuctl 二进制输出目录")
	addGitHubFlags(cmd)
	return cmd
}

func runSyncClient(cmd *cobra.Command, args []string) error {
	cfg, err := config.Load(configFile)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	fmt.Printf("拉取 rainbow: %s@%s → %s\n", syncClientRepoURL, syncClientRef, syncClientWorkDir)
	if err := fetchRainbowRepo(ctx, syncClientRepoURL, syncClientRef, syncClientWorkDir); err != nil {
		return err
	}

	fmt.Println("读取 pixiuctl 版本 ...")
	version, err := readPixiuctlVersion(ctx, syncClientWorkDir)
	if err != nil {
		return err
	}
	fmt.Printf("  version=%s\n", version)

	tag := pixiuctlGitHubTag(version)
	opts := mergeGitHubOptions(cfg.GitHub, githubOwner, githubRepo, tag, githubToken)
	if err := opts.Validate(); err != nil {
		return err
	}

	if err := os.MkdirAll(syncClientOutDir, 0o755); err != nil {
		return fmt.Errorf("创建输出目录失败 %s: %w", syncClientOutDir, err)
	}

	var files []string
	for _, t := range defaultPixiuctlTargets {
		name := pixiuctlBinaryAssetName(version, t.GOOS, t.GOARCH)
		out := filepath.Join(syncClientOutDir, name)
		fmt.Printf("编译 pixiuctl (%s/%s) → %s\n", t.GOOS, t.GOARCH, out)
		if err := buildPixiuctlBinary(ctx, syncClientWorkDir, out, t.GOOS, t.GOARCH); err != nil {
			return err
		}
		files = append(files, out)
	}

	fmt.Printf("确保 GitHub Release 存在: %s/%s@%s\n", opts.Owner, opts.Repo, opts.Tag)
	if err := ghupload.EnsureRelease(ctx, opts); err != nil {
		return err
	}

	fmt.Printf("上传到 GitHub Release %s/%s@%s ...\n", opts.Owner, opts.Repo, opts.Tag)
	res, err := ghupload.UploadFiles(ctx, opts, files)
	if err != nil {
		return err
	}
	for _, u := range res {
		fmt.Printf("  - %s → %s\n", u.LocalPath, u.BrowserURL)
	}
	return nil
}

// fetchRainbowRepo shallow clone 或在已有目录上 fetch+checkout。
func fetchRainbowRepo(ctx context.Context, repoURL, ref, dest string) error {
	gitDir := filepath.Join(dest, ".git")
	if st, err := os.Stat(gitDir); err == nil && st.IsDir() {
		cmd := exec.CommandContext(ctx, "git", "-C", dest, "fetch", "--depth", "1", "origin", ref)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("git fetch 失败: %w", err)
		}
		cmd = exec.CommandContext(ctx, "git", "-C", dest, "checkout", "-f", "FETCH_HEAD")
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("git checkout 失败: %w", err)
		}
		return nil
	}

	if err := os.RemoveAll(dest); err != nil {
		return fmt.Errorf("清理工作目录失败 %s: %w", dest, err)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "git", "clone", "--depth", "1", "--branch", ref, repoURL, dest)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git clone 失败: %w", err)
	}
	return nil
}

// readPixiuctlVersion 在 rainbow 源码目录执行 go run cmd/pixiuctl.go version。
func readPixiuctlVersion(ctx context.Context, rainbowRoot string) (string, error) {
	cmd := exec.CommandContext(ctx, "go", "run", "cmd/pixiuctl.go", "version")
	cmd.Dir = rainbowRoot
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("执行 pixiuctl version 失败: %w\n%s", err, stderr.String())
	}
	return parsePixiuctlVersion(stdout.String())
}

// parsePixiuctlVersion 解析 "pixiuctl version 0.2.4" → "0.2.4"。
func parsePixiuctlVersion(out string) (string, error) {
	line := strings.TrimSpace(out)
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = strings.TrimSpace(line[:i])
	}
	fields := strings.Fields(line)
	if len(fields) >= 3 && fields[0] == "pixiuctl" && fields[1] == "version" {
		v := strings.TrimSpace(fields[2])
		if v == "" {
			return "", fmt.Errorf("pixiuctl version 输出版本号为空: %q", out)
		}
		return v, nil
	}
	return "", fmt.Errorf("无法解析 pixiuctl version 输出: %q（期望形如 \"pixiuctl version 0.2.4\"）", out)
}

// pixiuctlGitHubTag 默认 pixiuctl-{version}；--github-tag 非空时覆盖。
func pixiuctlGitHubTag(version string) string {
	if tag := strings.TrimSpace(githubTag); tag != "" {
		return tag
	}
	return "pixiuctl-" + version
}

func pixiuctlBinaryAssetName(version, goos, goarch string) string {
	return fmt.Sprintf("pixiuctl-%s-%s-%s", version, goos, goarch)
}

func buildPixiuctlBinary(ctx context.Context, rainbowRoot, outPath, goos, goarch string) error {
	cmd := exec.CommandContext(ctx, "go", "build", "-o", outPath, "cmd/pixiuctl.go")
	cmd.Dir = rainbowRoot
	cmd.Env = append(os.Environ(),
		"CGO_ENABLED=0",
		"GOOS="+goos,
		"GOARCH="+goarch,
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("go build pixiuctl (%s/%s) 失败: %w", goos, goarch, err)
	}
	if err := os.Chmod(outPath, 0o755); err != nil {
		return fmt.Errorf("设置可执行权限失败 %s: %w", outPath, err)
	}
	return nil
}
