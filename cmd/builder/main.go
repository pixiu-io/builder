// builder 是一个制作 Kubernetes 离线安装包（含安装包与镜像）的 CLI 工具。
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"builder/internal/builder"
	"builder/internal/config"
	"builder/internal/mirror"
	"builder/internal/s3upload"
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
	buildSkipImages bool
	buildSkipAddons bool
	buildDryRun     bool
	buildUpload     bool
	buildS3Bucket   string
	buildS3Prefix   string
	buildS3Endpoint string
	buildS3Region   string
)

// upload 子命令 flags
var (
	uploadFiles      []string
	uploadS3Bucket   string
	uploadS3Prefix   string
	uploadS3Endpoint string
	uploadS3Region   string
)

// verify 子命令 flags
var verifyBundle string

// serve 子命令 flags
var (
	serveBundles       []string
	serveDataDir       string
	serveRegistryAddr  string
	serveRepoAddr      string
	serveAdvertiseHost string
	serveSkipImages    bool
	serveSkipPackages  bool
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
	root.AddCommand(newServeCmd())
	root.AddCommand(newListOSCmd())
	root.AddCommand(newListK8sCmd())
	root.AddCommand(newListImagesCmd())
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
  builder build --mode images --kubernetes-version v1.27.3 --arch amd64 --out ./dist`,
		RunE: runBuild,
	}
	cmd.Flags().StringVar(&buildOS, "os", "", "目标操作系统（任意，如 ubuntu；--mode images 时可省略）")
	cmd.Flags().StringVar(&buildOSVersion, "os-version", "", "操作系统版本（任意，如 22.04；--mode images 时可省略）")
	cmd.Flags().StringVar(&buildK8sVersion, "kubernetes-version", "", "k8s 版本（必填，如 v1.27.3）")
	cmd.Flags().StringVar(&buildArch, "arch", "amd64", "目标架构（amd64/arm64）")
	cmd.Flags().StringVar(&buildMirror, "mirror", "official", "镜像仓库源（official/aliyun/tencent，当前仅 official 完整实现，仅影响镜像阶段）")
	cmd.Flags().StringVar(&buildWorkDir, "workdir", "./work", "工作目录（bundle 在此构建）")
	cmd.Flags().StringVar(&buildOutDir, "out", "./dist", "产物输出目录（tar.gz 输出到此）")
	cmd.Flags().StringVar(&buildMode, "mode", "all", "构建模式：packages=仅软件包 / images=仅镜像 / all=两者都构建（默认）")
	cmd.Flags().BoolVar(&buildSkipImages, "skip-images", false, "跳过镜像阶段（等价 --mode packages，兼容旧用法）")
	cmd.Flags().BoolVar(&buildSkipAddons, "skip-addons", false, "跳过附加组件镜像（flannel/dashboard 等），仅核心镜像")
	cmd.Flags().BoolVar(&buildDryRun, "dry-run", false, "仅演练管线，不执行真实下载/拉取")
	cmd.Flags().BoolVar(&buildUpload, "upload", false, "构建完成后将产物 tar.gz 上传到 S3（需配置 s3.bucket 或 --s3-bucket）")
	cmd.Flags().StringVar(&buildS3Bucket, "s3-bucket", "", "S3 bucket（覆盖配置文件 s3.bucket）")
	cmd.Flags().StringVar(&buildS3Prefix, "s3-prefix", "", "S3 对象键前缀（覆盖配置文件 s3.prefix）")
	cmd.Flags().StringVar(&buildS3Endpoint, "s3-endpoint", "", "S3 兼容 endpoint（如 MinIO，覆盖配置文件 s3.endpoint）")
	cmd.Flags().StringVar(&buildS3Region, "s3-region", "", "S3 region（覆盖配置文件 s3.region）")
	cmd.MarkFlagRequired("kubernetes-version")
	return cmd
}

func runBuild(cmd *cobra.Command, args []string) error {
	cfg, err := config.Load(configFile)
	if err != nil {
		return err
	}

	mirrorVal, err := mirror.ParseMirror(buildMirror)
	if err != nil {
		return err
	}

	if buildK8sVersion == "" {
		return fmt.Errorf("缺少 --kubernetes-version")
	}

	// 构建模式合并：--mode 与 --skip-images 兼容
	//   --skip-images（配合默认 --mode all）→ 视为 packages
	//   --mode 非 all 且 --skip-images 同时设置 → mode 优先
	mode := buildMode
	if buildSkipImages && mode == "all" {
		mode = "packages"
	}
	switch mode {
	case "packages", "images", "all":
	default:
		return fmt.Errorf("非法 --mode 取值 %q（可选: packages=仅软件包 / images=仅镜像 / all=两者都构建）", buildMode)
	}
	skipImages := buildSkipImages
	if mode == "images" {
		// --mode images 表示仅构建镜像，与 --skip-images 意愿相反；按 mode 优先忽略 --skip-images
		skipImages = false
	}

	// --mode images 可不指定 OS；packages/all 必须指定
	if mode != "images" {
		if buildOS == "" || buildOSVersion == "" {
			return fmt.Errorf("--os 与 --os-version 为必填（仅 --mode images 时可省略）")
		}
	} else if (buildOS == "") != (buildOSVersion == "") {
		return fmt.Errorf("--mode images 下 --os 与 --os-version 需同时指定或同时省略")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	res, err := builder.Build(ctx, builder.Options{
		Config:     cfg,
		OS:         buildOS,
		OSVersion:  buildOSVersion,
		Arch:       buildArch,
		K8sVersion: buildK8sVersion,
		Mirror:     mirrorVal,
		WorkDir:    buildWorkDir,
		OutDir:     buildOutDir,
		Mode:       mode,
		SkipImages: skipImages,
		SkipAddons: buildSkipAddons,
		DryRun:     buildDryRun,
	})
	if err != nil {
		return err
	}

	fmt.Println(res.StepsTable())
	if buildDryRun {
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
		if buildDryRun {
			fmt.Println("[dry-run] 跳过 S3 上传")
			return nil
		}
		files := res.TarPaths
		if len(files) == 0 && res.TarPath != "" {
			files = []string{res.TarPath}
		}
		uploaded, err := uploadArtifacts(ctx, cfg, files, buildS3Bucket, buildS3Prefix, buildS3Endpoint, buildS3Region)
		if err != nil {
			return err
		}
		fmt.Println("S3 上传完成：")
		for _, u := range uploaded {
			fmt.Printf("  - %s → %s\n", u.LocalPath, u.URI)
		}
	}
	return nil
}

func newUploadCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "upload",
		Short: "将已有离线包 tar.gz 上传到 S3",
		Example: `  builder upload --file ./dist/pixiu-offline-ubuntu-22.04-amd64-v1.27.3-packages.tar.gz
  builder upload --file a.tar.gz --file b.tar.gz --s3-bucket my-bucket --s3-prefix releases/`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(configFile)
			if err != nil {
				return err
			}
			if len(uploadFiles) == 0 {
				return fmt.Errorf("请通过 --file 指定至少一个 tar.gz")
			}
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			uploaded, err := uploadArtifacts(ctx, cfg, uploadFiles, uploadS3Bucket, uploadS3Prefix, uploadS3Endpoint, uploadS3Region)
			if err != nil {
				return err
			}
			fmt.Println("S3 上传完成：")
			for _, u := range uploaded {
				fmt.Printf("  - %s → %s\n", u.LocalPath, u.URI)
			}
			return nil
		},
	}
	cmd.Flags().StringArrayVar(&uploadFiles, "file", nil, "待上传的本地文件（可重复）")
	cmd.Flags().StringVar(&uploadS3Bucket, "s3-bucket", "", "S3 bucket（覆盖配置文件 s3.bucket）")
	cmd.Flags().StringVar(&uploadS3Prefix, "s3-prefix", "", "S3 对象键前缀（覆盖配置文件 s3.prefix）")
	cmd.Flags().StringVar(&uploadS3Endpoint, "s3-endpoint", "", "S3 兼容 endpoint（覆盖配置文件 s3.endpoint）")
	cmd.Flags().StringVar(&uploadS3Region, "s3-region", "", "S3 region（覆盖配置文件 s3.region）")
	return cmd
}

func newServeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "加载离线产物，提供 docker pull（短名）与 yum/dnf/apt 软件源",
		Long: `将 builder 产物（目录或 tar.gz）加载后常驻服务：
  - OCI registry：docker pull <host>:5000/<短名>:<tag>
  - HTTP 软件源：dnf/yum 使用 /rpm，apt 使用 /deb

纯 Go 实现，不依赖 createrepo / apt-ftparchive 等外部工具。`,
		Example: `  builder serve --bundle ./dist/pixiu-offline-centos-8-amd64-v1.27.3-packages.tar.gz \
                --bundle ./dist/pixiu-offline-centos-8-amd64-v1.27.3-images.tar.gz
  builder serve --bundle ./work/pixiu-offline-ubuntu-22.04-amd64-v1.27.3 --advertise-host 192.168.1.10`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(serveBundles) == 0 {
				return fmt.Errorf("请通过 --bundle 指定至少一个离线包目录或 tar.gz")
			}
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			_, err := serve.Run(ctx, serve.Options{
				Bundles:       serveBundles,
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
	cmd.Flags().StringVar(&serveDataDir, "data-dir", "./serve-data", "工作目录（解压、repodata、registry blob）")
	cmd.Flags().StringVar(&serveRegistryAddr, "registry-addr", "0.0.0.0:5000", "OCI registry 监听地址")
	cmd.Flags().StringVar(&serveRepoAddr, "repo-addr", "0.0.0.0:8080", "软件源 HTTP 监听地址")
	cmd.Flags().StringVar(&serveAdvertiseHost, "advertise-host", "127.0.0.1", "打印给客户端的主机名/IP（不含端口）")
	cmd.Flags().BoolVar(&serveSkipImages, "skip-images", false, "不提供镜像 registry")
	cmd.Flags().BoolVar(&serveSkipPackages, "skip-packages", false, "不提供软件源")
	return cmd
}

// uploadArtifacts 合并配置与 CLI 覆盖后上传文件。
func uploadArtifacts(ctx context.Context, cfg *config.Config, files []string, bucket, prefix, endpoint, region string) ([]s3upload.Result, error) {
	opts := mergeS3Options(cfg.S3, bucket, prefix, endpoint, region)
	fmt.Printf("上传到 s3://%s/%s （共 %d 个文件）...\n", opts.Bucket, strings.TrimSuffix(opts.Prefix, "/"), len(files))
	return s3upload.UploadFiles(ctx, opts, files)
}

func mergeS3Options(cfg config.S3Config, bucket, prefix, endpoint, region string) s3upload.Options {
	opts := s3upload.Options{
		Bucket:         cfg.Bucket,
		Region:         cfg.Region,
		Endpoint:       cfg.Endpoint,
		Prefix:         cfg.Prefix,
		ForcePathStyle: cfg.ForcePathStyle,
	}
	if bucket != "" {
		opts.Bucket = bucket
	}
	if prefix != "" {
		opts.Prefix = prefix
	}
	if endpoint != "" {
		opts.Endpoint = endpoint
	}
	if region != "" {
		opts.Region = region
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
			fmt.Println("示例: --os ubuntu --os-version 24.04（构建镜像默认 ubuntu:24.04）")
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
			for _, a := range cfg.Addons.Addons {
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

func newVerifyCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "verify",
		Short: "校验离线安装包完整性（目录或 tar.gz）",
		Example: `  builder verify --bundle ./dist/pixiu-offline-ubuntu-22.04-amd64-v1.27.3.tar.gz
  builder verify --bundle ./work/pixiu-offline-ubuntu-22.04-amd64-v1.27.3`,
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
