// builder 是一个制作 Kubernetes 离线安装包（含安装包与镜像）的 CLI 工具。
package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/google/go-containerregistry/pkg/crane"

	"builder/internal/builder"
	"builder/internal/config"
	"builder/internal/ghupload"
	"builder/internal/mirror"
	"builder/internal/serve"
)

// 全局 flag
var configFile string

// build 子命令 flags
var (
	buildOS         string
	buildOSVersion  string
	buildK8sVersion string // --kubernetes-version
	buildArch       string
	buildMirror     string
	buildWorkDir    string
	buildOutDir     string
	buildMode       string
	buildSkipAddons bool
	buildOnlyAddons bool
	buildDryRun     bool
	buildKeepFiles  bool
	buildVerbose    bool
	buildKubeadmDir string
	buildUpload     bool
)

// upload 子命令 flags
var (
	uploadFiles []string
	uploadDir   string
	uploadSkips []string
)

// upload kubeadm 子命令 flags
var (
	uploadKubeadmVersion string
	uploadKubeadmArch    string
	uploadKubeadmOutDir  string
)

// verify 子命令 flags
var verifyBundle string

// serve 子命令 flags
var (
	serveBundles       []string
	serveDir           string
	serveDataDir       string
	serveRegistryAddr  string
	serveRepoAddr      string
	serveAdvertiseHost string
	serveSkipImages    bool
	serveSkipPackages  bool
)

// github release 上传 flags（build --upload 与 upload 子命令共用）
var (
	githubOwner string
	githubRepo  string
	githubTag   string
	githubToken string
)

func main() {
	if err := newRootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		os.Exit(1)
	}
}

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "builder",
		Short: "制作 Kubernetes 离线安装包（apt/dnf 软件包 + 运行时 + 镜像）",
		Long: `builder 生成一个完整的 Kubernetes 离线安装包，包含：
  - k8s 软件包（kubeadm / kubelet / kubectl，容器内 apt/dnf 下载）
  - 容器运行时软件包（containerd.io / cri-tools，runc 由 containerd.io 内嵌提供）
  - 系统依赖软件包（conntrack / ipvsadm / nfs 等，容器内 apt/dnf 下载）
  - 核心与附加组件镜像（docker pull + save）
  - 离线安装脚本（install.sh / load-images.sh）
  - 完整性清单 manifest.yaml（可校验）`,
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			switch cmd.Name() {
			case "serve", "version", "help":
				return nil
			}
			// 校验配置文件可读
			if _, err := os.Stat(configFile); err != nil {
				return fmt.Errorf("配置文件不可用: %s（可设置 --configFile 或 BUILDER_CONFIG_FILE）", configFile)
			}
			return nil
		},
	}

	root.PersistentFlags().StringVar(&configFile, "configFile", config.DefaultConfigFile(), "配置文件路径（builder.yaml）")

	root.AddCommand(newBuildCmd())
	root.AddCommand(newUploadCmd())
	root.AddCommand(newUploadKubeadmCmd())
	root.AddCommand(newServeCmd())
	root.AddCommand(newListOSCmd())
	root.AddCommand(newListK8sCmd())
	root.AddCommand(newListImagesCmd())
	root.AddCommand(newListServeImagesCmd())
	root.AddCommand(newVerifyCmd())
	root.AddCommand(newVersionCmd())

	return root
}

func newBuildCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "build",
		Short: "构建离线安装包",
		Args:  cobra.NoArgs,
		Example: `  builder build --os ubuntu --os-version 22.04 --kubernetes-version v1.27.3 --arch amd64
  builder build --mode images --kubernetes-version v1.27.3 --arch amd64 --out ./dist
  builder build --os ubuntu --os-version 22.04 --kubernetes-version v1.27.3 --only-addons
  builder build --os ubuntu --os-version 22.04 --kubernetes-version v1.27.3 --skip-addons`,
		RunE: runBuild,
	}
	cmd.Flags().StringVar(&buildOS, "os", "", "目标操作系统（任意，如 ubuntu；--mode images 时可省略）")
	cmd.Flags().StringVar(&buildOSVersion, "os-version", "", "操作系统版本（任意，如 22.04；--mode images 时可省略）")
	cmd.Flags().StringVar(&buildK8sVersion, "kubernetes-version", "", "k8s 版本（如 v1.27.3；--only-addons 时可选）")
	cmd.Flags().StringVar(&buildArch, "arch", "amd64", "目标架构（amd64/arm64）")
	cmd.Flags().StringVar(&buildMirror, "mirror", "aliyun", "镜像仓库源（默认 aliyun；official/aliyun/tencent，镜像阶段获取与生成 k8s 镜像均带该仓库地址）")
	cmd.Flags().StringVar(&buildWorkDir, "workdir", "./work", "工作目录（bundle 在此构建）")
	cmd.Flags().StringVar(&buildOutDir, "out", "./dist", "产物输出目录（tar.gz 输出到此）")
	cmd.Flags().StringVar(&buildMode, "mode", "all", "构建模式：packages=仅软件包 / images=仅镜像 / all=两者都构建（默认）")
	cmd.Flags().BoolVar(&buildSkipAddons, "skip-addons", false, "跳过附加组件（addon_images 与 addon_packages 均不并入），仅核心软件包/镜像")
	cmd.Flags().BoolVar(&buildOnlyAddons, "only-addons", false, "只打包附加组件（addon_images/addon_packages），核心软件包与镜像全去；与 --skip-addons 互斥")
	cmd.Flags().BoolVar(&buildDryRun, "dry-run", false, "仅演练管线，不执行真实下载/拉取")
	cmd.Flags().BoolVar(&buildKeepFiles, "keep-files", false, "构建完成后保留中间文件（packages/images/bundle 目录；默认清理）")
	cmd.Flags().BoolVarP(&buildVerbose, "verbose", "v", false, "打印详细过程日志（镜像下载/pull 进度等）")
	cmd.Flags().StringVar(&buildKubeadmDir, "kubeadm-dir", "./kube", "kubeadm 二进制缓存目录（按 kubeadm-{version}-linux-{arch} 命名）")
	cmd.Flags().BoolVar(&buildUpload, "upload", false, "构建完成后将产物 tar.gz 上传到 GitHub Release（需配置 github.owner/repo 或 --github-owner/--github-repo）")
	addGitHubFlags(cmd)
	// 必填校验（os/os-version/kubernetes-version）改为手动判断：命令行或配置 build 节任一提供即可，
	// 因此不使用 cobra 的 MarkFlagRequired（它只认命令行，会阻断配置兜底）。
	// --only-addons 时 kubernetes-version 可省略（不构建 k8s 核心，无需推导 k8s 版本）。
	return cmd
}

func addGitHubFlags(cmd *cobra.Command) {
	cmd.Flags().StringVar(&githubOwner, "github-owner", "", "GitHub 仓库所有者（覆盖配置文件 github.owner）")
	cmd.Flags().StringVar(&githubRepo, "github-repo", "", "GitHub 仓库名（覆盖配置文件 github.repo）")
	cmd.Flags().StringVar(&githubTag, "github-tag", "", "GitHub Release tag（覆盖配置文件 github.tag；build 时默认用 --kubernetes-version）")
	cmd.Flags().StringVar(&githubToken, "github-token", "", "GitHub token（覆盖配置文件 github.token；也可用环境变量 GITHUB_TOKEN/GH_TOKEN）")
}

// buildFlagValues 记录 build 子命令各 flag 的当前值（含 flag 内置默认值或命令行显式传入值）。
type buildFlagValues struct {
	OS         string
	OSVersion  string
	K8sVersion string
	Arch       string
	Mirror     string
	WorkDir    string
	OutDir     string
	Mode       string
	SkipAddons bool
	OnlyAddons bool
	DryRun     bool
	KeepFiles  bool
	Verbose    bool
	KubeadmDir string
}

// buildFlagChanged 记录各 flag 是否被命令行显式设置（true 表示命令行值优先）。
// K8sVersion 由 --kubernetes-version 被设置即视为显式。
type buildFlagChanged struct {
	OS         bool
	OSVersion  bool
	K8sVersion bool
	Arch       bool
	Mirror     bool
	WorkDir    bool
	OutDir     bool
	Mode       bool
	SkipAddons bool
	OnlyAddons bool
	DryRun     bool
	KeepFiles  bool
	Verbose    bool
	KubeadmDir bool
}

// buildOptions build 子命令合并后的生效参数（Mirror 保持字符串，由调用方解析为 mirror.Mirror）。
type buildOptions struct {
	OS         string
	OSVersion  string
	K8sVersion string
	Arch       string
	Mirror     string
	WorkDir    string
	OutDir     string
	Mode       string
	SkipAddons bool
	OnlyAddons bool
	DryRun     bool
	KeepFiles  bool
	Verbose    bool
	KubeadmDir string
}

// resolveBuildOptions 按"命令行 > 配置文件 build 节 > flag 内置默认值"合并 build 参数。
// vals 为 flag 当前值（内置默认或命令行显式值），changed 标记哪些 flag 被显式设置，
// cfg.Build 为配置文件 build 节（可为零值，表示未配置）。
func resolveBuildOptions(cfg *config.Config, vals buildFlagValues, changed buildFlagChanged) buildOptions {
	return buildOptions{
		OS:         resolveString(changed.OS, vals.OS, cfg.Build.OS),
		OSVersion:  resolveString(changed.OSVersion, vals.OSVersion, cfg.Build.OSVersion),
		K8sVersion: resolveString(changed.K8sVersion, vals.K8sVersion, cfg.Build.KubernetesVersion),
		Arch:       resolveString(changed.Arch, vals.Arch, cfg.Build.Arch),
		Mirror:     resolveString(changed.Mirror, vals.Mirror, cfg.Build.Mirror),
		WorkDir:    resolveString(changed.WorkDir, vals.WorkDir, cfg.Build.WorkDir),
		OutDir:     resolveString(changed.OutDir, vals.OutDir, cfg.Build.OutDir),
		Mode:       resolveString(changed.Mode, vals.Mode, cfg.Build.Mode),
		SkipAddons: resolveBool(changed.SkipAddons, vals.SkipAddons, cfg.Build.SkipAddons),
		OnlyAddons: resolveBool(changed.OnlyAddons, vals.OnlyAddons, cfg.Build.OnlyAddons),
		DryRun:     resolveBool(changed.DryRun, vals.DryRun, cfg.Build.DryRun),
		KeepFiles:  resolveBool(changed.KeepFiles, vals.KeepFiles, cfg.Build.KeepFiles),
		Verbose:    resolveBool(changed.Verbose, vals.Verbose, cfg.Build.Verbose),
		KubeadmDir: resolveString(changed.KubeadmDir, vals.KubeadmDir, cfg.Build.KubeadmDir),
	}
}

// resolveString 字符串参数优先级：命令行显式 → 配置值（非空）→ flag 当前值（内置默认）。
// changed 为 true 时 flagVal 即命令行显式值；否则 flagVal 为 flag 内置默认值。
func resolveString(changed bool, flagVal, cfgVal string) string {
	if changed {
		return flagVal
	}
	if cfgVal != "" {
		return cfgVal
	}
	return flagVal
}

// resolveBool 布尔参数优先级：命令行显式 → 配置值 → flag 内置默认值（false）。
// bool 无"空"概念，配置未设置时默认 false，与 flag 默认一致。
func resolveBool(changed bool, flagVal, cfgVal bool) bool {
	if changed {
		return flagVal
	}
	return cfgVal
}

// requiredMissing 返回 build 必填参数中缺失项的列表（空表示无缺失）。
// os/os-version 在 images 模式下可不指定；kubernetes-version 在 --only-addons 时可不指定，
// 其余情况任何模式均必填。
// 缺失项由命令行 flag 或配置 build 节任一提供即可，这里只看最终生效值。
func requiredMissing(opts buildOptions, mode string) []string {
	var missing []string
	if !opts.OnlyAddons && opts.K8sVersion == "" {
		missing = append(missing, "kubernetes-version")
	}
	if mode != "images" {
		if opts.OS == "" {
			missing = append(missing, "os")
		}
		if opts.OSVersion == "" {
			missing = append(missing, "os-version")
		}
	}
	return missing
}

func runBuild(cmd *cobra.Command, args []string) error {
	cfg, err := config.Load(configFile)
	if err != nil {
		return err
	}

	// 合并命令行（显式优先）与配置 build 节：命令行 > 配置文件 > flag 内置默认值。
	opts := resolveBuildOptions(cfg, buildFlagValues{
		OS:         buildOS,
		OSVersion:  buildOSVersion,
		K8sVersion: buildK8sVersion,
		Arch:       buildArch,
		Mirror:     buildMirror,
		WorkDir:    buildWorkDir,
		OutDir:     buildOutDir,
		Mode:       buildMode,
		SkipAddons: buildSkipAddons,
		OnlyAddons: buildOnlyAddons,
		DryRun:     buildDryRun,
		KeepFiles:  buildKeepFiles,
		Verbose:    buildVerbose,
		KubeadmDir: buildKubeadmDir,
	}, buildFlagChanged{
		OS:         cmd.Flags().Changed("os"),
		OSVersion:  cmd.Flags().Changed("os-version"),
		K8sVersion: cmd.Flags().Changed("kubernetes-version"),
		Arch:       cmd.Flags().Changed("arch"),
		Mirror:     cmd.Flags().Changed("mirror"),
		WorkDir:    cmd.Flags().Changed("workdir"),
		OutDir:     cmd.Flags().Changed("out"),
		Mode:       cmd.Flags().Changed("mode"),
		SkipAddons: cmd.Flags().Changed("skip-addons"),
		OnlyAddons: cmd.Flags().Changed("only-addons"),
		DryRun:     cmd.Flags().Changed("dry-run"),
		KeepFiles:  cmd.Flags().Changed("keep-files"),
		Verbose:    cmd.Flags().Changed("verbose"),
		KubeadmDir: cmd.Flags().Changed("kubeadm-dir"),
	})

	mirrorVal, err := mirror.ParseMirror(opts.Mirror)
	if err != nil {
		return err
	}

	// 构建模式校验：packages=仅软件包 / images=仅镜像 / all=两者都构建
	mode := opts.Mode
	switch mode {
	case "packages", "images", "all":
	default:
		return fmt.Errorf("非法 --mode 取值 %q（可选: packages=仅软件包 / images=仅镜像 / all=两者都构建）", opts.Mode)
	}

	// 必填校验：os / os-version / kubernetes-version 由命令行或配置 build 节任一提供。
	// --mode images 可不指定 OS；packages/all 必须指定；--only-addons 时 kubernetes-version 可省略。
	if mode == "images" && (opts.OS == "") != (opts.OSVersion == "") {
		return fmt.Errorf("--mode images 下 --os 与 --os-version 需同时指定或同时省略")
	}
	if missing := requiredMissing(opts, mode); len(missing) > 0 {
		return fmt.Errorf("缺少必填参数: %s（可通过命令行 flag 或配置文件 build 节指定）", strings.Join(missing, ", "))
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	kubeadmBin := ""
	if buildNeedsKubeadm(mode, opts) {
		var cleanup func()
		kubeadmBin, cleanup, err = prepareBuildKubeadm(ctx, cfg, opts.K8sVersion, opts.Arch, opts.KubeadmDir, opts.Verbose)
		if err != nil {
			return err
		}
		defer cleanup()
	}

	res, err := builder.Build(ctx, builder.Options{
		Config:     cfg,
		OS:         opts.OS,
		OSVersion:  opts.OSVersion,
		Arch:       opts.Arch,
		K8sVersion: opts.K8sVersion,
		Mirror:     mirrorVal,
		WorkDir:    opts.WorkDir,
		OutDir:     opts.OutDir,
		Mode:       mode,
		SkipAddons: opts.SkipAddons,
		OnlyAddons: opts.OnlyAddons,
		DryRun:     opts.DryRun,
		KeepFiles:  opts.KeepFiles,
		Verbose:    opts.Verbose,
		KubeadmBin: kubeadmBin,
	})
	if err != nil {
		return err
	}

	fmt.Println(res.StepsTable())
	if opts.DryRun {
		fmt.Println("[dry-run] 未执行真实下载/拉取，以上为管线演练结果")
	}
	fmt.Printf("bundle 目录: %s\n", res.BundleDir)
	if len(res.TarPaths) > 1 {
		fmt.Println("离线包产物（独立 tar）：")
		for _, p := range res.TarPaths {
			fmt.Printf("  - %s\n", p)
		}
	} else {
		fmt.Printf("离线包产物: %s\n", res.TarPath)
	}

	if buildUpload {
		if opts.DryRun {
			fmt.Println("[dry-run] 跳过 GitHub Release 上传")
			return nil
		}
		files := res.TarPaths
		if len(files) == 0 && res.TarPath != "" {
			files = []string{res.TarPath}
		}
		if err := uploadGitHubArtifacts(ctx, cfg, files, opts.K8sVersion); err != nil {
			return err
		}
	}
	return nil
}

func newUploadCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "upload",
		Short: "将离线包 tar.gz 或文件夹下所有文件上传到 GitHub Release",
		Example: `  builder upload --file ./dist/a.tar.gz --github-owner acme --github-repo builder --github-tag v1.27.3
  builder upload --file a.tar.gz --file b.tar.gz --github-owner acme --github-repo builder --github-tag v1.27.3
  builder upload --dir ./dist --skip md5sum.txt --skip checksum.txt --github-owner acme --github-repo builder --github-tag v1.27.3`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(configFile)
			if err != nil {
				return err
			}
			files := make([]string, 0, len(uploadFiles))
			files = append(files, uploadFiles...)
			if uploadDir != "" {
				dirFiles, err := collectDirFiles(uploadDir)
				if err != nil {
					return fmt.Errorf("遍历目录 %s 失败: %w", uploadDir, err)
				}
				files = append(files, dirFiles...)
			}
			files = filterSkipped(files, uploadSkips)
			if len(files) == 0 {
				return fmt.Errorf("没有可上传的文件（请用 --file 指定文件或 --dir 指定文件夹）")
			}
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return uploadGitHubArtifacts(ctx, cfg, files, "")
		},
	}
	cmd.Flags().StringArrayVar(&uploadFiles, "file", nil, "待上传的本地文件（可重复）")
	cmd.Flags().StringVar(&uploadDir, "dir", "", "指定文件夹，递归上传其下所有文件")
	cmd.Flags().StringArrayVar(&uploadSkips, "skip", nil, "按文件名忽略的文件（可重复，对 --file 与 --dir 均生效）")
	addGitHubFlags(cmd)
	return cmd
}

func newUploadKubeadmCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "upload-kubeadm",
		Short: "下载 kubeadm 二进制并上传到 GitHub Release",
		Example: `  builder upload-kubeadm --kubernetes-version v1.31.6 --arch amd64 --github-owner acme --github-repo builder
  builder upload-kubeadm --kubernetes-version v1.31.6 --arch amd64 --out-dir ./dist --github-tag v1.31.6`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(configFile)
			if err != nil {
				return err
			}
			if uploadKubeadmVersion == "" {
				return fmt.Errorf("缺少 --kubernetes-version")
			}
			if !cfg.ValidK8s(uploadKubeadmVersion) {
				return fmt.Errorf("非法的 k8s 版本格式: %s（期望形如 v1.31.6）", uploadKubeadmVersion)
			}
			if !config.ValidArch(uploadKubeadmArch) {
				return fmt.Errorf("不支持的架构 %s（可选 amd64/arm64）", uploadKubeadmArch)
			}

			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			githubOpts := mergeGitHubOptions(cfg.GitHub, githubOwner, githubRepo, githubTag, githubToken)
			fmt.Printf("确保 GitHub Release 存在: %s/%s@%s\n", githubOpts.Owner, githubOpts.Repo, effectiveGitHubTag(cfg, uploadKubeadmVersion))
			if err := ensureGitHubRelease(ctx, cfg, uploadKubeadmVersion); err != nil {
				return err
			}

			if err := os.MkdirAll(uploadKubeadmOutDir, 0o755); err != nil {
				return fmt.Errorf("创建输出目录失败 %s: %w", uploadKubeadmOutDir, err)
			}
			name := fmt.Sprintf("kubeadm-%s-linux-%s", uploadKubeadmVersion, uploadKubeadmArch)
			path := filepath.Join(uploadKubeadmOutDir, name)
			u := kubeadmDownloadURL(uploadKubeadmVersion, uploadKubeadmArch)
			fmt.Printf("下载 kubeadm: %s → %s\n", u, path)
			if err := downloadFile(ctx, u, path, 0o755); err != nil {
				return err
			}
			fmt.Printf("下载完成: %s\n", path)
			return uploadGitHubArtifacts(ctx, cfg, []string{path}, uploadKubeadmVersion)
		},
	}
	cmd.Flags().StringVar(&uploadKubeadmVersion, "kubernetes-version", "", "k8s 版本（如 v1.31.6，必填）")
	cmd.Flags().StringVar(&uploadKubeadmArch, "arch", "amd64", "目标架构（amd64/arm64）")
	cmd.Flags().StringVar(&uploadKubeadmOutDir, "out-dir", "./dist", "kubeadm 下载输出目录")
	addGitHubFlags(cmd)
	return cmd
}

// collectDirFiles 递归收集 dir 下所有普通文件（跳过目录本身）。
func collectDirFiles(dir string) ([]string, error) {
	var files []string
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		files = append(files, path)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return files, nil
}

// filterSkipped 按文件名（basename）过滤掉 skip 列表中的文件。
func filterSkipped(files, skips []string) []string {
	skipSet := make(map[string]bool, len(skips))
	for _, s := range skips {
		skipSet[s] = true
	}
	out := make([]string, 0, len(files))
	for _, f := range files {
		if skipSet[filepath.Base(f)] {
			continue
		}
		out = append(out, f)
	}
	return out
}

func kubeadmDownloadURL(version, arch string) string {
	return fmt.Sprintf("https://dl.k8s.io/release/%s/bin/linux/%s/kubeadm", version, arch)
}

func downloadFile(ctx context.Context, rawURL, dst string, mode os.FileMode) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return fmt.Errorf("创建下载请求失败: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("下载失败 %s: %w", rawURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		msg := strings.TrimSpace(string(data))
		if msg == "" {
			msg = resp.Status
		}
		return fmt.Errorf("下载失败 %s: HTTP %d: %s", rawURL, resp.StatusCode, msg)
	}

	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return fmt.Errorf("创建下载文件失败 %s: %w", tmp, err)
	}
	_, copyErr := io.Copy(out, resp.Body)
	closeErr := out.Close()
	if copyErr != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("写入下载文件失败 %s: %w", tmp, copyErr)
	}
	if closeErr != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("关闭下载文件失败 %s: %w", tmp, closeErr)
	}
	if err := os.Chmod(tmp, mode); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("设置文件权限失败 %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("保存下载文件失败 %s: %w", dst, err)
	}
	return nil
}

func buildNeedsKubeadm(mode string, opts buildOptions) bool {
	return mode != "packages" && !opts.OnlyAddons && !opts.DryRun
}

func kubeadmAssetName(version, arch string) string {
	return fmt.Sprintf("kubeadm-%s-linux-%s", version, arch)
}

func prepareBuildKubeadm(ctx context.Context, cfg *config.Config, version, arch, dir string, verbose bool) (string, func(), error) {
	if dir == "" {
		dir = "./kube"
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", nil, fmt.Errorf("创建 kubeadm 缓存目录失败 %s: %w", dir, err)
	}
	path := filepath.Join(dir, kubeadmAssetName(version, arch))
	if st, err := os.Stat(path); err == nil {
		if st.IsDir() {
			return "", nil, fmt.Errorf("kubeadm 缓存路径是目录: %s", path)
		}
		if err := os.Chmod(path, 0o755); err != nil {
			return "", nil, fmt.Errorf("设置 kubeadm 可执行权限失败 %s: %w", path, err)
		}
		fmt.Printf("复用本地 kubeadm: %s\n", path)
		return path, func() {}, nil
	} else if !os.IsNotExist(err) {
		return "", nil, fmt.Errorf("检查 kubeadm 缓存文件失败 %s: %w", path, err)
	}
	return downloadBuildKubeadmFromGitHub(ctx, cfg, version, arch, path, verbose)
}

func downloadBuildKubeadmFromGitHub(ctx context.Context, cfg *config.Config, version, arch, path string, verbose bool) (string, func(), error) {
	opts := mergeGitHubOptions(cfg.GitHub, githubOwner, githubRepo, githubTag, githubToken)
	if opts.Tag == "" {
		opts.Tag = version
	}
	if verbose {
		opts.Progress = os.Stdout
	}
	if opts.Owner == "" || opts.Repo == "" {
		return "", nil, fmt.Errorf("github owner/repo 不能为空（配置 github.owner/github.repo 或 --github-owner/--github-repo）")
	}
	if opts.Tag == "" {
		return "", nil, fmt.Errorf("github tag 不能为空（配置 github.tag、--github-tag，或提供 --kubernetes-version）")
	}

	assetName := kubeadmAssetName(version, arch)
	fmt.Printf("从 GitHub Release 下载 kubeadm: %s/%s@%s/%s → %s\n", opts.Owner, opts.Repo, opts.Tag, assetName, path)
	if err := ghupload.DownloadAsset(ctx, opts, assetName, path, 0o755); err != nil {
		_ = os.Remove(path)
		return "", nil, err
	}
	return path, func() {}, nil
}

func effectiveGitHubTag(cfg *config.Config, defaultTag string) string {
	opts := mergeGitHubOptions(cfg.GitHub, githubOwner, githubRepo, githubTag, githubToken)
	if opts.Tag != "" {
		return opts.Tag
	}
	return defaultTag
}

func ensureGitHubRelease(ctx context.Context, cfg *config.Config, defaultTag string) error {
	opts := mergeGitHubOptions(cfg.GitHub, githubOwner, githubRepo, githubTag, githubToken)
	if opts.Tag == "" {
		opts.Tag = defaultTag
	}
	if opts.Owner == "" || opts.Repo == "" {
		return fmt.Errorf("github owner/repo 不能为空（配置 github.owner/github.repo 或 --github-owner/--github-repo）")
	}
	if opts.Tag == "" {
		return fmt.Errorf("github tag 不能为空（配置 github.tag、--github-tag，或提供 --kubernetes-version）")
	}
	return ghupload.EnsureRelease(ctx, opts)
}

// uploadGitHubArtifacts 合并配置与 CLI 覆盖后上传文件到 GitHub Release。
// defaultTag 在配置与 flag 均未指定 tag 时使用（build 场景一般为 kubernetes 版本）。
func uploadGitHubArtifacts(ctx context.Context, cfg *config.Config, files []string, defaultTag string) error {
	opts := mergeGitHubOptions(cfg.GitHub, githubOwner, githubRepo, githubTag, githubToken)
	if opts.Tag == "" {
		opts.Tag = defaultTag
	}
	if opts.Owner == "" || opts.Repo == "" {
		return fmt.Errorf("github owner/repo 不能为空（配置 github.owner/github.repo 或 --github-owner/--github-repo）")
	}
	if opts.Tag == "" {
		return fmt.Errorf("github tag 不能为空（配置 github.tag、--github-tag，或 build 时提供 --kubernetes-version）")
	}
	// 目标 tag 的 Release 不存在时自动创建（EnsureRelease GET 404 后创建）。
	fmt.Printf("确保 GitHub Release 存在: %s/%s@%s\n", opts.Owner, opts.Repo, opts.Tag)
	if err := ghupload.EnsureRelease(ctx, opts); err != nil {
		return err
	}
	fmt.Printf("上传到 GitHub Release %s/%s@%s （共 %d 个文件）...\n", opts.Owner, opts.Repo, opts.Tag, len(files))
	res, err := ghupload.UploadFiles(ctx, opts, files)
	if err != nil {
		return err
	}
	fmt.Println("上传完成：")
	for _, u := range res {
		fmt.Printf("  - %s → %s\n", u.LocalPath, u.BrowserURL)
	}
	return nil
}

func newServeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "加载离线产物，提供 docker pull（短名）与 yum/dnf/apt 软件源",
		Long: `将 builder 产物（目录或 tar.gz）加载后常驻服务：
  - OCI registry：docker pull <host>:5000/<短名>:<tag>
  - HTTP 软件源：dnf/yum 使用 /rpm，apt 使用 /deb

纯 Go 实现，不依赖 createrepo / apt-ftparchive 等外部工具。`,
		Example: `  builder serve --bundle ./dist/pixiu-packages-centos-8-amd64-v1.27.3.tar.gz \
                --bundle ./dist/pixiu-images-centos-8-amd64-v1.27.3.tar.gz
  builder serve --bundle ./work/pixiu-ubuntu-22.04-amd64-v1.27.3 --advertise-host 192.168.1.10`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(serveBundles) == 0 && serveDir == "" {
				return fmt.Errorf("请通过 --bundle 指定离线包，或 --dir 指定离线包目录")
			}
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			_, err := serve.Run(ctx, serve.Options{
				Bundles:       serveBundles,
				Dir:           serveDir,
				DataDir:       serveDataDir,
				RegistryAddr:  serveRegistryAddr,
				RepoAddr:      serveRepoAddr,
				AdvertiseHost: serveAdvertiseHost,
				SkipImages:    serveSkipImages,
				SkipPackages:  serveSkipPackages,
			})
			return err
		},
	}
	cmd.Flags().StringArrayVar(&serveBundles, "bundle", nil, "离线包目录或 tar.gz（可重复，例如 packages + images）")
	cmd.Flags().StringVar(&serveDir, "dir", "", "离线包目录：加载其下所有 *.tar.gz 并轮询热加载新包（3s）")
	cmd.Flags().StringVar(&serveDataDir, "data-dir", "./serve-data", "工作目录（解压、repodata、registry blob）")
	cmd.Flags().StringVar(&serveRegistryAddr, "registry-addr", "0.0.0.0:5000", "OCI registry 监听地址")
	cmd.Flags().StringVar(&serveRepoAddr, "repo-addr", "0.0.0.0:8080", "软件源 HTTP 监听地址")
	cmd.Flags().StringVar(&serveAdvertiseHost, "advertise-host", serve.LocalIP(), "打印给客户端的主机名/IP（不含端口），默认本机 IP")
	cmd.Flags().BoolVar(&serveSkipImages, "skip-images", false, "不提供镜像 registry")
	cmd.Flags().BoolVar(&serveSkipPackages, "skip-packages", false, "不提供软件源")
	return cmd
}

func mergeGitHubOptions(cfg config.GitHubConfig, owner, repo, tag, token string) ghupload.Options {
	opts := ghupload.Options{
		Owner: cfg.Owner,
		Repo:  cfg.Repo,
		Tag:   cfg.Tag,
		Token: cfg.Token,
	}
	if owner != "" {
		opts.Owner = owner
	}
	if repo != "" {
		opts.Repo = repo
	}
	if tag != "" {
		opts.Tag = tag
	}
	if token != "" {
		opts.Token = token
	}
	return opts
}

func newListOSCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list-os",
		Short: "列出参考的操作系统与版本（实际 build 支持任意 OS/版本）",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(configFile)
			if err != nil {
				return err
			}
			fmt.Println("参考 OS/版本（builder.yaml oses 节；build 支持任意 OS/版本，未登记时按约定推导构建镜像）：")
			for _, osDef := range cfg.OSRegistry.OSes {
				fmt.Printf("  %-12s 包管理器: %-4s 架构: %s  版本: %s\n",
					osDef.Name, osDef.PkgManager, strings.Join(osDef.Archs, "/"), strings.Join(osDef.Versions, ", "))
			}
			fmt.Println()
			fmt.Println("示例: --os ubuntu --os-version 24.04（构建镜像默认 swr.cn-north-4.myhuaweicloud.com/pixiu-public/ubuntu:24.04）")
			return nil
		},
	}
}

func newListK8sCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list-k8s",
		Short: "列出参考的 k8s 版本与运行时版本",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(configFile)
			if err != nil {
				return err
			}
			fmt.Println("参考 k8s 版本（builder.yaml versions 节，仅作常见版本参考）：")
			for _, v := range cfg.K8sVersions.Versions {
				fmt.Printf("  %-12s containerd: %s  crictl: %s  runc: %s\n",
					v.Version, v.Containerd, v.Crictl, v.Runc)
			}
			fmt.Println()
			fmt.Println("支持任意 vX.Y.Z 版本（无需注册，仓库自动推导），例如: v1.29.5、v1.30.2")
			return nil
		},
	}
}

func newListImagesCmd() *cobra.Command {
	var listOS string
	var listK8sVersion string
	var listArch string

	cmd := &cobra.Command{
		Use:   "list-images",
		Short: "列出附加组件镜像清单",
		Args:  cobra.NoArgs,
		Example: `  builder list-images --os ubuntu --kubernetes-version v1.27.3 --arch amd64
  builder list-images --os ubuntu --kubernetes-version v1.31.0 --arch arm64`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(configFile)
			if err != nil {
				return err
			}

			if listK8sVersion == "" {
				return fmt.Errorf("缺少 --kubernetes-version")
			}

			if !cfg.ValidK8s(listK8sVersion) {
				return fmt.Errorf("非法的 k8s 版本格式: %s（期望形如 v1.31.0，如 v1.29.5）", listK8sVersion)
			}
			if listArch != "" && !config.ValidArch(listArch) {
				return fmt.Errorf("不支持的架构 %s（可选 amd64/arm64）", listArch)
			}

			fmt.Printf("附加组件镜像（%s / %s）:\n", listOS, listK8sVersion)
			for _, a := range cfg.AddonImages.Addons {
				fmt.Printf("  %-16s %s:%s\n", a.Name, a.Image, a.Tag)
			}
			fmt.Println()
			fmt.Println("核心镜像清单（kube-apiserver/controller-manager/scheduler/kube-proxy/etcd/pause/coredns 等）")
			fmt.Println("由 build 阶段下载官方 kubeadm 二进制后生成：")
			fmt.Printf("  kubeadm config images list --kubernetes-version %s --image-repository registry.k8s.io\n", listK8sVersion)
			return nil
		},
	}
	cmd.Flags().StringVar(&listOS, "os", "", "目标操作系统（必填，如 ubuntu）")
	cmd.Flags().StringVar(&listK8sVersion, "kubernetes-version", "", "k8s 版本（必填，如 v1.27.3）")
	cmd.Flags().StringVar(&listArch, "arch", "amd64", "目标架构")
	cmd.MarkFlagRequired("os")
	cmd.MarkFlagRequired("kubernetes-version")
	return cmd
}

func newListServeImagesCmd() *cobra.Command {
	var registryAddr string
	cmd := &cobra.Command{
		Use:   "list-serve-images",
		Short: "列出 serve 已加载的镜像（查询运行中的 registry）",
		Example: `  builder list-serve-images
  builder list-serve-images --registry-addr 192.168.1.10:5000`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return listServeImages(context.Background(), registryAddr)
		},
	}
	cmd.Flags().StringVar(&registryAddr, "registry-addr", "127.0.0.1:5000", "serve registry 地址")
	return cmd
}

// listServeImages 通过 Docker V2 API 查询 registry 的 _catalog 与各仓库 tags，
// 打印 serve 已加载的全部镜像（host/repo:tag）。
func listServeImages(ctx context.Context, addr string) error {
	repos, err := crane.Catalog(addr, crane.Insecure)
	if err != nil {
		return fmt.Errorf("查询 registry %s 失败（serve 是否已启动？）: %w", addr, err)
	}
	if len(repos) == 0 {
		fmt.Printf("registry %s 暂无已加载镜像\n", addr)
		return nil
	}
	sort.Strings(repos)
	count := 0
	for _, repo := range repos {
		tags, err := crane.ListTags(addr+"/"+repo, crane.Insecure)
		if err != nil {
			fmt.Fprintf(os.Stderr, "列出 %s tags 失败: %v\n", repo, err)
			continue
		}
		sort.Strings(tags)
		for _, tag := range tags {
			fmt.Printf("%s/%s:%s\n", addr, repo, tag)
			count++
		}
	}
	fmt.Printf("共 %d 个镜像\n", count)
	return nil
}

func newVerifyCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "verify",
		Short: "校验离线安装包完整性（目录或 tar.gz）",
		Example: `  builder verify --bundle ./dist/pixiu-images-ubuntu-22.04-amd64-v1.27.3.tar.gz
  builder verify --bundle ./work/pixiu-ubuntu-22.04-amd64-v1.27.3`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if verifyBundle == "" {
				return fmt.Errorf("--bundle 为必填参数")
			}
			m, err := builder.Verify(verifyBundle)
			if err != nil {
				return fmt.Errorf("校验失败: %w", err)
			}
			fmt.Printf("校验通过 ✔\n")
			fmt.Printf("  bundle: %s\n", verifyBundle)
			fmt.Printf("  元数据: os=%s/%s arch=%s k8s=%s mirror=%s\n",
				m.Meta.OS, m.Meta.OSVersion, m.Meta.Arch, m.Meta.K8sVersion, m.Meta.Mirror)
			fmt.Printf("  文件: %d  镜像: %d  脚本: %d\n", len(m.Files), len(m.Images), len(m.Scripts))
			return nil
		},
	}
	cmd.Flags().StringVar(&verifyBundle, "bundle", "", "bundle 目录或 tar.gz 路径（必填）")
	cmd.MarkFlagRequired("bundle")
	return cmd
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "打印版本信息",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Println("builder v0.1.0")
		},
	}
}
