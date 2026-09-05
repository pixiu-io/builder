// Package images 生成核心镜像清单（kubeadm config images list），
// 并 pull + save 核心镜像与附加组件镜像为 docker-save tar 文件。
// runtime=docker：在 PackImage 容器内经 docker.sock 操作；
// runtime=containerd（默认）：宿主机 ctr pull/export 再转为 docker-save。
package images

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unicode"

	"builder/internal/config"
	rt "builder/internal/runtime"
)

// 默认镜像打包容器（仅 docker 模式；含 docker CLI，通过挂载的 sock 操作宿主机 daemon）。
const defaultPackImage = "swr.cn-north-4.myhuaweicloud.com/pixiu-public/pixiukit/docker:24-cli"

// 容器名前缀。
const (
	containerNameImagesPack = "builder-images"
	containerNameImagesList = "builder-images-list"
)

// uniqueContainerName 生成带阶段标识的唯一容器名。
func uniqueContainerName(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
}

// Options 镜像阶段配置。
type Options struct {
	// Runtime 容器运行时：containerd（默认）或 docker。
	Runtime string
	// DockerBin docker 命令路径，默认 "docker"。
	DockerBin string
	// CtrBin ctr 命令路径，默认 "ctr"。
	CtrBin string
	// DockerSock 宿主机 docker socket，默认 /var/run/docker.sock；仅 docker 模式挂入打包容器。
	DockerSock string
	// ContainerdAddress ctr --address，默认 /run/containerd/containerd.sock。
	ContainerdAddress string
	// BuildImage 构建容器镜像，仅在宿主机无法直接执行 kubeadm 时用于挂载二进制跑 images list。
	BuildImage string
	// PackImage 镜像打包容器（含 docker CLI），仅 docker 模式；默认 pixiukit/docker:24-cli。
	PackImage string
	// PkgManager 包管理器：apt 或 dnf（保留字段，镜像清单阶段已不再使用）。
	PkgManager string
	// K8sMinor k8s 大版本（v1.27），保留字段。
	K8sMinor string
	// Codename apt 版本代号（jammy 等），保留字段。
	Codename string
	// RPMDistro dnf 发行版标识（rhel9 等），保留字段。
	RPMDistro string
	// AptOS apt 发行版家族（ubuntu/debian），保留字段。
	AptOS string
	// K8sVersion k8s 版本，如 v1.27.3。
	K8sVersion string
	// ImageRepository 核心镜像仓库，默认 registry.k8s.io。
	ImageRepository string
	// Arch 目标架构，用于与宿主机架构比对并提示 warning。
	Arch string
	// KubeadmBin 可选：已有 kubeadm 二进制路径（测试注入）；为空时从 dl.k8s.io 下载。
	KubeadmBin string
	// KubeadmBaseURL kubeadm 下载基址，默认 https://dl.k8s.io/release。
	KubeadmBaseURL string
	// KubeadmMode kubeadm 获取模式：local=本地下载（默认）/ remote=ssh 远端下载+拷回。
	KubeadmMode string
	// KubeadmRemoteHost remote 模式远端服务器（user@host，免密登录）。
	KubeadmRemoteHost string
	// KubeadmRemotePath remote 模式远端缓存目录，默认 ~/.builder-kubeadm（含 {version}/{arch} 子目录）。
	KubeadmRemotePath string
	// Verbose 打印详细过程日志（下载 kubeadm、镜像 pull/save 进度）；默认 false=精简。
	Verbose bool
	// CoreImages 外部传入的最终核心镜像完整引用清单（已解析）。
	CoreImages []string
	// CoreFilter 外部传入的核心镜像过滤项（短名或完整引用）。
	CoreFilter []string
	// Addons 外部传入的最终附加组件镜像清单（可为空，表示不拉取附加组件）。
	Addons []config.Addon
	// SkipAddons 跳过附加组件镜像拉取（仅核心镜像）。
	SkipAddons bool
	// ImagesOutDir bundle 内 images 目录（含 core/addons 子目录）。
	ImagesOutDir string
	// DryRun 只构造命令不执行。
	DryRun bool
}

// SavedImage 单个已保存的镜像 tar。
type SavedImage struct {
	Name        string `json:"name"`
	SourceImage string `json:"source_image"`
	TarPath     string `json:"tar_path"`
	Size        int64  `json:"size"`
	SHA256      string `json:"sha256"`
}

// Result 镜像阶段结果。
type Result struct {
	// CoreImages kubeadm 生成的核心镜像清单。
	CoreImages []string
	Core       []SavedImage
	Addons     []SavedImage
	Skipped    bool
	SkipReason string
	// SkipAddons 按 --skip-addons 显式跳过附加组件镜像拉取（核心镜像仍完整）。
	SkipAddons bool
	// Arch 目标架构（用户指定）。
	Arch string
	// HostArch 宿主机架构（用于跨架构 warning）。
	HostArch string
	// ArchMismatch 目标架构与宿主机不一致。
	ArchMismatch bool
}

// saveJob 容器内 pull + save 的单项任务。
type saveJob struct {
	Name   string // tar 基名（不含 .tar）
	Image  string // 完整镜像引用
	SubDir string // core / addons
}

// DockerAvailable 检查 docker 可用性，返回 (可用, 提示信息)。
// 保留供旧调用方；新代码请用 runtime.Available。
func DockerAvailable(bin string) (bool, string) {
	ok, msg := rt.Available(rt.Config{Runtime: rt.Docker, DockerBin: bin})
	if !ok {
		return false, fmt.Sprintf("%s（镜像阶段将被跳过）", msg)
	}
	return true, ""
}

func runtimeConfig(opts Options) (rt.Config, string, error) {
	resolved, err := rt.Resolve(opts.Runtime, opts.DockerBin)
	if err != nil {
		return rt.Config{}, "", err
	}
	cfg := rt.Config{
		Runtime:           resolved,
		DockerBin:         opts.DockerBin,
		CtrBin:            opts.CtrBin,
		DockerSock:        opts.DockerSock,
		ContainerdAddress: opts.ContainerdAddress,
		PackImage:         opts.PackImage,
	}
	return cfg, resolved, nil
}

// Fetch 执行核心镜像清单生成 + 拉取 + save。
func Fetch(ctx context.Context, opts Options) (*Result, error) {
	rtCfg, _, err := runtimeConfig(opts)
	if err != nil {
		return nil, err
	}
	if opts.ImageRepository == "" {
		opts.ImageRepository = "registry.k8s.io"
	}
	if opts.PackImage == "" {
		opts.PackImage = defaultPackImage
		rtCfg.PackImage = defaultPackImage
	}
	if opts.DockerSock == "" {
		opts.DockerSock = rt.DefaultDockerSock
		rtCfg.DockerSock = rt.DefaultDockerSock
	}

	res := &Result{HostArch: runtime.GOARCH, ArchMismatch: opts.Arch != "" && opts.Arch != runtime.GOARCH}
	if res.ArchMismatch {
		res.Arch = opts.Arch
	}

	if opts.DryRun {
		os.MkdirAll(filepath.Join(opts.ImagesOutDir, "core"), 0o755)
		os.MkdirAll(filepath.Join(opts.ImagesOutDir, "addons"), 0o755)
		return res, nil
	}

	if ok, reason := rt.Available(rtCfg); !ok {
		res.Skipped = true
		res.SkipReason = reason + "（镜像阶段将被跳过）"
		return res, nil
	}

	absOut, err := filepath.Abs(opts.ImagesOutDir)
	if err != nil {
		return res, fmt.Errorf("解析 ImagesOutDir 绝对路径失败: %w", err)
	}
	opts.ImagesOutDir = absOut
	if err := os.MkdirAll(filepath.Join(absOut, "core"), 0o755); err != nil {
		return res, fmt.Errorf("创建 core 目录失败: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(absOut, "addons"), 0o755); err != nil {
		return res, fmt.Errorf("创建 addons 目录失败: %w", err)
	}

	var coreImages []string
	if opts.CoreImages != nil {
		coreImages = opts.CoreImages
	} else {
		if opts.Verbose {
			fmt.Printf("  [images] 生成核心镜像清单（kubeadm config images list --image-repository %s）...\n", opts.ImageRepository)
		}
		coreImages, err = listCoreImages(ctx, opts, rtCfg)
		if err != nil {
			return res, fmt.Errorf("生成核心镜像清单失败: %w", err)
		}
		if len(opts.CoreFilter) > 0 {
			coreImages = filterCoreImages(coreImages, opts.CoreFilter)
		}
	}
	res.CoreImages = coreImages

	var jobs []saveJob
	for _, img := range coreImages {
		jobs = append(jobs, saveJob{Name: SafeTarName(img), Image: img, SubDir: "core"})
	}
	if opts.SkipAddons {
		res.SkipAddons = true
	} else {
		for _, a := range opts.Addons {
			for _, ea := range a.Expanded() {
				img := ea.Image + ":" + ea.Tag
				jobs = append(jobs, saveJob{Name: ea.Name, Image: img, SubDir: "addons"})
			}
		}
	}

	saved, err := pullAndSave(ctx, opts, rtCfg, jobs)
	if err != nil {
		return res, err
	}
	for i, j := range jobs {
		if i >= len(saved) {
			break
		}
		if j.SubDir == "addons" {
			res.Addons = append(res.Addons, saved[i])
		} else {
			res.Core = append(res.Core, saved[i])
		}
	}

	return res, nil
}

// listCoreImages 下载（或复用）官方 kubeadm 二进制，执行
// `kubeadm config images list` 获取核心镜像清单。
// Linux 且架构一致时在宿主机直跑；否则挂载进 BuildImage 容器执行。
func listCoreImages(ctx context.Context, opts Options, rtCfg rt.Config) ([]string, error) {
	if opts.K8sVersion == "" {
		return nil, fmt.Errorf("镜像清单生成依赖 K8sVersion")
	}
	if opts.ImageRepository == "" {
		opts.ImageRepository = "registry.k8s.io"
	}
	arch := normalizeArch(opts.Arch)

	kubeadmPath := opts.KubeadmBin
	var cleanup func()
	if kubeadmPath == "" {
		var err error
		kubeadmPath, cleanup, err = downloadKubeadm(ctx, opts.K8sVersion, arch, opts.KubeadmBaseURL, opts.Verbose, opts.KubeadmMode, opts.KubeadmRemoteHost, opts.KubeadmRemotePath)
		if err != nil {
			return nil, err
		}
		if cleanup != nil {
			defer cleanup()
		}
	}

	listArgs := []string{
		"config", "images", "list",
		"--kubernetes-version", opts.K8sVersion,
		"--image-repository", opts.ImageRepository,
	}

	var cmdDesc string
	var out []byte
	var err error
	if canRunKubeadmOnHost(arch) {
		cmd := exec.CommandContext(ctx, kubeadmPath, listArgs...)
		cmdDesc = kubeadmPath + " " + strings.Join(listArgs, " ")
		if opts.Verbose {
			fmt.Printf("  [images] 执行 kubeadm 生成核心镜像清单（%s）...\n", cmdDesc)
		}
		out, err = cmd.CombinedOutput()
	} else {
		if opts.BuildImage == "" {
			return nil, fmt.Errorf("当前平台无法直接执行 linux/%s kubeadm，且未提供 BuildImage", arch)
		}
		absKubeadm, absErr := filepath.Abs(kubeadmPath)
		if absErr != nil {
			return nil, absErr
		}
		runOpts := rt.RunOpts{
			Image:       opts.BuildImage,
			NamePrefix:  containerNameImagesList,
			Binds:       []string{absKubeadm + ":/kubeadm:ro"},
			NetworkHost: rtCfg.Runtime == rt.Containerd,
			Entrypoint:  "/kubeadm",
			Args:        listArgs,
		}
		cmdDesc, out, err = rt.RunCombined(ctx, rtCfg, runOpts)
		if opts.Verbose {
			fmt.Printf("  [images] 执行 kubeadm 生成核心镜像清单（%s）...\n", cmdDesc)
		}
	}
	if err != nil {
		return nil, fmt.Errorf("kubeadm 生成镜像清单失败: %v\n命令: %s\n输出: %s",
			err, cmdDesc, strings.TrimSpace(string(out)))
	}

	images := filterImageLines(string(out))
	if len(images) == 0 {
		return nil, fmt.Errorf("kubeadm 未返回任何镜像\n命令: %s\n输出: %s",
			cmdDesc, strings.TrimSpace(string(out)))
	}
	if opts.Verbose {
		fmt.Printf("  [images] 核心镜像清单生成完成：%d 个\n", len(images))
	}
	return images, nil
}

// canRunKubeadmOnHost 判断是否可在宿主机直接执行目标架构的 linux kubeadm。
func canRunKubeadmOnHost(arch string) bool {
	return runtime.GOOS == "linux" && normalizeArch(runtime.GOARCH) == normalizeArch(arch)
}

// normalizeArch 将 GOARCH / 用户输入规范为 kubeadm 发布所用 arch（amd64/arm64）。
func normalizeArch(arch string) string {
	switch arch {
	case "", "x86_64":
		return "amd64"
	case "aarch64":
		return "arm64"
	default:
		return arch
	}
}

// downloadKubeadm 获取 kubeadm 二进制。
// mode=local：本地直接下载（dl.k8s.io / CDN，可配 baseURL）；
// mode=remote：ssh 到免密服务器按 版本/架构 缓存目录下载/复用，scp 拷回本地。
func downloadKubeadm(ctx context.Context, k8sVersion, arch, baseURL string, verbose bool, mode, remoteHost, remotePath string) (path string, cleanup func(), err error) {
	if mode == "remote" {
		return downloadKubeadmRemote(ctx, k8sVersion, arch, baseURL, verbose, remoteHost, remotePath)
	}
	return downloadKubeadmLocal(ctx, k8sVersion, arch, baseURL, verbose)
}

// downloadKubeadmLocal 本地直接下载 kubeadm 二进制，依次尝试官方与 CDN 镜像。
func downloadKubeadmLocal(ctx context.Context, k8sVersion, arch, baseURL string, verbose bool) (path string, cleanup func(), err error) {
	bases := []string{baseURL}
	if baseURL == "" {
		bases = []string{
			"https://dl.k8s.io/release",
			"https://cdn.dl.k8s.io/release",
		}
	}

	var lastErr error
	for _, base := range bases {
		url := fmt.Sprintf("%s/%s/bin/linux/%s/kubeadm", strings.TrimSuffix(base, "/"), k8sVersion, arch)
		if verbose {
			fmt.Printf("  [images] 下载 kubeadm（%s）...\n", url)
		}
		path, cleanup, err = fetchKubeadmURL(ctx, url)
		if err == nil {
			return path, cleanup, nil
		}
		lastErr = err
	}
	return "", nil, lastErr
}

// downloadKubeadmRemote 通过 ssh 在远端服务器下载/复用 kubeadm 二进制并 scp 拷回。
// 远端缓存目录：{remotePath}/{version}/{arch}/kubeadm（remotePath 默认 ~/.builder-kubeadm）。
// 远端已有该文件时直接拷贝，否则在远端 curl/wget 下载。
func downloadKubeadmRemote(ctx context.Context, k8sVersion, arch, baseURL string, verbose bool, host, remotePath string) (path string, cleanup func(), err error) {
	if host == "" {
		return "", nil, fmt.Errorf("remote 模式需要配置 kubeadm_remote_host（user@host，免密登录）")
	}
	base := baseURL
	if base == "" {
		base = "https://dl.k8s.io/release"
	}
	url := fmt.Sprintf("%s/%s/bin/linux/%s/kubeadm", strings.TrimSuffix(base, "/"), k8sVersion, arch)
	if remotePath == "" {
		remotePath = "~/.builder-kubeadm"
	}
	remoteDir := fmt.Sprintf("%s/%s/%s", remotePath, k8sVersion, arch)
	remoteFile := remoteDir + "/kubeadm"

	exists, err := remoteFileExists(ctx, host, remoteFile)
	if err != nil {
		return "", nil, err
	}
	if exists {
		if verbose {
			fmt.Printf("  [images] 远端已存在 %s，直接拷贝\n", remoteFile)
		}
	} else {
		if verbose {
			fmt.Printf("  [images] 远端下载 kubeadm（%s）→ %s\n", url, remoteFile)
		}
		// 远端下载：curl 优先，wget 兜底。
		script := fmt.Sprintf("mkdir -p %s && (curl -fsSL -o %s '%s' || wget -q -O %s '%s')",
			remoteDir, remoteFile, url, remoteFile, url)
		cmd := exec.CommandContext(ctx, "ssh", host, script)
		if out, e := cmd.CombinedOutput(); e != nil {
			return "", nil, fmt.Errorf("远端下载 kubeadm 失败: %v\n输出: %s", e, strings.TrimSpace(string(out)))
		}
		if verbose {
			fmt.Printf("  [images] 远端下载完成：%s\n", remoteFile)
		}
	}

	// scp 拷回本地临时文件
	f, err := os.CreateTemp("", "builder-kubeadm-*")
	if err != nil {
		return "", nil, err
	}
	localPath := f.Name()
	f.Close()
	cleanup = func() { _ = os.Remove(localPath) }

	if verbose {
		fmt.Printf("  [images] 从远端拷贝 kubeadm 到本地 ...\n")
	}
	scp := exec.CommandContext(ctx, "scp", host+":"+remoteFile, localPath)
	if out, e := scp.CombinedOutput(); e != nil {
		cleanup()
		return "", nil, fmt.Errorf("scp 拷贝 kubeadm 失败: %v\n输出: %s", e, strings.TrimSpace(string(out)))
	}
	if verbose {
		fmt.Printf("  [images] kubeadm 已就绪（%s）\n", localPath)
	}
	if err := os.Chmod(localPath, 0o755); err != nil {
		cleanup()
		return "", nil, err
	}
	return localPath, cleanup, nil
}

// remoteFileExists 检查远端文件是否存在（ssh [ -f ... ]）。
func remoteFileExists(ctx context.Context, host, remoteFile string) (bool, error) {
	cmd := exec.CommandContext(ctx, "ssh", host, "[ -f "+remoteFile+" ] && echo yes || echo no")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return false, fmt.Errorf("ssh 检查远端文件失败: %v\n输出: %s", err, strings.TrimSpace(string(out)))
	}
	return strings.Contains(strings.TrimSpace(string(out)), "yes"), nil
}

func fetchKubeadmURL(ctx context.Context, url string) (path string, cleanup func(), err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", nil, fmt.Errorf("构造 kubeadm 下载请求失败: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", nil, fmt.Errorf("下载 kubeadm 失败（%s）: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", nil, fmt.Errorf("下载 kubeadm 失败（%s）: HTTP %d", url, resp.StatusCode)
	}

	f, err := os.CreateTemp("", "builder-kubeadm-*")
	if err != nil {
		return "", nil, fmt.Errorf("创建 kubeadm 临时文件失败: %w", err)
	}
	path = f.Name()
	cleanup = func() { _ = os.Remove(path) }

	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		cleanup()
		return "", nil, fmt.Errorf("写入 kubeadm 失败: %w", err)
	}
	if err := f.Close(); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("关闭 kubeadm 临时文件失败: %w", err)
	}
	if err := os.Chmod(path, 0o755); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("设置 kubeadm 可执行权限失败: %w", err)
	}
	return path, cleanup, nil
}

// filterImageLines 过滤容器内脚本输出中的非镜像行：
// 跳过空行、含空格/制表符的行，以及不以合法镜像名首字符（字母/数字）开头的行。
func filterImageLines(out string) []string {
	var images []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.ContainsAny(line, " \t") {
			continue
		}
		if !isValidImageStart(line) {
			continue
		}
		images = append(images, line)
	}
	return images
}

// filterCoreImages 按过滤项（短名或完整引用）筛选 kubeadm 生成的核心镜像清单。
// 保留与任一过滤项匹配的镜像：短名匹配镜像末段名（ShortName），完整引用要求完全一致。
// 过滤项为空时原样返回（拉取全部核心镜像）。
func filterCoreImages(images []string, filters []string) []string {
	if len(filters) == 0 {
		return images
	}
	var out []string
	for _, img := range images {
		for _, f := range filters {
			if img == f || ShortName(img) == f {
				out = append(out, img)
				break
			}
		}
	}
	return out
}

// isValidImageStart 判断镜像名首字符是否合法（字母/数字）。
func isValidImageStart(line string) bool {
	if line == "" {
		return false
	}
	r := rune(line[0])
	return unicode.IsLetter(r) || unicode.IsDigit(r)
}

// pullAndSave 按 runtime 拉取并保存为 docker-save tar。
func pullAndSave(ctx context.Context, opts Options, rtCfg rt.Config, jobs []saveJob) ([]SavedImage, error) {
	if len(jobs) == 0 {
		return nil, nil
	}
	if opts.DryRun {
		var out []SavedImage
		for _, j := range jobs {
			out = append(out, SavedImage{
				Name:        j.Name,
				SourceImage: j.Image,
				TarPath:     filepath.Join(opts.ImagesOutDir, j.SubDir, j.Name+".tar"),
			})
		}
		return out, nil
	}

	rtJobs := make([]rt.SaveJob, 0, len(jobs))
	for _, j := range jobs {
		rtJobs = append(rtJobs, rt.SaveJob{
			Name:    j.Name,
			Image:   j.Image,
			SubDir:  j.SubDir,
			OutRoot: opts.ImagesOutDir,
			Arch:    opts.Arch,
		})
	}
	if err := rt.PullAndSave(ctx, rtCfg, rtJobs, opts.Verbose); err != nil {
		return nil, err
	}

	var saved []SavedImage
	for _, j := range jobs {
		tarPath := filepath.Join(opts.ImagesOutDir, j.SubDir, j.Name+".tar")
		st, err := os.Stat(tarPath)
		if err != nil {
			return nil, fmt.Errorf("save 后读取 %s 失败: %w", tarPath, err)
		}
		sum, err := fileSHA256(tarPath)
		if err != nil {
			return nil, fmt.Errorf("计算镜像 tar sha256 失败: %w", err)
		}
		saved = append(saved, SavedImage{
			Name:        j.Name,
			SourceImage: j.Image,
			TarPath:     tarPath,
			Size:        st.Size(),
			SHA256:      sum,
		})
	}
	return saved, nil
}

// buildPullSaveScript 构造 docker 模式下容器内批量 pull + save 脚本（单测与文档对照）。
func buildPullSaveScript(jobs []saveJob) string {
	var b strings.Builder
	b.WriteString("set -e\n")
	b.WriteString("mkdir -p /out/core /out/addons\n")
	total := len(jobs)
	for i, j := range jobs {
		img := "'" + strings.ReplaceAll(j.Image, "'", `'\''`) + "'"
		tar := "'" + strings.ReplaceAll("/out/"+j.SubDir+"/"+j.Name+".tar", "'", `'\''`) + "'"
		b.WriteString(fmt.Sprintf("echo \"[images] %d/%d pull %s\"\n", i+1, total, j.Image))
		b.WriteString("docker pull " + img + "\n")
		b.WriteString(fmt.Sprintf("echo \"[images] %d/%d save %s\"\n", i+1, total, j.Image))
		b.WriteString("docker save -o " + tar + " " + img + "\n")
	}
	return b.String()
}

// shellSingleQuote 用单引号包裹 shell 参数，并转义内部单引号。
func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// ShortName 提取镜像 short name（不含 registry 路径与 tag）。
func ShortName(image string) string {
	s := image
	if idx := strings.LastIndex(s, "/"); idx >= 0 {
		s = s[idx+1:]
	}
	if idx := strings.Index(s, "@"); idx >= 0 {
		s = s[:idx]
	}
	if idx := strings.Index(s, ":"); idx >= 0 {
		s = s[:idx]
	}
	return s
}

// SafeTarName 将镜像名转换为安全的 tar 文件名（字母数字 + . _ - 保留，其余替换为 -）。
func SafeTarName(image string) string {
	var b strings.Builder
	for _, r := range ShortName(image) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '.' || r == '_' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteRune('-')
		}
	}
	return b.String()
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
