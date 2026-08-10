// Package packages 在目标系统容器内通过包管理器（apt/dnf）递归下载
// k8s 组件、容器运行时与系统依赖包，并生成对应的软件源（apt 系 k8s 源使用阿里云 kubernetes-new 镜像；
// dnf/yum 与 containerd 源默认使用国内镜像，containerd 默认阿里云）。
// 依赖闭包交由包管理器处理；docker 不可用时该步骤标记为 skipped。
package packages

import (
	"fmt"
	"strings"
)

// Repo 描述一个需要配置的软件源。apt 与 dnf 二选一（按目标 OS 包管理器使用）。
// 注意：apt 系 k8s 源使用阿里云 kubernetes-new；containerd 源按配置的国内镜像（默认阿里云）生成。
type Repo struct {
	// Name 源标识，用于 apt sources.list.d 文件名与 dnf repo 文件名。
	Name string
	// AptLine apt 源 deb 行。
	AptLine string
	// AptKeyURL apt 源 GPG key 下载 URL（容器内 curl 下载后 dearmor）。
	AptKeyURL string
	// AptKeyDest 容器内 dearmor 后 key 的落盘路径。
	AptKeyDest string
	// DnfRepoBlock dnf repo 文件完整内容（[repo-id] 块）。
	DnfRepoBlock string
	// DnfKeyURL dnf 源 GPG key URL（容器内 rpm --import）。
	DnfKeyURL string
}

// k8sSigningKeyMinor 拉取已续期的 OBS 仓库签名密钥所用的仓库版本。
// 旧仓库路径（如 v1.27）下的 Release.key 仍可能是已过期密钥，会导致：
//
//	EXPKEYSIG 234654DA9A296436 isv:kubernetes OBS Project
//
// 官方建议改用较新仓库（≥v1.31）的密钥校验所有 k8s 软件仓库。
// 参见 https://github.com/kubernetes/release/issues/3818
const k8sSigningKeyMinor = "v1.31"

// K8sRepos 返回 k8s 组件源。
// k8sMinor 形如 v1.27，由 k8s 版本前两段推导（决定软件包仓库路径）。
// apt 系与 dnf/yum 系均使用阿里云 kubernetes-new 镜像，避免构建环境访问 pkgs.k8s.io 不稳定。
// GPG/RPM 签名密钥固定从 k8sSigningKeyMinor 仓库拉取，避免旧版仓库密钥过期。
func K8sRepos(k8sMinor string) []Repo {
	if k8sMinor == "" {
		k8sMinor = "v1.27" // 保底默认，通常由调用方从版本推导
	}
	keyMinor := k8sSigningKeyMinor
	keyDest := "/etc/apt/keyrings/kubernetes-apt-keyring.gpg"
	return []Repo{{
		Name: "kubernetes",
		AptLine: fmt.Sprintf(
			"deb [signed-by=%s] https://mirrors.aliyun.com/kubernetes-new/core/stable/%s/deb/ /",
			keyDest, k8sMinor),
		AptKeyURL:  fmt.Sprintf("https://mirrors.aliyun.com/kubernetes-new/core/stable/%s/deb/Release.key", keyMinor),
		AptKeyDest: keyDest,
		DnfRepoBlock: fmt.Sprintf(`[kubernetes]
name=Kubernetes (stable %s)
baseurl=https://mirrors.aliyun.com/kubernetes-new/core/stable/%s/rpm/
enabled=1
gpgcheck=0
gpgkey=https://mirrors.aliyun.com/kubernetes-new/core/stable/%s/rpm/repodata/repomd.xml.key`, k8sMinor, k8sMinor, keyMinor),
		DnfKeyURL: fmt.Sprintf("https://mirrors.aliyun.com/kubernetes-new/core/stable/%s/rpm/repodata/repomd.xml.key", keyMinor),
	}}
}

// containerdMirror 描述 containerd（docker-ce）软件源镜像：URL 前缀与 dnf rpm 路径段。
type containerdMirror struct {
	// host URL 前缀（含 /docker-ce 段；官方 download.docker.com 无该段）。
	host string
	// rpmDir dnf baseurl/gpgkey 的发行版段（国内镜像 centos；官方 rhel）。
	rpmDir string
}

// containerdMirrors 按 repoType 映射 containerd 源镜像。
// aliyun=阿里云（默认）、ustc=中科大、tuna=清华（对齐 kubez openEuler/Kylin）、docker=官方。
// openEuler 使用系统源（repoType=none），不落入本映射。
var containerdMirrors = map[string]containerdMirror{
	"aliyun": {host: "mirrors.aliyun.com/docker-ce", rpmDir: "centos"},
	"ustc":   {host: "mirrors.ustc.edu.cn/docker-ce", rpmDir: "centos"},
	"tuna":   {host: "mirrors.tuna.tsinghua.edu.cn/docker-ce", rpmDir: "centos"},
	"docker": {host: "download.docker.com", rpmDir: "rhel"},
}

// dockerCERepoBlockEL7 生成对齐 kubez-ansible docker-ce.repo-openEuler.j2 的 dnf repo 块。
// 关键点：固定 linux/centos/7（不用 $releasever，避免 Kylin 上 releasever≠7 指错仓库），
// 段名/name/gpg 与 j2 一致；stable 启用，其余段保持 enabled=0。
func dockerCERepoBlockEL7(host string) string {
	return fmt.Sprintf(`[docker-ce-stable]
name=Docker CE Stable - $basearch
baseurl=https://%s/linux/centos/7/$basearch/stable
enabled=1
gpgcheck=1
gpgkey=https://%s/linux/centos/gpg

[docker-ce-stable-debuginfo]
name=Docker CE Stable - Debuginfo $basearch
baseurl=https://%s/linux/centos/7/debug-$basearch/stable
enabled=0
gpgcheck=1
gpgkey=https://%s/linux/centos/gpg

[docker-ce-stable-source]
name=Docker CE Stable - Sources
baseurl=https://%s/linux/centos/7/source/stable
enabled=0
gpgcheck=1
gpgkey=https://%s/linux/centos/gpg

[docker-ce-test]
name=Docker CE Test - $basearch
baseurl=https://%s/linux/centos/7/$basearch/test
enabled=0
gpgcheck=1
gpgkey=https://%s/linux/centos/gpg

[docker-ce-test-debuginfo]
name=Docker CE Test - Debuginfo $basearch
baseurl=https://%s/linux/centos/7/debug-$basearch/test
enabled=0
gpgcheck=1
gpgkey=https://%s/linux/centos/gpg

[docker-ce-test-source]
name=Docker CE Test - Sources
baseurl=https://%s/linux/centos/7/source/test
enabled=0
gpgcheck=1
gpgkey=https://%s/linux/centos/gpg

[docker-ce-nightly]
name=Docker CE Nightly - $basearch
baseurl=https://%s/linux/centos/7/$basearch/nightly
enabled=0
gpgcheck=1
gpgkey=https://%s/linux/centos/gpg

[docker-ce-nightly-debuginfo]
name=Docker CE Nightly - Debuginfo $basearch
baseurl=https://%s/linux/centos/7/debug-$basearch/nightly
enabled=0
gpgcheck=1
gpgkey=https://%s/linux/centos/gpg

[docker-ce-nightly-source]
name=Docker CE Nightly - Sources
baseurl=https://%s/linux/centos/7/source/nightly
enabled=0
gpgcheck=1
gpgkey=https://%s/linux/centos/gpg`,
		host, host,
		host, host,
		host, host,
		host, host,
		host, host,
		host, host,
		host, host,
		host, host,
		host, host,
	)
}

// CentOS7ExtrasRepos 返回 CentOS 7 extras 归档源（vault）。
// 用途：Kylin / openEuler 等 el7 兼容系统安装 docker-ce 时，
// docker-ce-rootless-extras 硬依赖 fuse-overlayfs>=0.7、slirp4netns>=0.4，
// 麒麟/欧拉系统源通常不提供这两包，需从 CentOS 7 extras 拉取。
// 默认走阿里云 centos-vault，与 builder 国内镜像策略一致。
func CentOS7ExtrasRepos() []Repo {
	return []Repo{{
		Name: "centos7-extras",
		DnfRepoBlock: `[centos7-extras]
name=CentOS-7 - Extras (vault)
baseurl=https://mirrors.aliyun.com/centos-vault/7.9.2009/extras/$basearch/
enabled=1
gpgcheck=0`,
	}}
}

// PackageListHasPrefix 判断软件包清单是否包含以 name 开头的条目
//（精确匹配 name，或 name-version / name=version / NEVRA 前缀）。
func PackageListHasPrefix(pkgs []string, name string) bool {
	for _, p := range pkgs {
		if p == name || strings.HasPrefix(p, name+"-") || strings.HasPrefix(p, name+"=") {
			return true
		}
	}
	return false
}

// NeedsCentOS7Extras 判断是否需追加 CentOS 7 extras 源：
// rpmDistro 为 rhel7（Kylin/openEuler 等）且清单含 docker-ce 时返回 true。
func NeedsCentOS7Extras(rpmDistro string, pkgs []string) bool {
	return rpmDistro == "rhel7" && PackageListHasPrefix(pkgs, "docker-ce")
}

// IsSystemContainerdPkg 判断是否为系统源 containerd 包（containerd / containerd-version），
// 排除 docker-ce 源的 containerd.io。
func IsSystemContainerdPkg(p string) bool {
	p = strings.TrimSpace(p)
	if p == "containerd" {
		return true
	}
	return strings.HasPrefix(p, "containerd-") && !strings.HasPrefix(p, "containerd.io")
}

// SplitSystemContainerdAndDockerCE 在同清单同时含系统 containerd 与 docker-ce 时拆成两批下载：
//   primary：去掉 docker-ce*（保留系统 containerd + k8s 等）
//   dockerBatch：docker-ce* + containerd.io（docker-ce 硬依赖；与系统 containerd Conflicts）
// 无需拆分时返回 (pkgs, nil)。离线包可同时收录两套运行时 RPM，安装时二选一。
func SplitSystemContainerdAndDockerCE(pkgs []string) (primary, dockerBatch []string) {
	hasSys, hasDocker := false, false
	for _, p := range pkgs {
		if IsSystemContainerdPkg(p) {
			hasSys = true
		}
		if PackageListHasPrefix([]string{p}, "docker-ce") {
			hasDocker = true
		}
	}
	if !(hasSys && hasDocker) {
		return pkgs, nil
	}
	for _, p := range pkgs {
		if PackageListHasPrefix([]string{p}, "docker-ce") {
			dockerBatch = append(dockerBatch, p)
			continue
		}
		primary = append(primary, p)
	}
	if !PackageListHasPrefix(dockerBatch, "containerd.io") {
		dockerBatch = append([]string{"containerd.io"}, dockerBatch...)
	}
	return primary, dockerBatch
}

// ContainerdRepos 返回 containerd 源（docker-ce 源）。
// repoType 决定源镜像：aliyun=阿里云 mirrors.aliyun.com/docker-ce（默认，空值同）；
// ustc=中科大 mirrors.ustc.edu.cn/docker-ce；tuna=清华 mirrors.tuna.tsinghua.edu.cn/docker-ce
// （对齐 kubez-ansible docker-ce.repo-openEuler.j2）；docker=官方 download.docker.com。
// 注：containerd.io 包由 docker-ce 源提供。原 packages.containerd.io 域名在当前网络与
// 公共 DNS（223.5.5.5 / 8.8.8.8）均 NXDOMAIN 无法解析，已改用国内镜像（默认阿里云）提供 containerd.io 包。
// aptOS 为 apt 发行版家族（ubuntu/debian）；codename 为 apt 版本代号（jammy/bookworm 等）；
// rpmDistro 为 dnf 发行版标识（rhel9/rhel7 等，rocky→rhel9、openEuler/Kylin→rhel7）。
// rhel7：dnf 块对齐 docker-ce.repo-openEuler.j2（固定 centos/7，写入 docker-ce.repo）；
// 其他：单段 [docker-ce-stable]，baseurl 为 {rpmDir}/{major}。
func ContainerdRepos(aptOS, codename, rpmDistro, repoType string) []Repo {
	if aptOS == "" {
		aptOS = "ubuntu"
	}
	m, ok := containerdMirrors[repoType]
	if !ok {
		m = containerdMirrors["aliyun"] // 默认阿里云（含空串/未知值）
	}
	// major 由 rpmDistro 去掉 rhel 前缀得到（rhel9→9、rhel7→7）；非 rhel 前缀原样保留。
	major := rpmDistro
	if strings.HasPrefix(rpmDistro, "rhel") {
		major = strings.TrimPrefix(rpmDistro, "rhel")
	}
	keyDest := "/etc/apt/keyrings/containerd-apt-keyring.gpg"

	// el7（Kylin/CentOS7 等）：对齐 kubez docker-ce.repo-openEuler.j2。
	// 固定 centos/7；dnf host 默认强制 tuna——阿里云对部分 IP 拉取 centos/7 包会 403，
	// 中科大 ustc 的 centos/7 路径常 404；仅 containerd_repo=docker 时走官方。
	if major == "7" {
		host := containerdMirrors["tuna"].host
		if repoType == "docker" {
			host = "download.docker.com"
		}
		return []Repo{{
			Name: "docker-ce", // 与 kubez dest /etc/yum.repos.d/docker-ce.repo 一致
			AptLine: fmt.Sprintf(
				"deb [signed-by=%s] https://%s/linux/%s %s stable",
				keyDest, m.host, aptOS, codename),
			AptKeyURL:    fmt.Sprintf("https://%s/linux/%s/gpg", m.host, aptOS),
			AptKeyDest:   keyDest,
			DnfRepoBlock: dockerCERepoBlockEL7(host),
			DnfKeyURL:    fmt.Sprintf("https://%s/linux/centos/gpg", host),
		}}
	}

	return []Repo{{
		Name: "containerd",
		AptLine: fmt.Sprintf(
			"deb [signed-by=%s] https://%s/linux/%s %s stable",
			keyDest, m.host, aptOS, codename),
		AptKeyURL:  fmt.Sprintf("https://%s/linux/%s/gpg", m.host, aptOS),
		AptKeyDest: keyDest,
		DnfRepoBlock: fmt.Sprintf(`[docker-ce-stable]
name=docker-ce-stable
baseurl=https://%s/linux/%s/%s/$basearch/stable
enabled=1
gpgcheck=1
gpgkey=https://%s/linux/%s/gpg`, m.host, m.rpmDir, major, m.host, m.rpmDir),
		DnfKeyURL: fmt.Sprintf("https://%s/linux/%s/gpg", m.host, m.rpmDir),
	}}
}

// AptSourceScript 生成容器内配置 apt 源的 shell 片段：
// 下载并 dearmor GPG key，写入 /etc/apt/sources.list.d/。
func AptSourceScript(repos []Repo) string {
	var b strings.Builder
	b.WriteString("mkdir -p /etc/apt/keyrings\n")
	for _, r := range repos {
		if r.AptLine == "" || r.AptKeyURL == "" || r.AptKeyDest == "" {
			continue
		}
		// 先删除旧 key，避免 gpg --dearmor 因目标已存在失败，或残留过期密钥。
		b.WriteString("rm -f " + r.AptKeyDest + "\n")
		b.WriteString("curl -fsSL " + r.AptKeyURL + " | gpg --dearmor -o " + r.AptKeyDest + "\n")
		b.WriteString("chmod 644 " + r.AptKeyDest + "\n")
		b.WriteString("echo '" + r.AptLine + "' > /etc/apt/sources.list.d/" + r.Name + ".list\n")
	}
	return b.String()
}

// DnfSourceScript 生成容器内配置 dnf/yum 源的 shell 片段：
// 写入 /etc/yum.repos.d/ 并 rpm --import 导入 GPG key。
// CentOS 7 等使用 yum 的系统同样适用（yum 与 dnf 共用 /etc/yum.repos.d/ 与 rpm --import 语法）。
func DnfSourceScript(repos []Repo) string {
	var b strings.Builder
	for _, r := range repos {
		if r.DnfRepoBlock == "" {
			continue
		}
		b.WriteString("cat > /etc/yum.repos.d/" + r.Name + ".repo <<'REPO'\n")
		b.WriteString(r.DnfRepoBlock + "\nREPO\n")
		if r.DnfKeyURL != "" {
			b.WriteString("rpm --import " + r.DnfKeyURL + "\n")
		}
	}
	return b.String()
}

// BuildPackageList 按包管理器生成容器内下载的软件包清单：
// k8s 三件套（kubeadm/kubelet/kubectl） + 运行时（containerdPkg/cri-tools） + 系统依赖。
// 注：runc 由 containerd 包（containerd.io 或系统源 containerd）内嵌提供，不单独安装，
// 避免 docker-ce 源的 containerd.io 与独立 runc 包存在 Conflicts: runc 冲突导致 apt 无法同时解析。
// pinK8s=true 且 k8sVersion 非空时，对 k8s 三件套按 --kubernetes-version 精确锁定到对应 patch：
//   - apt：pkg=X.Y.Z-1.1（deb 的 version-release 固定为 1.1）
//   - dnf/yum：pkg-X.Y.Z（只锁 version，与 kubez-ansible 一致；不写 pkg-X.Y.Z.arch，
//     因为那不是合法 NEVRA——dnf 会把 1.31.6.x86_64 当成 version。多架构仓库冲突
//     由下载脚本 repoquery --arch= 解析为 name-ver-rel.arch 后再下载，见 BuildDownloadScript）
// arch 保留以兼容调用方；当前不写入包名（架构在下载脚本中约束）。
// 源内无该 patch 时 apt/dnf 依赖解析失败即构建失败，不会静默回退同 minor 最新 patch。
// 默认 false（或版本为空）使用源内 stable 最新版本。
// containerdPkg 为空时默认 "containerd.io"（docker-ce 源包名）；openEuler 等系统源场景传 "containerd"。
func BuildPackageList(pkgManager, k8sVersion string, deps []string, pinK8s bool, containerdPkg, arch string) []string {
	_ = arch // 架构约束走下载脚本 repoquery --arch=，不写入包名
	if containerdPkg == "" {
		containerdPkg = "containerd.io"
	}
	ver := strings.TrimPrefix(k8sVersion, "v")
	k8sPkgs := []string{"kubeadm", "kubelet", "kubectl"}
	if pinK8s && ver != "" {
		for i, p := range k8sPkgs {
			if pkgManager == "dnf" || pkgManager == "yum" {
				// 与 kubez-ansible（kubeadm-{{ kube_release }}）一致：只钉 version
				k8sPkgs[i] = p + "-" + ver
			} else {
				k8sPkgs[i] = p + "=" + ver + "-1.1"
			}
		}
	}
	out := make([]string, 0, len(k8sPkgs)+2+len(deps))
	out = append(out, k8sPkgs...)
	// runc 由 containerd 包内嵌提供，不单独安装（避免 Conflicts: runc 冲突）
	out = append(out, containerdPkg, "cri-tools")
	out = append(out, deps...)
	return out
}

// RPMArch 将构建架构（amd64/arm64）映射为 rpm 架构名（x86_64/aarch64）。
// 未知架构返回空串（调用方可不追加架构后缀）。
func RPMArch(arch string) string {
	switch strings.ToLower(strings.TrimSpace(arch)) {
	case "amd64", "x86_64":
		return "x86_64"
	case "arm64", "aarch64":
		return "aarch64"
	default:
		return ""
	}
}
