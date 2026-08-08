// Package config 负责加载 builder 的单份清单配置文件 builder.yaml，
// 并提供查找与校验方法。
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config 聚合清单配置（oses / versions / addon_images / addon_packages）、
// 可选 GitHub Release 上传配置与 build 默认参数。
// OSRegistry / K8sVersions / AddonImages 以 inline 展开，使单文件顶层键直接映射。
type Config struct {
	OSRegistry  OSRegistry  `yaml:",inline"`
	K8sVersions K8sVersions `yaml:",inline"`
	AddonImages AddonImages `yaml:",inline"`
	// AddonPackages 附加安装包列表（与 addon_images 平级的顶层节）。
	// mode ∈ {packages, all} 且未 --skip-addons 时并入软件包下载清单；--only-addons 时为其软件包主体。
	// 每项为 {name, version} 对象；version 为空 = 不锁版本（透传纯包名），
	// version 非空按目标包管理器语法转译（apt: name=version；dnf: name-version）。
	AddonPackages []AddonPackage `yaml:"addon_packages"`
	GitHub        GitHubConfig   `yaml:"github"`
	// Build build 子命令默认参数（优先级：命令行 > 配置 > 内置默认值）。
	Build BuildOptions `yaml:"build"`

	// 配置加载来源路径（仅用于日志/调试）
	SourceFile string `yaml:"-"`
}

// BuildOptions build 子命令可配置的默认参数。
// 命令行显式传参优先；未传时回落到本配置节；配置值为空时再回落到 flag 内置默认值。
// 核心软件包/镜像始终使用默认清单（BuildPackageList / kubeadm 生成）；
// 自定义能力由顶层 addon_packages / addon_images（与 --mode / --only-addons / --skip-addons 联动）提供。
type BuildOptions struct {
	OS                string `yaml:"os"`
	OSVersion         string `yaml:"os_version"`
	KubernetesVersion string `yaml:"kubernetes_version"`
	Arch              string `yaml:"arch"`
	Mirror            string `yaml:"mirror"`
	WorkDir           string `yaml:"workdir"`
	OutDir            string `yaml:"out"`
	Mode              string `yaml:"mode"`
	SkipAddons        bool   `yaml:"skip_addons"`
	OnlyAddons        bool   `yaml:"only_addons"`
	DryRun            bool   `yaml:"dry_run"`
	// KeepFiles 构建完成后是否保留中间文件（packages/images/bundle 目录）；默认 false=清理。
	KeepFiles bool `yaml:"keep_files"`
	// Verbose 打印详细过程日志（镜像下载/pull 进度等）；默认 false=精简输出。
	Verbose bool `yaml:"verbose"`
	// KubeadmDir kubeadm 二进制缓存目录，默认 ./kube。
	KubeadmDir string `yaml:"kubeadm_dir"`
}

// GitHubConfig 产物上传到 GitHub Release 的参数。
// token 建议用环境变量 GITHUB_TOKEN / GH_TOKEN，避免明文写入配置文件。
type GitHubConfig struct {
	Owner string `yaml:"owner"`
	Repo  string `yaml:"repo"`
	Tag   string `yaml:"tag"`
	Token string `yaml:"token"`
}

// OSRegistry 操作系统注册表。
type OSRegistry struct {
	OSes []OS `yaml:"oses"`
}

// OS 单个操作系统定义。
type OS struct {
	Name        string            `yaml:"name"`
	Versions    []string          `yaml:"versions"`
	PkgManager  string            `yaml:"pkg_manager"`
	BuildImages map[string]string `yaml:"build_images"`
	Archs       []string          `yaml:"archs"`

	// Codename apt 系默认版本代号（如 ubuntu 22.04 → jammy），
	// 单版本 OS 可直接用该字段；多版本 OS 用 Codenames 按版本映射。
	Codename string `yaml:"codename"`
	// Codenames apt 系版本 → 版本代号映射（ubuntu 20.04→focal、22.04→jammy…）。
	Codenames map[string]string `yaml:"codenames"`
	// RPMDistro dnf/yum 系发行版标识，用于 containerd dnf 源（rocky→rhel9、openEuler→rhel7）。
	RPMDistro string `yaml:"rpm_distro"`
	// ContainerdPkg containerd 软件包包名；留空时按发行版推断（ResolveOS）：
	// openEuler 等从系统源安装的发行版推断为 "containerd"，其余默认 "containerd.io"（docker-ce 源包名）。
	// 显式配置（如 openEuler 系统源包名 "containerd"）优先于推断。
	ContainerdPkg string `yaml:"containerd_pkg"`
	// ContainerdRepo containerd 源类型：aliyun=阿里云 mirrors.aliyun.com/docker-ce（默认）；
	// ustc=中科大 mirrors.ustc.edu.cn/docker-ce；docker=官方 download.docker.com（可选）；
	// none=不配置 docker-ce 源，containerd 由系统源（everything 等）提供。
	// 留空时按发行版推断（ResolveOS）：openEuler 推断为 none，其余默认 aliyun。
	ContainerdRepo string `yaml:"containerd_repo"`
}

// ImageFor 返回指定 OS 版本的构建容器镜像（build_images 映射，用于容器内下载软件包）。
// 未在映射中配置时按约定回退为 {name}:{version}（openEuler 等特殊发行版见 DefaultBuildImage）。
func (o *OS) ImageFor(version string) (string, error) {
	if version == "" {
		return "", fmt.Errorf("OS 版本不能为空")
	}
	if img, ok := o.BuildImages[version]; ok && img != "" {
		return img, nil
	}
	return DefaultBuildImage(o.Name, version), nil
}

// CodenameFor 返回指定 OS 版本的 apt 版本代号。
// 优先取 Codenames[version]，未命中时回退到 Codename，再回退到常见版本启发式。
func (o *OS) CodenameFor(version string) string {
	if c, ok := o.Codenames[version]; ok && c != "" {
		return c
	}
	if o.Codename != "" {
		return o.Codename
	}
	return InferCodename(o.Name, version)
}

// K8sVersions k8s 版本 → 运行时版本映射。
type K8sVersions struct {
	Versions []K8sVersion `yaml:"versions"`
}

// K8sVersion 单个 k8s 版本及配套运行时版本。
type K8sVersion struct {
	Version    string `yaml:"version"`
	Containerd string `yaml:"containerd"`
	Crictl     string `yaml:"crictl"`
	Runc       string `yaml:"runc"`
}

// AddonImages 附加组件镜像清单（顶层 addon_images 节）。
type AddonImages struct {
	Addons []Addon `yaml:"addon_images"`
}

// Addon 单个附加组件镜像。
type Addon struct {
	Name  string `yaml:"name"`
	Image string `yaml:"image"`
	Tag   string `yaml:"tag"`
}

// AddonPackage 单个附加安装包（name + 可选 version）。
// Version 为空（或省略）表示不锁版本，透传纯包名给容器内包管理器；
// Version 非空按目标包管理器语法转译：apt 系 name=version；dnf/yum 系 name-version。
type AddonPackage struct {
	Name    string `yaml:"name"`
	Version string `yaml:"version"`
}

// FindAddon 按 name 查找 addon，返回引用（nil, false 表示不存在）。
func (c *Config) FindAddon(name string) (*Addon, bool) {
	for i := range c.AddonImages.Addons {
		if c.AddonImages.Addons[i].Name == name {
			return &c.AddonImages.Addons[i], true
		}
	}
	return nil, false
}

// 配置文件名常量。
const FileName = "builder.yaml"

// DefaultConfigFile 返回默认配置文件路径。
// 优先使用环境变量 BUILDER_CONFIG_FILE；否则返回生产默认路径 /etc/pixiu/builder.yaml
// （与 pixiu 生产配置惯例一致）。
// 本地开发请通过 --configFile ./builder.yaml 或 export BUILDER_CONFIG_FILE=./builder.yaml 指定。
func DefaultConfigFile() string {
	if f := os.Getenv("BUILDER_CONFIG_FILE"); f != "" {
		return f
	}
	return filepath.Join("/etc/pixiu", FileName)
}

// Load 从指定配置文件路径加载 builder.yaml。path 为空时使用 DefaultConfigFile()。
func Load(path string) (*Config, error) {
	if path == "" {
		path = DefaultConfigFile()
	}

	cfg := &Config{SourceFile: path}

	if err := loadYAML(path, cfg); err != nil {
		return nil, err
	}

	// 空清单校验：OS 注册表与 k8s 版本清单为空视为配置错误，addon_images 允许为空。
	if len(cfg.OSRegistry.OSes) == 0 {
		return nil, fmt.Errorf("OS 注册表（oses）配置为空: %s", path)
	}
	if len(cfg.K8sVersions.Versions) == 0 {
		return nil, fmt.Errorf("k8s 版本（versions）配置为空: %s", path)
	}

	return cfg, nil
}

func loadYAML(path string, out any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("读取配置文件失败 %s: %w", path, err)
	}
	if err := yaml.Unmarshal(data, out); err != nil {
		return fmt.Errorf("解析配置文件失败 %s: %w", path, err)
	}
	return nil
}

// FindOS 按名称查找操作系统，返回 nil, false 表示不存在。
func (c *Config) FindOS(name string) (*OS, bool) {
	for i := range c.OSRegistry.OSes {
		if c.OSRegistry.OSes[i].Name == name {
			return &c.OSRegistry.OSes[i], true
		}
	}
	return nil, false
}

// FindK8s 按版本查找 k8s 版本定义（可选参考配置）。
// 版本注册在 builder.yaml versions 节时返回对应记录；否则返回 nil, false。
// 注意：未注册版本也能 build，此方法仅用于获取已知版本的参考字段（如 crictl 覆盖）。
func (c *Config) FindK8s(version string) (*K8sVersion, bool) {
	for i := range c.K8sVersions.Versions {
		if c.K8sVersions.Versions[i].Version == version {
			return &c.K8sVersions.Versions[i], true
		}
	}
	return nil, false
}

// ValidOS 校验 OS 名称与版本组合是否在注册表中。
// 注意：build 已支持任意 OS/版本（与 ValidK8s 类似），此方法仅供 list-os 等参考展示；
// 构建管线请使用 ResolveOS，勿再以 ValidOS 拦截。
func (c *Config) ValidOS(name, version string) bool {
	os, ok := c.FindOS(name)
	if !ok {
		return false
	}
	for _, v := range os.Versions {
		if v == version {
			return true
		}
	}
	return false
}

// ValidOSArch 校验架构：任意 OS 均允许 amd64/arm64（注册表 archs 仅作参考，不再拦截）。
func (c *Config) ValidOSArch(name, arch string) bool {
	return ValidArch(arch)
}

// ValidArch 校验目标架构是否为 builder 支持的值。
func ValidArch(arch string) bool {
	switch arch {
	case "amd64", "arm64":
		return true
	default:
		return false
	}
}

// ResolvedOS 解析后的目标 OS 构建参数（注册表命中或按约定合成）。
type ResolvedOS struct {
	Name       string
	Version    string
	PkgManager string
	BuildImage string
	Codename   string
	RPMDistro  string
	// ContainerdPkg containerd 软件包包名（未配置时按发行版推断：openEuler→containerd；其他→containerd.io）。
	ContainerdPkg string
	// ContainerdRepo containerd 源（未配置时按发行版推断：openEuler→none；其他→aliyun）。
	ContainerdRepo string
	// FromRegistry 是否命中 builder.yaml oses 节（仅版本未登记时仍可能为 true）。
	FromRegistry bool
}

// ResolveOS 解析任意 OS/版本：优先使用注册表中的 pkg_manager / build_images / codename；
// 未登记的 OS 或版本按约定推导（构建镜像默认为 {os}:{version}），与任意 k8s 版本策略一致。
func (c *Config) ResolveOS(name, version string) (*ResolvedOS, error) {
	name = strings.TrimSpace(name)
	version = strings.TrimSpace(version)
	if name == "" || version == "" {
		return nil, fmt.Errorf("OS/版本不能为空")
	}

	res := &ResolvedOS{
		Name:           name,
		Version:        version,
		ContainerdPkg:  "containerd.io",
		ContainerdRepo: "aliyun",
	}
	if osDef, ok := c.FindOS(name); ok {
		res.FromRegistry = true
		res.PkgManager = osDef.PkgManager
		if res.PkgManager == "" {
			res.PkgManager = InferPkgManager(name, version)
		}
		img, err := osDef.ImageFor(version)
		if err != nil {
			return nil, err
		}
		res.BuildImage = img
		res.Codename = osDef.CodenameFor(version)
		res.RPMDistro = osDef.RPMDistro
		if res.RPMDistro == "" {
			res.RPMDistro = InferRPMDistro(name, version)
		}
		if osDef.ContainerdPkg != "" {
			res.ContainerdPkg = osDef.ContainerdPkg
		} else {
			res.ContainerdPkg = InferContainerdPkg(name)
		}
		if osDef.ContainerdRepo != "" {
			res.ContainerdRepo = osDef.ContainerdRepo
		} else {
			res.ContainerdRepo = InferContainerdRepo(name)
		}
		return res, nil
	}

	res.PkgManager = InferPkgManager(name, version)
	res.BuildImage = DefaultBuildImage(name, version)
	res.Codename = InferCodename(name, version)
	res.RPMDistro = InferRPMDistro(name, version)
	res.ContainerdPkg = InferContainerdPkg(name)
	res.ContainerdRepo = InferContainerdRepo(name)
	return res, nil
}

const defaultBuildImageRegistry = "swr.cn-north-4.myhuaweicloud.com/pixiu-public"

// DefaultBuildImage 按发行版约定生成构建容器镜像引用。
func DefaultBuildImage(osName, version string) string {
	switch strings.ToLower(osName) {
	case "openeuler":
		return defaultBuildImageRegistry + "/openeuler/openeuler:" + version
	default:
		return defaultBuildImageRegistry + "/" + osName + ":" + version
	}
}

// InferPkgManager 按发行版名称与版本推断包管理器。
// centos/rhel/almalinux 主版本为 7 时使用 yum（CentOS 7 无 dnf），
// 8/9+ 使用 dnf；rocky/fedora/openeuler/amazonlinux 一律 dnf；其余（ubuntu/debian/未知）apt。
func InferPkgManager(osName, version string) string {
	switch strings.ToLower(osName) {
	case "centos", "rhel", "almalinux":
		// 取主版本号：7 / 7.9 → 7 → yum；8 / 9 → dnf
		major := strings.Split(version, ".")[0]
		if major == "7" {
			return "yum"
		}
		return "dnf"
	case "rocky", "fedora", "openeuler", "amazonlinux":
		return "dnf"
	default:
		// ubuntu / debian / 未知发行版默认 apt
		return "apt"
	}
}

// InferCodename 常见 ubuntu/debian 版本 → apt 代号；未知则空（调用方可跳过依赖代号的源）。
func InferCodename(osName, version string) string {
	switch strings.ToLower(osName) {
	case "ubuntu":
		switch version {
		case "18.04":
			return "bionic"
		case "20.04":
			return "focal"
		case "22.04":
			return "jammy"
		case "24.04":
			return "noble"
		}
	case "debian":
		switch version {
		case "10":
			return "buster"
		case "11":
			return "bullseye"
		case "12":
			return "bookworm"
		case "13":
			return "trixie"
		}
	}
	return ""
}

// InferRPMDistro 按发行版推断 containerd dnf 源所用 rhel 标识。
func InferRPMDistro(osName, version string) string {
	switch strings.ToLower(osName) {
	case "rocky", "almalinux", "rhel", "centos":
		// 取主版本号：9 / 9.3 → rhel9
		major := strings.Split(version, ".")[0]
		if major != "" {
			return "rhel" + major
		}
		return "rhel9"
	case "openeuler":
		return "rhel7"
	case "fedora":
		return "fedora"
	default:
		return ""
	}
}

// InferContainerdPkg 按发行版推断 containerd 软件包包名：
// openEuler 从系统源（everything 仓库）安装，包名为 "containerd"；
// 其余发行版从 docker-ce 源安装，包名为 "containerd.io"。
// 仅当 builder.yaml 未显式配置 containerd_pkg 时生效（显式配置优先）。
func InferContainerdPkg(osName string) string {
	if strings.ToLower(osName) == "openeuler" {
		return "containerd"
	}
	return "containerd.io"
}

// InferContainerdRepo 按发行版推断 containerd 源类型：
// openEuler 对应 download.docker.com/linux/rhel/7/ 实测 404（docker 官方已停止发布 RHEL7 仓库），
// 返回 "none"（不配置 docker-ce 源，由系统源提供）；其余发行版返回 "aliyun"（国内镜像默认）。
// 仅当 builder.yaml 未显式配置 containerd_repo 时生效（显式配置优先）。
func InferContainerdRepo(osName string) string {
	if strings.ToLower(osName) == "openeuler" {
		return "none"
	}
	return "aliyun"
}

// k8sVersionRe 合法 k8s 版本格式：vX.Y.Z（如 v1.31.0、v1.29.5）。
var k8sVersionRe = regexp.MustCompile(`^v\d+\.\d+\.\d+$`)

// ValidK8s 校验 k8s 版本格式是否合法。
// 不再要求版本注册在 builder.yaml versions 节中，任意合法 vX.Y.Z 均可 build。
func (c *Config) ValidK8s(version string) bool {
	return k8sVersionRe.MatchString(version)
}

// CrictlVersionFor 返回 k8s 版本对应的 crictl 回退版本（cri-tools 包缺失时使用）。
// 版本注册在 builder.yaml versions 节且 crictl 字段非空时返回清单值（可覆盖默认推导）；
// 否则推导为 strings.TrimPrefix(k8sVersion, "v")（cri-tools 版本与 k8s 版本对齐，如 v1.29.5→1.29.5）。
func (c *Config) CrictlVersionFor(k8sVersion string) string {
	if ks, ok := c.FindK8s(k8sVersion); ok && ks.Crictl != "" {
		return ks.Crictl
	}
	return strings.TrimPrefix(k8sVersion, "v")
}

// OSNames 返回全部 OS 名称（有序）。
func (c *Config) OSNames() []string {
	out := make([]string, 0, len(c.OSRegistry.OSes))
	for _, os := range c.OSRegistry.OSes {
		out = append(out, os.Name)
	}
	return out
}

// K8sVersionsList 返回全部 k8s 版本（有序）。
func (c *Config) K8sVersionsList() []string {
	out := make([]string, 0, len(c.K8sVersions.Versions))
	for _, v := range c.K8sVersions.Versions {
		out = append(out, v.Version)
	}
	return out
}

// SystemDepsForOS 返回指定 OS 的容器内依赖包清单。
// osVersion 用于区分同族发行版的包管理器（如 centos 7 → yum、centos 8+ → dnf）。
func (c *Config) SystemDepsForOS(osName, osVersion string) []string {
	// 通用依赖：conntrack ipvsadm socat ebtables chrony
	deps := []string{"conntrack", "ipvsadm", "socat", "ebtables", "chrony"}
	pkgManager := InferPkgManager(osName, osVersion)
	if osDef, ok := c.FindOS(osName); ok && osDef.PkgManager != "" {
		pkgManager = osDef.PkgManager
	}
	switch pkgManager {
	case "apt":
		deps = append(deps, "nfs-common")
	case "dnf", "yum":
		deps = append(deps, "nfs-utils")
	}
	return deps
}

// CodenameFor 返回指定 OS 版本的 apt 版本代号（用于 containerd 源）。
func (c *Config) CodenameFor(osName, osVersion string) string {
	if osDef, ok := c.FindOS(osName); ok {
		if c := osDef.CodenameFor(osVersion); c != "" {
			return c
		}
	}
	return InferCodename(osName, osVersion)
}

// RPMDistroFor 返回指定 OS 的 dnf 发行版标识（用于 containerd 源）。
func (c *Config) RPMDistroFor(osName string) string {
	if osDef, ok := c.FindOS(osName); ok && osDef.RPMDistro != "" {
		return osDef.RPMDistro
	}
	return InferRPMDistro(osName, "")
}

// K8sMinor 从 k8s 版本（如 v1.27.3）推导 Kubernetes 包源大版本 repo（如 v1.27）。
func K8sMinor(version string) (string, error) {
	trimmed := strings.TrimPrefix(version, "v")
	parts := strings.Split(trimmed, ".")
	if len(parts) < 3 || parts[0] == "" || parts[1] == "" {
		return "", fmt.Errorf("非法 k8s 版本 %q：期望形如 v1.27.3", version)
	}
	return "v" + parts[0] + "." + parts[1], nil
}

// ParseK8sVersion 解析正式版 vX.Y.Z，返回 major/minor/patch。
func ParseK8sVersion(version string) (major, minor, patch int, err error) {
	if !k8sVersionRe.MatchString(version) {
		return 0, 0, 0, fmt.Errorf("非法或不正式的 k8s 版本 %q：期望形如 v1.31.0（不含 -rc/-alpha/-beta）", version)
	}
	parts := strings.Split(strings.TrimPrefix(version, "v"), ".")
	if _, err = fmt.Sscanf(parts[0], "%d", &major); err != nil {
		return 0, 0, 0, err
	}
	if _, err = fmt.Sscanf(parts[1], "%d", &minor); err != nil {
		return 0, 0, 0, err
	}
	if _, err = fmt.Sscanf(parts[2], "%d", &patch); err != nil {
		return 0, 0, 0, err
	}
	return major, minor, patch, nil
}

// CompareK8sVersion 比较两个正式版：-1(a<b) / 0 / 1(a>b)。
func CompareK8sVersion(a, b string) (int, error) {
	am, an, ap, err := ParseK8sVersion(a)
	if err != nil {
		return 0, err
	}
	bm, bn, bp, err := ParseK8sVersion(b)
	if err != nil {
		return 0, err
	}
	if am != bm {
		if am < bm {
			return -1, nil
		}
		return 1, nil
	}
	if an != bn {
		if an < bn {
			return -1, nil
		}
		return 1, nil
	}
	if ap < bp {
		return -1, nil
	}
	if ap > bp {
		return 1, nil
	}
	return 0, nil
}

// FilterStableK8sTags 从 tag 列表中筛出正式版（vX.Y.Z），且版本 >= minVersion（如 v1.31.0），并按版本升序去重。
func FilterStableK8sTags(tags []string, minVersion string) ([]string, error) {
	if _, _, _, err := ParseK8sVersion(minVersion); err != nil {
		return nil, fmt.Errorf("非法最小版本 %s: %w", minVersion, err)
	}
	seen := make(map[string]bool)
	var out []string
	for _, tag := range tags {
		tag = strings.TrimSpace(tag)
		if !k8sVersionRe.MatchString(tag) {
			continue
		}
		cmp, err := CompareK8sVersion(tag, minVersion)
		if err != nil || cmp < 0 {
			continue
		}
		if seen[tag] {
			continue
		}
		seen[tag] = true
		out = append(out, tag)
	}
	sort.Slice(out, func(i, j int) bool {
		cmp, _ := CompareK8sVersion(out[i], out[j])
		return cmp < 0
	})
	return out, nil
}
