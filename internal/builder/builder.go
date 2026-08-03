// Package builder 编排 builder 的完整构建管线：
// 容器内软件包下载与镜像清单+拉取+save（--mode all 时并行）→
// 渲染脚本 → 生成 manifest → 打包 tar.gz；并提供 bundle verify。
package builder

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"golang.org/x/sync/errgroup"

	"builder/internal/config"
	"builder/internal/images"
	"builder/internal/manifest"
	"builder/internal/mirror"
	"builder/internal/packages"
	"builder/internal/script"
)

// Options build 参数。
type Options struct {
	Config     *config.Config
	OS         string
	OSVersion  string
	Arch       string
	K8sVersion string
	Mirror     mirror.Mirror
	WorkDir    string
	OutDir     string
	// Mode 构建模式：packages=仅软件包 / images=仅镜像 / all=两者都构建（默认）。
	// CLI 默认填充 all；库调用方为空时按 all 处理。
	Mode string
	// SkipImages 跳过镜像阶段（等价 Mode=packages，兼容旧用法）。
	SkipImages bool
	// SkipAddons 跳过附加组件镜像（flannel/dashboard 等），仅核心镜像。
	SkipAddons bool
	// DryRun 仅演练管线，不执行真实下载/拉取。
	DryRun bool
	// DockerBin docker 命令路径，默认 "docker"；测试注入用。
	DockerBin string
	// KubeadmBin 可选 kubeadm 二进制路径；为空时镜像阶段从 dl.k8s.io 下载（测试注入用）。
	KubeadmBin string
	// Out 实时构建日志输出，默认 os.Stdout；测试注入用。
	Out io.Writer
}

// StepResult 单个构建步骤结果。
type StepResult struct {
	Name    string `json:"name"`
	Status  string `json:"status"` // ok / skipped / failed
	Message string `json:"message"`
}

// Result build 总结果。
type Result struct {
	BundleDir  string
	BundleName string
	// TarPath 单产物路径（packages/images 模式）；--mode all 时为软件包 tar（兼容旧字段）。
	TarPath string
	// TarPaths 全部产物路径；--mode all 时含 packages 与 images 两个独立 tar.gz。
	TarPaths []string
	Steps    []StepResult
}

// logf 向 w 输出带统一前缀 [builder] 的实时构建日志。
func logf(w io.Writer, format string, args ...interface{}) {
	fmt.Fprintf(w, "[builder] "+format+"\n", args...)
}

// Build 执行完整构建管线。
func Build(ctx context.Context, opts Options) (*Result, error) {
	if opts.Config == nil {
		return nil, fmt.Errorf("builder: Config 不能为空")
	}
	if opts.WorkDir == "" {
		opts.WorkDir = "./work"
	}
	if opts.OutDir == "" {
		opts.OutDir = "./dist"
	}
	if opts.Out == nil {
		opts.Out = os.Stdout
	}

	// 构建模式归一化：
	//   - 库调用方未填 Mode 时按 all 处理
	//   - SkipImages=true 且 Mode 为 all（或空）→ 视为 packages（--skip-images 兼容）
	//   - Mode 为 images 时强制清除 SkipImages（images 模式意愿为仅构建镜像，mode 优先）
	if opts.Mode == "" {
		opts.Mode = "all"
	}
	if opts.SkipImages && opts.Mode == "all" {
		opts.Mode = "packages"
	}
	if opts.Mode == "images" {
		opts.SkipImages = false
	}

	// --mode images 且未指定 OS 时：选用默认构建容器（仅用于容器内 kubeadm 拉清单），
	// bundle 名使用 images 前缀，不绑定具体发行版。
	osOmitted := opts.OS == "" && opts.OSVersion == ""
	if opts.Mode == "images" && osOmitted {
		name, ver, err := defaultBuildOS(opts.Config)
		if err != nil {
			return nil, err
		}
		opts.OS, opts.OSVersion = name, ver
	} else if (opts.OS == "") != (opts.OSVersion == "") {
		return nil, fmt.Errorf("--os 与 --os-version 需同时指定或同时省略")
	}

	// WorkDir/OutDir 转绝对路径：docker -v 挂载宿主机目录必须是绝对路径，
	// 相对路径（如 "./work" → work/pixiu-offline-.../packages）会被 docker 当作
	// 本地 volume 名校验而失败（含 "/" 非法）。统一转绝对路径也避免其他相对路径问题。
	if abs, err := filepath.Abs(opts.WorkDir); err != nil {
		return nil, fmt.Errorf("解析 WorkDir 绝对路径失败: %w", err)
	} else {
		opts.WorkDir = abs
	}
	if abs, err := filepath.Abs(opts.OutDir); err != nil {
		return nil, fmt.Errorf("解析 OutDir 绝对路径失败: %w", err)
	} else {
		opts.OutDir = abs
	}

	// 参数合法性校验
	if err := validateOptions(opts); err != nil {
		return nil, err
	}

	var bundleName string
	if opts.Mode == "images" && osOmitted {
		bundleName = ImagesBundleName(opts.Arch, opts.K8sVersion)
	} else {
		bundleName = BundleName(opts.OS, opts.OSVersion, opts.Arch, opts.K8sVersion)
	}
	bundleDir := filepath.Join(opts.WorkDir, bundleName)
	if err := makeDirs(bundleDir); err != nil {
		return nil, err
	}

	osDef, err := opts.Config.ResolveOS(opts.OS, opts.OSVersion)
	if err != nil {
		return nil, err
	}
	buildImage := osDef.BuildImage
	k8sMinor, err := config.K8sMinor(opts.K8sVersion)
	if err != nil {
		return nil, err
	}
	codename := osDef.Codename
	aptOS := aptFamily(osDef.Name)

	res := &Result{BundleDir: bundleDir, BundleName: bundleName}
	step := func(name, status, msg string) {
		res.Steps = append(res.Steps, StepResult{Name: name, Status: status, Message: msg})
	}

	// 实时日志 helper：仅新增输出，不改变管线语义。
	const totalSteps = 5
	var logMu sync.Mutex
	stepStart := func(n int, name string) {
		logMu.Lock()
		defer logMu.Unlock()
		logf(opts.Out, "步骤 %d/%d: %s ...", n, totalSteps, name)
	}
	stepDone := func(n int, suffix string) {
		logMu.Lock()
		defer logMu.Unlock()
		logf(opts.Out, "步骤 %d/%d: %s", n, totalSteps, suffix)
	}
	stepFail := func(n int, err error) (*Result, error) {
		logMu.Lock()
		defer logMu.Unlock()
		logf(opts.Out, "步骤 %d/%d: 失败：%v", n, totalSteps, err)
		return res, err
	}

	// ---------- Step 1/2: 软件包下载 + 镜像打包（mode=all 且两者均需真实执行时并行） ----------
	runPkg := opts.Mode != "images"
	runImg := opts.Mode != "packages" && !opts.SkipImages
	parallel := runPkg && runImg && !opts.DryRun

	type stepOut struct {
		sr  StepResult
		err error
	}
	doPackages := func(c context.Context) stepOut {
		if !runPkg {
			return stepOut{sr: StepResult{Name: "容器内软件包下载", Status: "skipped", Message: "按 --mode images 跳过软件包"}}
		}
		pkgList := packages.BuildPackageList(osDef.PkgManager, opts.K8sVersion, opts.Config.SystemDepsForOS(opts.OS), false)
		if opts.DryRun {
			return stepOut{sr: StepResult{Name: "容器内软件包下载", Status: "ok", Message: "dry-run"}}
		}
		pkgRes, err := packages.Fetch(c, packages.Options{
			OutDir:        filepath.Join(bundleDir, "packages"),
			BuildImage:    buildImage,
			PkgManager:    osDef.PkgManager,
			K8sMinor:      k8sMinor,
			Codename:      codename,
			RPMDistro:     osDef.RPMDistro,
			AptOS:         aptOS,
			Pkgs:          pkgList,
			CrictlVersion: opts.Config.CrictlVersionFor(opts.K8sVersion),
			Arch:          opts.Arch,
			DockerBin:     opts.DockerBin,
		})
		if err != nil {
			return stepOut{err: fmt.Errorf("[容器内软件包下载] 失败: %w", err)}
		}
		if pkgRes.Skipped {
			return stepOut{err: fmt.Errorf("[容器内软件包下载] 中断: %s", pkgRes.SkipReason)}
		}
		msg := fmt.Sprintf("收集 %d 个软件包", len(pkgRes.Files))
		if pkgRes.CrictlMissing {
			msg += "（cri-tools 包不可用，已回退下载 crictl 静态 tar）"
		}
		return stepOut{sr: StepResult{Name: "容器内软件包下载", Status: "ok", Message: msg}}
	}
	doImages := func(c context.Context) stepOut {
		if !runImg {
			return stepOut{sr: StepResult{Name: "镜像清单与保存", Status: "skipped", Message: "按 --mode packages 跳过镜像"}}
		}
		if opts.DryRun {
			return stepOut{sr: StepResult{Name: "镜像清单与保存", Status: "ok", Message: "dry-run"}}
		}
		if opts.SkipImages {
			return stepOut{sr: StepResult{Name: "镜像清单与保存", Status: "skipped", Message: "按配置跳过"}}
		}
		imgRes, err := images.Fetch(c, images.Options{
			BuildImage:      buildImage,
			PkgManager:      osDef.PkgManager,
			K8sMinor:        k8sMinor,
			Codename:        codename,
			RPMDistro:       osDef.RPMDistro,
			AptOS:           aptOS,
			K8sVersion:      opts.K8sVersion,
			ImageRepository: opts.Mirror.ImageRepository(),
			Arch:            opts.Arch,
			KubeadmBin:      opts.KubeadmBin,
			Addons:          opts.Config.Addons.Addons,
			SkipAddons:      opts.SkipAddons,
			ImagesOutDir:    filepath.Join(bundleDir, "images"),
			DockerBin:       opts.DockerBin,
		})
		if err != nil {
			return stepOut{err: fmt.Errorf("[镜像清单与保存] 失败: %w", err)}
		}
		if imgRes.Skipped {
			return stepOut{err: fmt.Errorf("[镜像清单与保存] 中断: %s", imgRes.SkipReason)}
		}
		msg := fmt.Sprintf("核心 %d 个 + addons %d 个", len(imgRes.Core), len(imgRes.Addons))
		if imgRes.SkipAddons {
			msg += "（按 --skip-addons 跳过附加组件）"
		}
		if imgRes.ArchMismatch {
			msg += fmt.Sprintf("（注意: 目标 arch=%s 与本机 %s 不同，镜像按本机架构拉取）", imgRes.Arch, imgRes.HostArch)
		}
		return stepOut{sr: StepResult{Name: "镜像清单与保存", Status: "ok", Message: msg}}
	}

	recordStep := func(n int, out stepOut) (*Result, error) {
		if out.err != nil {
			return stepFail(n, out.err)
		}
		step(out.sr.Name, out.sr.Status, out.sr.Message)
		suffix := "完成（" + out.sr.Message + "）"
		if out.sr.Status == "skipped" {
			suffix = "跳过（" + out.sr.Message + "）"
		}
		stepDone(n, suffix)
		return nil, nil
	}

	if parallel {
		logf(opts.Out, "步骤 1–2/%d: 软件包下载与镜像打包并行执行 ...", totalSteps)
		stepStart(1, "容器内软件包下载")
		stepStart(2, "镜像清单与保存")
		g, gctx := errgroup.WithContext(ctx)
		var pkgOut, imgOut stepOut
		g.Go(func() error {
			pkgOut = doPackages(gctx)
			return pkgOut.err
		})
		g.Go(func() error {
			imgOut = doImages(gctx)
			return imgOut.err
		})
		_ = g.Wait()
		if pkgOut.err != nil {
			return stepFail(1, pkgOut.err)
		}
		if imgOut.err != nil {
			return stepFail(2, imgOut.err)
		}
		if _, err := recordStep(1, pkgOut); err != nil {
			return res, err
		}
		if _, err := recordStep(2, imgOut); err != nil {
			return res, err
		}
	} else {
		stepStart(1, "容器内软件包下载")
		if r, err := recordStep(1, doPackages(ctx)); err != nil {
			return r, err
		}
		stepStart(2, "镜像清单与保存")
		if r, err := recordStep(2, doImages(ctx)); err != nil {
			return r, err
		}
	}

	// ---------- Step 3: 渲染脚本 ----------
	stepStart(3, "渲染脚本")
	installDir := filepath.Join(bundleDir, "install")
	if _, err := script.WriteDir(installDir, script.Data{
		K8sVersion:      opts.K8sVersion,
		ImageRepository: opts.Mirror.ImageRepository(),
	}); err != nil {
		return stepFail(3, fmt.Errorf("[渲染脚本] 失败: %w", err))
	}
	step("渲染脚本", "ok", "install.sh + load-images.sh")
	stepDone(3, "完成（install.sh + load-images.sh）")

	// ---------- Step 4: 生成 manifest ----------
	stepStart(4, "生成 manifest")
	metaOS, metaOSVer := opts.OS, opts.OSVersion
	if opts.Mode == "images" && osOmitted {
		metaOS, metaOSVer = "images", "-"
	}
	meta := manifest.Meta{
		OS:         metaOS,
		OSVersion:  metaOSVer,
		Arch:       opts.Arch,
		K8sVersion: opts.K8sVersion,
		Mirror:     opts.Mirror.String(),
		HostArch:   hostArch(),
	}

	var packTargets []packTarget // 待打包的独立 bundle（目录名 = tar 顶层名）
	if opts.Mode == "all" {
		// all：拆成两个独立 bundle（packages / images），各自带脚本与 manifest
		pkgName := PackagesBundleName(opts.OS, opts.OSVersion, opts.Arch, opts.K8sVersion)
		imgName := ImagesOfflineBundleName(opts.OS, opts.OSVersion, opts.Arch, opts.K8sVersion)
		pkgDir := filepath.Join(opts.WorkDir, pkgName)
		imgDir := filepath.Join(opts.WorkDir, imgName)
		if err := materializeSplitBundle(bundleDir, pkgDir, []string{"packages"}, meta); err != nil {
			return stepFail(4, fmt.Errorf("[生成 manifest] 软件包 bundle 失败: %w", err))
		}
		if err := materializeSplitBundle(bundleDir, imgDir, []string{"images"}, meta); err != nil {
			return stepFail(4, fmt.Errorf("[生成 manifest] 镜像 bundle 失败: %w", err))
		}
		// 合并工作目录仍写一份完整 manifest，便于排查
		if m, err := manifest.Generate(bundleDir, meta); err == nil {
			_ = m.Write(filepath.Join(bundleDir, manifest.ManifestFileName))
		}
		packTargets = []packTarget{
			{Dir: pkgDir, Name: pkgName},
			{Dir: imgDir, Name: imgName},
		}
		step("生成 manifest", "ok", "packages + images 各一份")
		stepDone(4, "完成（packages + images 独立 manifest）")
	} else {
		m, err := manifest.Generate(bundleDir, meta)
		if err != nil {
			return stepFail(4, fmt.Errorf("[生成 manifest] 失败: %w", err))
		}
		if err := m.Write(filepath.Join(bundleDir, manifest.ManifestFileName)); err != nil {
			return stepFail(4, err)
		}
		msg4 := fmt.Sprintf("%d files, %d images, %d scripts", len(m.Files), len(m.Images), len(m.Scripts))
		step("生成 manifest", "ok", msg4)
		stepDone(4, "完成（"+msg4+"）")
		packTargets = []packTarget{{Dir: bundleDir, Name: bundleName}}
	}

	// ---------- Step 5: 打包 tar.gz ----------
	stepStart(5, "打包 tar.gz")
	if err := os.MkdirAll(opts.OutDir, 0o755); err != nil {
		return stepFail(5, err)
	}
	var tarPaths []string
	for _, t := range packTargets {
		tarPath := filepath.Join(opts.OutDir, t.Name+".tar.gz")
		if err := TarGz(t.Dir, tarPath); err != nil {
			return stepFail(5, fmt.Errorf("[打包 tar.gz] %s 失败: %w", t.Name, err))
		}
		tarPaths = append(tarPaths, tarPath)
	}
	res.TarPaths = tarPaths
	if len(tarPaths) > 0 {
		res.TarPath = tarPaths[0]
	}
	msg5 := strings.Join(tarPaths, ", ")
	step("打包 tar.gz", "ok", msg5)
	stepDone(5, "完成（"+msg5+"）")

	return res, nil
}

// packTarget 单个待打 tar.gz 的 bundle 目录。
type packTarget struct {
	Dir  string
	Name string
}

// BundleName 生成完整/单模式 bundle 目录名。
func BundleName(osName, osVer, arch, k8sVer string) string {
	return fmt.Sprintf("pixiu-offline-%s-%s-%s-%s", osName, osVer, arch, k8sVer)
}

// PackagesBundleName / ImagesOfflineBundleName：--mode all 拆分后的独立产物名。
func PackagesBundleName(osName, osVer, arch, k8sVer string) string {
	return BundleName(osName, osVer, arch, k8sVer) + "-packages"
}

func ImagesOfflineBundleName(osName, osVer, arch, k8sVer string) string {
	return BundleName(osName, osVer, arch, k8sVer) + "-images"
}

// materializeSplitBundle 从合并构建目录复制指定子目录 + install，并生成独立 manifest。
func materializeSplitBundle(srcBundle, dstBundle string, parts []string, meta manifest.Meta) error {
	if err := os.RemoveAll(dstBundle); err != nil {
		return err
	}
	if err := os.MkdirAll(dstBundle, 0o755); err != nil {
		return err
	}
	for _, part := range parts {
		src := filepath.Join(srcBundle, part)
		dst := filepath.Join(dstBundle, part)
		if _, err := os.Stat(src); err != nil {
			// 允许源侧为空：仍创建空目录，保持与单模式结构一致
			if err := os.MkdirAll(dst, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := copyDir(src, dst); err != nil {
			return fmt.Errorf("复制 %s 失败: %w", part, err)
		}
	}
	installSrc := filepath.Join(srcBundle, "install")
	if _, err := os.Stat(installSrc); err == nil {
		if err := copyDir(installSrc, filepath.Join(dstBundle, "install")); err != nil {
			return fmt.Errorf("复制 install 失败: %w", err)
		}
	}
	m, err := manifest.Generate(dstBundle, meta)
	if err != nil {
		return err
	}
	return m.Write(filepath.Join(dstBundle, manifest.ManifestFileName))
}

// copyDir 递归复制目录树。
func copyDir(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, info.Mode())
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode())
		if err != nil {
			return err
		}
		defer out.Close()
		_, err = io.Copy(out, in)
		return err
	})
}

// ImagesBundleName 生成仅镜像模式（未指定 OS）的 bundle 目录名。
func ImagesBundleName(arch, k8sVer string) string {
	return fmt.Sprintf("pixiu-offline-images-%s-%s", arch, k8sVer)
}

// defaultBuildOS 为 --mode images 未指定 OS 时挑选默认构建容器发行版。
// 优先 ubuntu/22.04；否则取 ubuntu 第一个版本；再否则取清单中第一个 OS 的第一个版本。
func defaultBuildOS(cfg *config.Config) (name, version string, err error) {
	if cfg == nil {
		return "", "", fmt.Errorf("配置为空，无法选择默认构建 OS")
	}
	if osDef, ok := cfg.FindOS("ubuntu"); ok && len(osDef.Versions) > 0 {
		for _, v := range osDef.Versions {
			if v == "22.04" {
				return "ubuntu", "22.04", nil
			}
		}
		return "ubuntu", osDef.Versions[0], nil
	}
	if len(cfg.OSRegistry.OSes) == 0 || len(cfg.OSRegistry.OSes[0].Versions) == 0 {
		return "", "", fmt.Errorf("配置中无可用 OS，无法选择默认构建镜像")
	}
	osDef := cfg.OSRegistry.OSes[0]
	return osDef.Name, osDef.Versions[0], nil
}

// aptFamily 返回 apt 发行版家族（用于 containerd 源路径：ubuntu/debian）。
func aptFamily(osName string) string {
	if osName == "debian" {
		return "debian"
	}
	return "ubuntu"
}

// validateOptions 校验 build 参数合法性。
func validateOptions(opts Options) error {
	// Mode 为空视为默认 all（Build 已归一化；直接调用 validateOptions 时兜底）。
	mode := opts.Mode
	if mode == "" {
		mode = "all"
	}
	switch mode {
	case "all", "packages", "images":
	default:
		return fmt.Errorf("非法的构建模式 %q（可选: packages=仅软件包 / images=仅镜像 / all=两者都构建）", opts.Mode)
	}
	// packages/all 必须指定 OS；images 模式可在 Build 中先填入默认 OS 后再校验。
	if mode != "images" && (opts.OS == "" || opts.OSVersion == "") {
		return fmt.Errorf("缺少 OS/版本（--mode images 时可省略）")
	}
	if opts.OS != "" || opts.OSVersion != "" {
		if opts.OS == "" || opts.OSVersion == "" {
			return fmt.Errorf("--os 与 --os-version 需同时指定")
		}
		// OS/版本任意（与 k8s 任意 vX.Y.Z 一致）；builder.yaml oses 仅作参考与默认推导覆盖
		if opts.Arch != "" && !config.ValidArch(opts.Arch) {
			return fmt.Errorf("不支持的架构 %s（可选 amd64/arm64）", opts.Arch)
		}
	}
	if !opts.Config.ValidK8s(opts.K8sVersion) {
		return fmt.Errorf("非法的 k8s 版本格式: %s（期望形如 v1.31.0，如 v1.29.5；支持任意 vX.Y.Z）", opts.K8sVersion)
	}
	if !opts.Mirror.IsSupported() {
		return fmt.Errorf("镜像源 %s 尚未完整实现，仅支持 official（%s）", opts.Mirror, opts.Mirror.Note())
	}
	return nil
}

func makeDirs(bundleDir string) error {
	dirs := []string{
		bundleDir,
		filepath.Join(bundleDir, "packages"),
		filepath.Join(bundleDir, "packages", "runtime"), // crictl 静态回退目录
		filepath.Join(bundleDir, "images", "core"),
		filepath.Join(bundleDir, "images", "addons"),
		filepath.Join(bundleDir, "install"),
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return fmt.Errorf("创建目录 %s 失败: %w", d, err)
		}
	}
	return nil
}

// hostArch 返回宿主机架构。
func hostArch() string {
	return runtime.GOARCH
}

// Verify 校验 bundle（目录或 tar.gz）。
// 返回解析后的 manifest。
func Verify(bundle string) (*manifest.Manifest, error) {
	info, err := os.Stat(bundle)
	if err != nil {
		return nil, fmt.Errorf("bundle 路径不存在: %s (%v)", bundle, err)
	}

	root := bundle
	tmpDir := ""
	if info.IsDir() {
		mfPath := filepath.Join(root, manifest.ManifestFileName)
		if _, err := os.Stat(mfPath); err != nil {
			return nil, fmt.Errorf("目录 %s 下未找到 manifest.yaml", bundle)
		}
	} else if strings.HasSuffix(bundle, ".tar.gz") {
		tmpDir, err = os.MkdirTemp("", "pixiu-offline-verify-*")
		if err != nil {
			return nil, fmt.Errorf("创建临时目录失败: %w", err)
		}
		defer os.RemoveAll(tmpDir)
		if err := UntarGz(bundle, tmpDir); err != nil {
			return nil, fmt.Errorf("解压 bundle 失败: %w", err)
		}
		root = findManifestDir(tmpDir)
		if root == "" {
			return nil, fmt.Errorf("tar.gz 中未找到 manifest.yaml")
		}
	} else {
		return nil, fmt.Errorf("bundle 必须是目录或 .tar.gz 文件: %s", bundle)
	}

	m, err := manifest.Load(filepath.Join(root, manifest.ManifestFileName))
	if err != nil {
		return nil, err
	}
	if err := m.Verify(root); err != nil {
		return nil, err
	}
	return m, nil
}

// findManifestDir 在解压目录中查找包含 manifest.yaml 的目录。
func findManifestDir(root string) string {
	var found string
	filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || found != "" {
			return nil
		}
		if !info.IsDir() && info.Name() == manifest.ManifestFileName {
			found = filepath.Dir(path)
		}
		return nil
	})
	return found
}

// TarGz 将目录打包为 tar.gz，tar 内顶层目录名为 src 的 base name。
func TarGz(srcDir, tarPath string) error {
	out, err := os.Create(tarPath)
	if err != nil {
		return fmt.Errorf("创建 tar.gz 失败: %w", err)
	}
	defer out.Close()

	gw := gzip.NewWriter(out)
	defer gw.Close()
	tw := tar.NewWriter(gw)
	defer tw.Close()

	base := filepath.Base(srcDir)
	err = filepath.Walk(srcDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(srcDir, path)
		if err != nil {
			return err
		}
		name := filepath.ToSlash(filepath.Join(base, rel))

		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = name
		if info.IsDir() {
			hdr.Name += "/"
		}

		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if !info.IsDir() {
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			defer f.Close()
			if _, err := io.Copy(tw, f); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	return nil
}

// UntarGz 解压 tar.gz 到 destDir，校验路径防目录穿越。
func UntarGz(tarPath, destDir string) error {
	f, err := os.Open(tarPath)
	if err != nil {
		return err
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		name := filepath.FromSlash(hdr.Name)
		target := filepath.Join(destDir, name)
		// 防目录穿越
		if !strings.HasPrefix(target, filepath.Clean(destDir)+string(os.PathSeparator)) && target != filepath.Clean(destDir) {
			return fmt.Errorf("tar 包含非法路径: %s", hdr.Name)
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
			of, err := os.Create(target)
			if err != nil {
				return err
			}
			if _, err := io.Copy(of, tr); err != nil {
				of.Close()
				return err
			}
			of.Close()
		}
	}
	return nil
}

// StepsTable 汇总各步骤状态，便于日志打印。
func (r *Result) StepsTable() string {
	var b strings.Builder
	b.WriteString("\n构建步骤汇总:\n")
	for _, s := range r.Steps {
		b.WriteString(fmt.Sprintf("  [%-8s] %s\n", s.Status, s.Name))
		if s.Message != "" && s.Status != "ok" {
			b.WriteString(fmt.Sprintf("             %s\n", s.Message))
		}
	}
	return b.String()
}
