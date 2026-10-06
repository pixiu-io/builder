package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"builder/internal/config"
	"builder/internal/ghupload"
)

const (
	defaultPixiuRepoURL = "https://github.com/caoyingjunz/pixiu.git"
	defaultPixiuRef     = "master"
	// deployAgentGOOS deploy-agent 固定目标系统（无 --os 参数）。
	deployAgentGOOS = "linux"
)

// sync deploy-agent 子命令 flags
var (
	syncDeployAgentRepoURL string
	syncDeployAgentRef     string
	syncDeployAgentWorkDir string
	syncDeployAgentOutDir  string
	syncDeployAgentArches  []string
)

func newSyncDeployAgentCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "deploy-agent",
		Short: "拉取 pixiu，交叉编译 deploy-agent（linux）并上传到 GitHub Release",
		Long: `从 GitHub 拉取 caoyingjunz/pixiu 源码（默认 ref=master），交叉编译 cmd/deploy-agent 为 linux 二进制，
版本号取 pixiu 的 git tag：--ref 本身是 tag 时直接使用，否则取远端全部 tag 中 semver 最大者；
产物为 pixiu-deploy-agent-{version}-{arch}（裸二进制，不打包），默认架构 amd64 + arm64。

默认 Release tag 为 deploy-agent-{version}（如 deploy-agent-v2.0.1），可用 --github-tag 覆盖。`,
		Example: `  builder sync deploy-agent --github-owner acme --github-repo builder
  go run ./cmd/builder sync deploy-agent \
    --configFile builder.yaml \
    --arch amd64 --arch arm64 \
    --github-owner acme --github-repo builder --github-tag deploy-agent --github-token "$TOKEN"`,
		RunE: runSyncDeployAgent,
	}
	cmd.Flags().StringVar(&syncDeployAgentRepoURL, "repo-url", defaultPixiuRepoURL, "pixiu 仓库 URL")
	cmd.Flags().StringVar(&syncDeployAgentRef, "ref", defaultPixiuRef, "git 分支或 tag")
	cmd.Flags().StringVar(&gitRepoToken, "hub-repo-token", "", "访问 pixiu 仓库的 token（私有仓库克隆用；默认复用 --github-token）")
	cmd.Flags().StringVar(&gitRepoToken, "repo-token", "", "已弃用，请使用 --hub-repo-token")
	_ = cmd.Flags().MarkHidden("repo-token")
	cmd.Flags().StringVar(&syncDeployAgentWorkDir, "workdir", "./work/pixiu-src", "pixiu 源码工作目录")
	cmd.Flags().StringVar(&syncDeployAgentOutDir, "out-dir", "./dist", "deploy-agent 产物输出目录")
	cmd.Flags().StringArrayVar(&syncDeployAgentArches, "arch", []string{"amd64", "arm64"}, "目标架构（可重复；默认 amd64 与 arm64）")
	addGitHubFlags(cmd)
	return cmd
}

func runSyncDeployAgent(cmd *cobra.Command, args []string) error {
	cfg, err := config.Load(configFile)
	if err != nil {
		return err
	}
	arches, err := normalizeSyncBuilderArches(syncDeployAgentArches)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	fmt.Printf("拉取 pixiu: %s@%s → %s\n", syncDeployAgentRepoURL, syncDeployAgentRef, syncDeployAgentWorkDir)
	// fetchRainbowRepo 实现与 rainbow 无关（shallow clone / fetch+checkout 的通用逻辑），此处直接复用于 pixiu。
	if err := fetchRainbowRepo(ctx, syncDeployAgentRepoURL, syncDeployAgentRef, syncDeployAgentWorkDir); err != nil {
		return err
	}

	fmt.Println("解析 pixiu 版本（git tag）...")
	version, err := readPixiuTagVersion(ctx, syncDeployAgentRepoURL, syncDeployAgentRef)
	if err != nil {
		return err
	}
	fmt.Printf("  version=%s\n", version)
	// commit hash 仅作追溯信息，读取失败不影响构建。
	if commit, err := gitShortHead(ctx, syncDeployAgentWorkDir); err == nil {
		fmt.Printf("  commit=%s\n", commit)
	} else {
		fmt.Printf("  commit=(读取失败: %v)\n", err)
	}

	tag := deployAgentGitHubTag(version)
	opts := mergeGitHubOptions(cfg.GitHub, githubOwner, githubRepo, tag, githubToken)
	// 先于编译校验 GitHub 配置，fail-fast 避免白编译。
	if err := opts.Validate(); err != nil {
		return err
	}

	if err := os.MkdirAll(syncDeployAgentOutDir, 0o755); err != nil {
		return fmt.Errorf("创建输出目录失败 %s: %w", syncDeployAgentOutDir, err)
	}
	outDirAbs, err := filepath.Abs(syncDeployAgentOutDir)
	if err != nil {
		return fmt.Errorf("解析输出目录失败 %s: %w", syncDeployAgentOutDir, err)
	}
	workDirAbs, err := filepath.Abs(syncDeployAgentWorkDir)
	if err != nil {
		return fmt.Errorf("解析工作目录失败 %s: %w", syncDeployAgentWorkDir, err)
	}

	var files []string
	for _, arch := range arches {
		name := deployAgentAssetName(version, arch)
		// 必须用绝对路径：go build 的 cwd 是 pixiu 源码目录，相对 -o 会写到错误位置。
		out := filepath.Join(outDirAbs, name)
		fmt.Printf("编译 deploy-agent (%s/%s) → %s\n", deployAgentGOOS, arch, out)
		if err := buildDeployAgentBinary(ctx, workDirAbs, out, deployAgentGOOS, arch); err != nil {
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

// buildDeployAgentBinary 在 pixiu 源码目录交叉编译 cmd/deploy-agent。
func buildDeployAgentBinary(ctx context.Context, srcRoot, outPath, goos, goarch string) error {
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
	c := exec.CommandContext(ctx, "go", "build", "-o", outPath, "./cmd/deploy-agent")
	c.Dir = srcRoot
	c.Env = append(os.Environ(),
		"CGO_ENABLED=0",
		"GOOS="+goos,
		"GOARCH="+goarch,
	)
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	if err := c.Run(); err != nil {
		return fmt.Errorf("go build deploy-agent (%s/%s) 失败: %w", goos, goarch, err)
	}
	if err := os.Chmod(outPath, 0o755); err != nil {
		return fmt.Errorf("设置可执行权限失败 %s: %w", outPath, err)
	}
	return nil
}

// readPixiuTagVersion 解析 pixiu 版本号：ls-remote 拉取远端全部 tag，
// --ref 本身是 tag 时直接使用该 tag，否则取 semver 最大的 tag。
func readPixiuTagVersion(ctx context.Context, repoURL, ref string) (string, error) {
	c := exec.CommandContext(ctx, "git", "ls-remote", "--tags", "--refs", authedRepoURL(repoURL))
	var stdout, stderr bytes.Buffer
	c.Stdout = &stdout
	c.Stderr = &stderr
	if err := c.Run(); err != nil {
		return "", fmt.Errorf("git ls-remote tags 失败: %w\n%s", err, stderr.String())
	}
	return resolvePixiuVersion(parseLsRemoteTags(stdout.String()), ref)
}

// parseLsRemoteTags 解析 git ls-remote --tags --refs 输出（每行 "<sha>\trefs/tags/<name>"）为 tag 名列表。
func parseLsRemoteTags(out string) []string {
	var tags []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		const prefix = "refs/tags/"
		if !strings.HasPrefix(fields[1], prefix) {
			continue
		}
		name := strings.TrimPrefix(fields[1], prefix)
		// --refs 不输出 peeled tag（<name>^{}），此处防御性跳过。
		if name == "" || strings.HasSuffix(name, "^{}") {
			continue
		}
		tags = append(tags, name)
	}
	return tags
}

// resolvePixiuVersion 从远端 tags 中解析版本：ref 本身是 tag 时直接返回 ref，否则取 semver 最大者。
func resolvePixiuVersion(tags []string, ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	for _, t := range tags {
		if t == ref {
			return ref, nil
		}
	}
	return latestSemverTag(tags)
}

// latestSemverTag 返回 semver 最大的 tag（不可解析的 tag 跳过）；无任何合法 semver 时报错。
// 比较相等时取字典序较大者，保证结果稳定。
func latestSemverTag(tags []string) (string, error) {
	best := ""
	var bestVer semver
	for _, t := range tags {
		v, ok := parseSemver(t)
		if !ok {
			continue
		}
		if best == "" {
			best, bestVer = t, v
			continue
		}
		if cmp := compareSemver(v, bestVer); cmp > 0 || (cmp == 0 && t > best) {
			best, bestVer = t, v
		}
	}
	if best == "" {
		return "", fmt.Errorf("未在远端 tag 中解析出合法 semver 版本号（tags=%v）", tags)
	}
	return best, nil
}

// semver 解析后的版本号：核心段（数字数组）+ prerelease 标识符（为空表示正式版）。
type semver struct {
	core []int
	pre  []string
}

// parseSemver 解析 "v?数字(.数字)*(-prerelease)?" 形式的 tag；不合法返回 false。
func parseSemver(tag string) (semver, bool) {
	s := strings.TrimSpace(tag)
	s = strings.TrimPrefix(s, "v")
	var prePart string
	if i := strings.IndexByte(s, '-'); i >= 0 {
		prePart = s[i+1:]
		s = s[:i]
	}
	if s == "" {
		return semver{}, false
	}
	parts := strings.Split(s, ".")
	core := make([]int, 0, len(parts))
	for _, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return semver{}, false
		}
		core = append(core, n)
	}
	var pre []string
	if prePart != "" {
		pre = strings.Split(prePart, ".")
		for _, id := range pre {
			if id == "" {
				return semver{}, false
			}
		}
	}
	return semver{core: core, pre: pre}, true
}

// compareSemver 比较两个 semver，返回 -1 / 0 / 1：
// 核心段逐段数值比较（缺失按 0）；核心段相同时有 prerelease 者小于正式版；
// prerelease 标识符按 semver 规则逐个比较，前缀相同时标识符多者大。
func compareSemver(a, b semver) int {
	n := len(a.core)
	if len(b.core) > n {
		n = len(b.core)
	}
	for i := 0; i < n; i++ {
		av, bv := 0, 0
		if i < len(a.core) {
			av = a.core[i]
		}
		if i < len(b.core) {
			bv = b.core[i]
		}
		if av != bv {
			if av < bv {
				return -1
			}
			return 1
		}
	}
	if len(a.pre) == 0 && len(b.pre) == 0 {
		return 0
	}
	if len(a.pre) == 0 {
		return 1
	}
	if len(b.pre) == 0 {
		return -1
	}
	n = len(a.pre)
	if len(b.pre) < n {
		n = len(b.pre)
	}
	for i := 0; i < n; i++ {
		if c := comparePrereleaseID(a.pre[i], b.pre[i]); c != 0 {
			return c
		}
	}
	switch {
	case len(a.pre) == len(b.pre):
		return 0
	case len(a.pre) < len(b.pre):
		return -1
	default:
		return 1
	}
}

// comparePrereleaseID 按 semver 规则比较两个 prerelease 标识符：数字标识符小于非数字标识符，
// 均为数字时按数值比较，均为非数字时按字典序比较。
func comparePrereleaseID(a, b string) int {
	an, aErr := strconv.Atoi(a)
	bn, bErr := strconv.Atoi(b)
	aNum, bNum := aErr == nil, bErr == nil
	switch {
	case aNum && bNum:
		switch {
		case an == bn:
			return 0
		case an < bn:
			return -1
		default:
			return 1
		}
	case aNum:
		return -1
	case bNum:
		return 1
	default:
		return strings.Compare(a, b)
	}
}

// gitShortHead 返回仓库 HEAD 的短 hash（追溯用）。
func gitShortHead(ctx context.Context, repoDir string) (string, error) {
	out, err := exec.CommandContext(ctx, "git", "-C", repoDir, "rev-parse", "--short", "HEAD").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// deployAgentGitHubTag 默认 deploy-agent-{version}；--github-tag 非空时覆盖。
func deployAgentGitHubTag(version string) string {
	if tag := strings.TrimSpace(githubTag); tag != "" {
		return tag
	}
	return "deploy-agent-" + version
}

// deployAgentAssetName 产物名：pixiu-deploy-agent-{version}-{arch}（裸二进制，不打包）。
func deployAgentAssetName(version, arch string) string {
	return fmt.Sprintf("pixiu-deploy-agent-%s-%s", version, arch)
}
