// Package images 生成核心镜像清单（kubeadm config images list），
// 并在容器内 docker pull + save 核心镜像与附加组件镜像为 tar 文件。
// 核心镜像清单通过官方 kubeadm 静态二进制生成（宿主机直跑或挂进构建容器）。
// 镜像打包在 PackImage 容器内执行（挂载 docker.sock + 输出目录），与软件包阶段一致。
// docker 不可用时步骤标记为 skipped。
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
)

// 默认镜像打包容器（仅含 docker CLI，通过挂载的 sock 操作宿主机 daemon）。
const defaultPackImage = "docker:24-cli"

// 容器名前缀：docker run --name，便于 docker ps 区分阶段。
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
	// DockerBin docker 命令路径，默认 "docker"。
	DockerBin string
	// DockerSock 宿主机 docker socket，默认 /var/run/docker.sock；挂入打包容器。
	DockerSock string
	// BuildImage 构建容器镜像（如 ubuntu:22.04），仅在宿主机无法直接执行 kubeadm
	// （非 Linux 或架构不一致）时，用于挂载二进制跑 images list。
	BuildImage string
	// PackImage 镜像打包容器（含 docker CLI），默认 docker:24-cli。
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
	// CoreImages 外部传入的最终核心镜像完整引用清单（已解析）。
	//   nil    → 未外部指定：内部走 kubeadm 生成默认核心清单（再用 CoreFilter 过滤）
	//   非 nil → 直接使用该清单（可为空 slice，表示不拉取任何核心镜像），不再走 kubeadm
	CoreImages []string
	// CoreFilter 外部传入的核心镜像过滤项（短名或完整引用）。
	// 仅当 CoreImages 为 nil（走 kubeadm 生成）时生效：对生成结果按过滤项匹配。
	// 为空表示拉取全部核心镜像。
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
func DockerAvailable(bin string) (bool, string) {
	if bin == "" {
		bin = "docker"
	}
	cmd := exec.Command(bin, "info")
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return false, fmt.Sprintf("docker 不可用: %s（镜像阶段将被跳过）", msg)
	}
	return true, ""
}

// Fetch 执行核心镜像清单生成 + 容器内拉取 + save。
func Fetch(ctx context.Context, opts Options) (*Result, error) {
	if opts.DockerBin == "" {
		opts.DockerBin = "docker"
	}
	if opts.ImageRepository == "" {
		opts.ImageRepository = "registry.k8s.io"
	}
	if opts.PackImage == "" {
		opts.PackImage = defaultPackImage
	}
	if opts.DockerSock == "" {
		opts.DockerSock = "/var/run/docker.sock"
	}

	res := &Result{HostArch: runtime.GOARCH, ArchMismatch: opts.Arch != "" && opts.Arch != runtime.GOARCH}
	if res.ArchMismatch {
		res.Arch = opts.Arch
	}

	// DryRun：仅构造目录，不执行任何 docker/kubeadm 命令
	if opts.DryRun {
		os.MkdirAll(filepath.Join(opts.ImagesOutDir, "core"), 0o755)
		os.MkdirAll(filepath.Join(opts.ImagesOutDir, "addons"), 0o755)
		return res, nil
	}

	// 检查 docker 可用性
	if ok, reason := DockerAvailable(opts.DockerBin); !ok {
		res.Skipped = true
		res.SkipReason = reason
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

	// 步骤 1：核心镜像清单。
	// 外部传入 CoreImages（非 nil）时直接使用（可能为空 = 不拉核心镜像），不再走 kubeadm；
	// 否则用官方 kubeadm 二进制生成（Linux 直跑或挂载进构建容器），并按 CoreFilter 过滤。
	var coreImages []string
	if opts.CoreImages != nil {
		coreImages = opts.CoreImages
	} else {
		coreImages, err = listCoreImages(ctx, opts)
		if err != nil {
			return res, fmt.Errorf("生成核心镜像清单失败: %w", err)
		}
		if len(opts.CoreFilter) > 0 {
			coreImages = filterCoreImages(coreImages, opts.CoreFilter)
		}
	}
	res.CoreImages = coreImages

	// 步骤 2/3：在打包容器内 pull + save（核心 + 可选 addons）
	var jobs []saveJob
	for _, img := range coreImages {
		jobs = append(jobs, saveJob{Name: SafeTarName(img), Image: img, SubDir: "core"})
	}
	if opts.SkipAddons {
		res.SkipAddons = true
	} else {
		for _, a := range opts.Addons {
			img := a.Image + ":" + a.Tag
			jobs = append(jobs, saveJob{Name: a.Name, Image: img, SubDir: "addons"})
		}
	}

	saved, err := pullAndSaveInContainer(ctx, opts, jobs)
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
func listCoreImages(ctx context.Context, opts Options) ([]string, error) {
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
		kubeadmPath, cleanup, err = downloadKubeadm(ctx, opts.K8sVersion, arch, opts.KubeadmBaseURL)
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

	var cmd *exec.Cmd
	var cmdDesc string
	if canRunKubeadmOnHost(arch) {
		cmd = exec.CommandContext(ctx, kubeadmPath, listArgs...)
		cmdDesc = kubeadmPath + " " + strings.Join(listArgs, " ")
	} else {
		if opts.BuildImage == "" {
			return nil, fmt.Errorf("当前平台无法直接执行 linux/%s kubeadm，且未提供 BuildImage", arch)
		}
		// 挂载二进制，entrypoint 直接跑 kubeadm，无需容器内 apt/dnf
		cmdArgs := []string{
			"run", "--rm",
			"--name", uniqueContainerName(containerNameImagesList),
			"-v", kubeadmPath + ":/kubeadm:ro",
			"--entrypoint", "/kubeadm",
			opts.BuildImage,
		}
		cmdArgs = append(cmdArgs, listArgs...)
		cmd = exec.CommandContext(ctx, opts.DockerBin, cmdArgs...)
		cmdDesc = "docker " + strings.Join(cmdArgs, " ")
	}

	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("kubeadm 生成镜像清单失败: %v\n命令: %s\n输出: %s",
			err, cmdDesc, strings.TrimSpace(string(out)))
	}

	images := filterImageLines(string(out))
	if len(images) == 0 {
		return nil, fmt.Errorf("kubeadm 未返回任何镜像\n命令: %s\n输出: %s",
			cmdDesc, strings.TrimSpace(string(out)))
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

// downloadKubeadm 从 dl.k8s.io 下载指定版本/架构的 kubeadm 二进制到临时文件。
// 依次尝试官方与 CDN 镜像，兼容部分网络环境。
func downloadKubeadm(ctx context.Context, k8sVersion, arch, baseURL string) (path string, cleanup func(), err error) {
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
		path, cleanup, err = fetchKubeadmURL(ctx, url)
		if err == nil {
			return path, cleanup, nil
		}
		lastErr = err
	}
	return "", nil, lastErr
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

// pullAndSaveInContainer 在 PackImage 容器内批量 docker pull + save。
// 挂载 docker.sock（操作宿主机 daemon）与 ImagesOutDir→/out。
func pullAndSaveInContainer(ctx context.Context, opts Options, jobs []saveJob) ([]SavedImage, error) {
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

	script := buildPullSaveScript(jobs)
	cmdArgs := []string{
		"run", "--rm",
		"--name", uniqueContainerName(containerNameImagesPack),
		"-v", opts.DockerSock + ":/var/run/docker.sock",
		"-v", opts.ImagesOutDir + ":/out",
		opts.PackImage,
		"sh", "-c", script,
	}
	cmd := exec.CommandContext(ctx, opts.DockerBin, cmdArgs...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("容器内镜像 pull/save 失败: %v\n命令: docker %s\n输出: %s",
			err, strings.Join(cmdArgs, " "), strings.TrimSpace(string(out)))
	}

	var saved []SavedImage
	for _, j := range jobs {
		tarPath := filepath.Join(opts.ImagesOutDir, j.SubDir, j.Name+".tar")
		st, err := os.Stat(tarPath)
		if err != nil {
			return nil, fmt.Errorf("容器内 save 后读取 %s 失败: %w", tarPath, err)
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

// buildPullSaveScript 构造容器内批量 pull + save 脚本。
// 镜像 tar 写到 /out/{core|addons}/{name}.tar。
func buildPullSaveScript(jobs []saveJob) string {
	var b strings.Builder
	b.WriteString("set -e\n")
	b.WriteString("mkdir -p /out/core /out/addons\n")
	for _, j := range jobs {
		img := shellSingleQuote(j.Image)
		tar := shellSingleQuote("/out/" + j.SubDir + "/" + j.Name + ".tar")
		b.WriteString("docker pull " + img + "\n")
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
