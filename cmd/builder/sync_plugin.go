package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
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
	defaultPluginVersion    = "v0.0.1"
	pluginConfigFileContent = `default:
    push_kubernetes: false
    push_images: false
kubernetes:
    version: ""
plugin:
    callback: 127.0.0.1:8090
registry:
    repository: test.io
    namespace: test
    username: test
    password: test
images:
    - name: nginx
      id: 12345678
      path: docker.io/nginx
      tags:
        - 1.19.1
`
	pluginReadmeContent = "plugin\n"
)

// sync plugin 子命令 flags
var (
	syncPluginRepoURL string
	syncPluginRef     string
	syncPluginWorkDir string
	syncPluginOutDir  string
	syncPluginVersion string
	syncPluginOS      string
	syncPluginArch    string
)

func newSyncPluginCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "plugin",
		Short: "拉取 rainbow、编译 plugin 子命令并打包上传到 GitHub Release",
		Long: `从 GitHub 拉取 caoyingjunz/rainbow 源码，交叉编译 cmd/plugin 为二进制（plugin），
生成固定内容的 config.yaml 与 README.md（内容为 plugin），
打包为 plugin-<version>.tar.gz 并上传到目标仓库 Release。

version 由 --version 指定，默认 v0.0.1；产物名与默认 Release tag（plugin-{version}）均使用它，
--github-tag 可覆盖 Release tag。`,
		Example: `  builder sync plugin --github-owner acme --github-repo builder
  builder sync plugin --version v0.2.0 --github-owner acme --github-repo builder
  go run ./cmd/builder sync plugin \
    --configFile builder.yaml \
    --github-owner acme --github-repo builder --github-token "$TOKEN"`,
		RunE: runSyncPlugin,
	}
	cmd.Flags().StringVar(&syncPluginRepoURL, "repo-url", defaultRainbowRepoURL, "rainbow 仓库 URL")
	cmd.Flags().StringVar(&syncPluginRef, "ref", defaultRainbowRef, "git 分支或 tag")
	cmd.Flags().StringVar(&gitRepoToken, "repo-token", "", "访问 rainbow 仓库的 token（私有仓库克隆用；默认复用 --github-token）")
	cmd.Flags().StringVar(&syncPluginWorkDir, "workdir", "./work/rainbow-src", "rainbow 源码工作目录")
	cmd.Flags().StringVar(&syncPluginOutDir, "out-dir", "./dist", "plugin 产物输出目录")
	cmd.Flags().StringVar(&syncPluginVersion, "version", defaultPluginVersion, "plugin 版本号（产物名 plugin-{version}.tar.gz 与默认 Release tag 使用）")
	cmd.Flags().StringVar(&syncPluginOS, "os", "linux", "目标操作系统")
	cmd.Flags().StringVar(&syncPluginArch, "arch", "amd64", "目标架构")
	addGitHubFlags(cmd)
	return cmd
}

func runSyncPlugin(cmd *cobra.Command, args []string) error {
	cfg, err := config.Load(configFile)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	fmt.Printf("拉取 rainbow: %s@%s → %s\n", syncPluginRepoURL, syncPluginRef, syncPluginWorkDir)
	if err := fetchRainbowRepo(ctx, syncPluginRepoURL, syncPluginRef, syncPluginWorkDir); err != nil {
		return err
	}

	if err := os.MkdirAll(syncPluginOutDir, 0o755); err != nil {
		return fmt.Errorf("创建输出目录失败 %s: %w", syncPluginOutDir, err)
	}
	outDirAbs, err := filepath.Abs(syncPluginOutDir)
	if err != nil {
		return fmt.Errorf("解析输出目录失败 %s: %w", syncPluginOutDir, err)
	}
	workDirAbs, err := filepath.Abs(syncPluginWorkDir)
	if err != nil {
		return fmt.Errorf("解析工作目录失败 %s: %w", syncPluginWorkDir, err)
	}

	// 1. 编译 plugin 子命令为二进制（固定名 plugin）
	binaryPath := filepath.Join(outDirAbs, "plugin")
	fmt.Printf("编译 plugin (%s/%s) → %s\n", syncPluginOS, syncPluginArch, binaryPath)
	if err := buildPluginBinary(ctx, workDirAbs, binaryPath, syncPluginOS, syncPluginArch); err != nil {
		return err
	}

	// 2. 生成固定内容的 config.yaml 与 README.md
	configPath := filepath.Join(outDirAbs, "config.yaml")
	readmePath := filepath.Join(outDirAbs, "README.md")
	if err := os.WriteFile(configPath, []byte(pluginConfigFileContent), 0o644); err != nil {
		return fmt.Errorf("写入 config.yaml 失败: %w", err)
	}
	if err := os.WriteFile(readmePath, []byte(pluginReadmeContent), 0o644); err != nil {
		return fmt.Errorf("写入 README.md 失败: %w", err)
	}

	// 3. 打包 plugin-<version>.tar.gz（顶层三个文件）
	tarName := fmt.Sprintf("plugin-%s.tar.gz", syncPluginVersion)
	tarPath := filepath.Join(outDirAbs, tarName)
	fmt.Printf("打包 %s（plugin + config.yaml + README.md）\n", tarName)
	if err := packFilesTarGz(tarPath, [][2]string{
		{"plugin", binaryPath},
		{"config.yaml", configPath},
		{"README.md", readmePath},
	}); err != nil {
		return err
	}

	// 4. 上传到 GitHub Release
	tag := pluginGitHubTag(syncPluginVersion)
	opts := mergeGitHubOptions(cfg.GitHub, githubOwner, githubRepo, tag, githubToken)
	if err := opts.Validate(); err != nil {
		return err
	}
	fmt.Printf("确保 GitHub Release 存在: %s/%s@%s\n", opts.Owner, opts.Repo, opts.Tag)
	if err := ghupload.EnsureRelease(ctx, opts); err != nil {
		return err
	}
	fmt.Printf("上传到 GitHub Release %s/%s@%s ...\n", opts.Owner, opts.Repo, opts.Tag)
	res, err := ghupload.UploadFiles(ctx, opts, []string{tarPath})
	if err != nil {
		return err
	}
	for _, u := range res {
		fmt.Printf("  - %s → %s\n", u.LocalPath, u.BrowserURL)
	}
	return nil
}

// buildPluginBinary 在 rainbow 源码目录交叉编译 cmd/plugin。
func buildPluginBinary(ctx context.Context, rainbowRoot, outPath, goos, goarch string) error {
	if !filepath.IsAbs(outPath) {
		abs, err := filepath.Abs(outPath)
		if err != nil {
			return fmt.Errorf("解析输出路径失败 %s: %w", outPath, err)
		}
		outPath = abs
	}
	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		return fmt.Errorf("创建输出目录失败 %s: %w", filepath.Dir(outPath), err)
	}
	c := exec.CommandContext(ctx, "go", "build", "-o", outPath, "./cmd/plugin")
	c.Dir = rainbowRoot
	c.Env = append(os.Environ(),
		"CGO_ENABLED=0",
		"GOOS="+goos,
		"GOARCH="+goarch,
	)
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	if err := c.Run(); err != nil {
		return fmt.Errorf("go build plugin (%s/%s) 失败: %w", goos, goarch, err)
	}
	if err := os.Chmod(outPath, 0o755); err != nil {
		return fmt.Errorf("设置可执行权限失败 %s: %w", outPath, err)
	}
	return nil
}

// pluginGitHubTag 默认 plugin-{version}；--github-tag 非空时覆盖。
func pluginGitHubTag(version string) string {
	if tag := strings.TrimSpace(githubTag); tag != "" {
		return tag
	}
	return "plugin-" + version
}

// packFilesTarGz 将若干文件按给定顶层名打包为 tar.gz（顺序固定，保证产物可复现）。
func packFilesTarGz(tarPath string, files [][2]string) error {
	out, err := os.Create(tarPath)
	if err != nil {
		return fmt.Errorf("创建 tar.gz 失败: %w", err)
	}
	defer out.Close()

	gw := gzip.NewWriter(out)
	defer gw.Close()
	tw := tar.NewWriter(gw)
	defer tw.Close()

	for _, f := range files {
		name, src := f[0], f[1]
		info, err := os.Stat(src)
		if err != nil {
			return fmt.Errorf("读取打包文件失败 %s: %w", src, err)
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return fmt.Errorf("生成 tar 头失败 %s: %w", name, err)
		}
		hdr.Name = name
		if err := tw.WriteHeader(hdr); err != nil {
			return fmt.Errorf("写入 tar 头失败 %s: %w", name, err)
		}
		srcFile, err := os.Open(src)
		if err != nil {
			return fmt.Errorf("打开打包文件失败 %s: %w", src, err)
		}
		if _, err := io.Copy(tw, srcFile); err != nil {
			srcFile.Close()
			return fmt.Errorf("写入文件内容失败 %s: %w", name, err)
		}
		srcFile.Close()
	}
	return nil
}
