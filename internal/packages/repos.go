// Package packages 在目标系统容器内通过包管理器（apt/dnf）递归下载
// k8s 组件、容器运行时与系统依赖包，并生成对应的软件源（pkgs.k8s.io / download.docker.com）。
// 依赖闭包交由包管理器处理；docker 不可用时该步骤标记为 skipped。
package packages

import (
	"fmt"
	"strings"
)

// Repo 描述一个需要配置的软件源。apt 与 dnf 二选一（按目标 OS 包管理器使用）。
// 注意：源 URL 以 pkgs.k8s.io / download.docker.com 官方结构为准，下载前已用 curl 实测可达。
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
// 官方建议改用较新仓库（≥v1.31）的密钥校验所有 pkgs.k8s.io 仓库。
// 参见 https://github.com/kubernetes/release/issues/3818
const k8sSigningKeyMinor = "v1.31"

// K8sRepos 返回 k8s 组件源（pkgs.k8s.io）。
// k8sMinor 形如 v1.27，由 k8s 版本前两段推导（决定软件包仓库路径）。
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
			"deb [signed-by=%s] https://pkgs.k8s.io/core:/stable:/%s/deb/ /",
			keyDest, k8sMinor),
		AptKeyURL:  fmt.Sprintf("https://pkgs.k8s.io/core:/stable:/%s/deb/Release.key", keyMinor),
		AptKeyDest: keyDest,
		DnfRepoBlock: fmt.Sprintf(`[kubernetes]
name=Kubernetes (stable %s)
baseurl=https://pkgs.k8s.io/core:/stable:/%s/rpm/
enabled=1
gpgcheck=1
gpgkey=https://pkgs.k8s.io/core:/stable:/%s/rpm/repodata/repomd.xml.key`, k8sMinor, k8sMinor, keyMinor),
		DnfKeyURL: fmt.Sprintf("https://pkgs.k8s.io/core:/stable:/%s/rpm/repodata/repomd.xml.key", keyMinor),
	}}
}

// ContainerdRepos 返回 containerd 源（docker 官方源 download.docker.com）。
// 注：containerd.io 包改由 docker 官方源提供。原 packages.containerd.io 域名在当前网络与
// 公共 DNS（223.5.5.5 / 8.8.8.8）均 NXDOMAIN 无法解析；download.docker.com 各发行版实测可达
// （ubuntu noble / rhel9 / containerd.io 包均 HTTP 200）。
// aptOS 为 apt 发行版家族（ubuntu/debian）；codename 为 apt 版本代号（jammy/bookworm 等）；
// rpmDistro 为 dnf 发行版标识（rhel9/rhel7 等，rocky→rhel9、openEuler→rhel7），
// 代码内按 download.docker.com 路径规则转换为 rhel/{大版本}。
func ContainerdRepos(aptOS, codename, rpmDistro string) []Repo {
	if aptOS == "" {
		aptOS = "ubuntu"
	}
	keyDest := "/etc/apt/keyrings/containerd-apt-keyring.gpg"
	// basePath 为 dnf baseurl 发行版段（rhel9→rhel/9）；gpgPath 为 gpg key 发行版段（rhel）。
	basePath, gpgPath := dockerRPMBase(rpmDistro)
	return []Repo{{
		Name: "containerd",
		AptLine: fmt.Sprintf(
			"deb [signed-by=%s] https://download.docker.com/linux/%s %s stable",
			keyDest, aptOS, codename),
		AptKeyURL:  fmt.Sprintf("https://download.docker.com/linux/%s/gpg", aptOS),
		AptKeyDest: keyDest,
		DnfRepoBlock: fmt.Sprintf(`[docker-ce-stable]
name=docker-ce-stable
baseurl=https://download.docker.com/linux/%s/$basearch/stable
enabled=1
gpgcheck=1
gpgkey=https://download.docker.com/linux/%s/gpg`, basePath, gpgPath),
		DnfKeyURL: fmt.Sprintf("https://download.docker.com/linux/%s/gpg", gpgPath),
	}}
}

// dockerRPMBase 把 dnf 发行版标识转换为 download.docker.com rpm 源的发行版段：
// baseurl 用 rhel/{大版本}（rhel9→rhel/9），gpg key 用 rhel（download.docker.com 的 gpg
// key 路径不带版本号，rhel/9/gpg 实测 404，rhel/gpg 实测 200）。非 rhel 前缀原样返回。
// 注：openEuler 对应 rhel7，download.docker.com/linux/rhel/7/ 实测 404（docker 官方已停止
// 发布 RHEL7 仓库），openEuler 场景 containerd dnf 源存在兼容性风险。
func dockerRPMBase(rpmDistro string) (basePath, gpgPath string) {
	if rpmDistro != "" && strings.HasPrefix(rpmDistro, "rhel") {
		return "rhel/" + strings.TrimPrefix(rpmDistro, "rhel"), "rhel"
	}
	return rpmDistro, rpmDistro
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
// 避免 download.docker.com 的 containerd.io 与独立 runc 包存在 Conflicts: runc 冲突导致 apt 无法同时解析。
// pinK8s=true 时对 k8s 组件做精确版本约束（apt: pkg=<ver>；dnf/yum: pkg-<ver>），
// 默认 false 使用源内 stable 最新版本。
// containerdPkg 为空时默认 "containerd.io"（docker 源包名）；openEuler 等系统源场景传 "containerd"。
func BuildPackageList(pkgManager, k8sVersion string, deps []string, pinK8s bool, containerdPkg string) []string {
	if containerdPkg == "" {
		containerdPkg = "containerd.io"
	}
	ver := strings.TrimPrefix(k8sVersion, "v")
	k8sPkgs := []string{"kubeadm", "kubelet", "kubectl"}
	if pinK8s {
		for i, p := range k8sPkgs {
			if pkgManager == "dnf" || pkgManager == "yum" {
				k8sPkgs[i] = p + "-" + ver
			} else {
				k8sPkgs[i] = p + "=" + ver
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
