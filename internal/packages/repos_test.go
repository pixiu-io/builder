package packages

import (
	"strings"
	"testing"
)

func TestK8sRepos(t *testing.T) {
	repos := K8sRepos("v1.27")
	if len(repos) != 1 {
		t.Fatalf("期望 1 个 repo，实际 %d", len(repos))
	}
	r := repos[0]
	// apt 软件包仓库指向阿里云目标小版本；签名密钥从阿里云 v1.31 拉取（规避旧版密钥过期与 EXPKEYSIG）
	for _, want := range []string{
		"https://mirrors.aliyun.com/kubernetes-new/core/stable/v1.27/deb/ /",
		"kubernetes-apt-keyring.gpg",
		"https://mirrors.aliyun.com/kubernetes-new/core/stable/v1.31/deb/Release.key",
		"[kubernetes]",
		"https://mirrors.aliyun.com/kubernetes-new/core/stable/v1.27/rpm/",
		"https://mirrors.aliyun.com/kubernetes-new/core/stable/v1.31/rpm/repodata/repomd.xml.key",
	} {
		if !strings.Contains(r.AptLine+r.AptKeyURL+r.DnfRepoBlock+r.DnfKeyURL, want) {
			t.Errorf("k8s repo 缺少 %q\nAptKeyURL=%s\nDnfKeyURL=%s", want, r.AptKeyURL, r.DnfKeyURL)
		}
	}
	if strings.Contains(r.AptKeyURL, "v1.27") {
		t.Errorf("AptKeyURL 不应再使用过期的 v1.27 Release.key: %s", r.AptKeyURL)
	}
	// 回归：exclude=kubelet kubeadm kubectl 不应加在 k8s 源自身（会过滤掉本源的 k8s 包，
	// 导致 dnf 报 "All matches were filtered out by exclude filtering"）。
	// exclude 的本意是防止系统源提供旧版 k8s 包，应加在系统源而非 k8s 源。
	for _, forbid := range []string{"exclude=kubelet", "exclude=kubeadm", "exclude=kubectl", "exclude="} {
		if strings.Contains(r.DnfRepoBlock, forbid) {
			t.Errorf("k8s dnf repo 不应含 %q:\n%s", forbid, r.DnfRepoBlock)
		}
	}
	// 与 kubez-ansible kubernetes.repo.j2 一致：gpgcheck=0
	if !strings.Contains(r.DnfRepoBlock, "gpgcheck=0") {
		t.Errorf("k8s dnf repo 应为 gpgcheck=0（对齐 kubez）:\n%s", r.DnfRepoBlock)
	}
	// [kubernetes] 块只保留 name/baseurl/enabled/gpgcheck/gpgkey，以 gpgkey 行收尾
	if !strings.HasSuffix(strings.TrimSpace(r.DnfRepoBlock), "repomd.xml.key") {
		t.Errorf("k8s dnf repo 应以 gpgkey 行收尾（不应有多余 exclude 行）:\n%s", r.DnfRepoBlock)
	}
}

func TestContainerdRepos(t *testing.T) {
	// 默认 aliyun（国内镜像 mirrors.aliyun.com/docker-ce）
	repos := ContainerdRepos("ubuntu", "jammy", "rhel9", "aliyun")
	if len(repos) != 1 {
		t.Fatalf("期望 1 个 repo，实际 %d", len(repos))
	}
	r := repos[0]
	for _, want := range []string{
		"https://mirrors.aliyun.com/docker-ce/linux/ubuntu jammy stable",
		"containerd-apt-keyring.gpg",
		"https://mirrors.aliyun.com/docker-ce/linux/ubuntu/gpg",
		"[docker-ce-stable]",
		"https://mirrors.aliyun.com/docker-ce/linux/centos/9/$basearch/stable",
		"https://mirrors.aliyun.com/docker-ce/linux/centos/gpg",
	} {
		if !strings.Contains(r.AptLine+r.AptKeyURL+r.DnfRepoBlock+r.DnfKeyURL, want) {
			t.Errorf("containerd repo 缺少 %q", want)
		}
	}

	// ustc（中科大 mirrors.ustc.edu.cn/docker-ce）
	ustc := ContainerdRepos("ubuntu", "noble", "", "ustc")
	for _, want := range []string{
		"https://mirrors.ustc.edu.cn/docker-ce/linux/ubuntu noble stable",
		"https://mirrors.ustc.edu.cn/docker-ce/linux/ubuntu/gpg",
	} {
		if !strings.Contains(ustc[0].AptLine+ustc[0].AptKeyURL, want) {
			t.Errorf("ustc repo 缺少 %q", want)
		}
	}

	// tuna + rhel7：对齐 kubez-ansible docker-ce.repo-openEuler.j2
	tuna := ContainerdRepos("", "", "rhel7", "tuna")
	if tuna[0].Name != "docker-ce" {
		t.Errorf("el7 repo Name = %q, want docker-ce", tuna[0].Name)
	}
	for _, want := range []string{
		"Docker CE Stable - $basearch",
		"https://mirrors.tuna.tsinghua.edu.cn/docker-ce/linux/centos/7/$basearch/stable",
		"https://mirrors.tuna.tsinghua.edu.cn/docker-ce/linux/centos/gpg",
		"[docker-ce-nightly-source]",
		"enabled=0",
	} {
		if !strings.Contains(tuna[0].DnfRepoBlock+tuna[0].DnfKeyURL, want) {
			t.Errorf("tuna el7 repo 缺少 %q:\n%s", want, tuna[0].DnfRepoBlock)
		}
	}
	// 不应使用 $releasever（Kylin 上会指错仓库）
	if strings.Contains(tuna[0].DnfRepoBlock, "$releasever") {
		t.Errorf("el7 docker-ce repo 不应含 $releasever:\n%s", tuna[0].DnfRepoBlock)
	}

	// el7 即使配置 aliyun，dnf 也强制 tuna（规避阿里云 centos/7 包 403）
	aliyunEL7 := ContainerdRepos("", "", "rhel7", "aliyun")
	if !strings.Contains(aliyunEL7[0].DnfRepoBlock, "mirrors.tuna.tsinghua.edu.cn/docker-ce/linux/centos/7") {
		t.Errorf("el7+aliyun 应强制 tuna dnf host:\n%s", aliyunEL7[0].DnfRepoBlock)
	}
	if strings.Contains(aliyunEL7[0].DnfRepoBlock, "mirrors.aliyun.com/docker-ce") {
		t.Errorf("el7 dnf 不应再含 aliyun docker-ce:\n%s", aliyunEL7[0].DnfRepoBlock)
	}

	// docker 官方保留（download.docker.com，rpm 段为 rhel）
	docker := ContainerdRepos("ubuntu", "jammy", "rhel9", "docker")
	for _, want := range []string{
		"https://download.docker.com/linux/rhel/9/$basearch/stable",
		"https://download.docker.com/linux/rhel/gpg",
	} {
		if !strings.Contains(docker[0].DnfRepoBlock+docker[0].DnfKeyURL, want) {
			t.Errorf("docker repo 缺少 %q", want)
		}
	}

	// 空 repoType 默认 aliyun
	def := ContainerdRepos("debian", "bookworm", "", "")
	if !strings.Contains(def[0].AptLine, "https://mirrors.aliyun.com/docker-ce/linux/debian bookworm stable") {
		t.Errorf("空 repoType 应默认 aliyun: %q", def[0].AptLine)
	}
}

func TestAptSourceScript(t *testing.T) {
	s := AptSourceScript(ContainerdRepos("ubuntu", "jammy", "", ""))
	for _, want := range []string{
		"mkdir -p /etc/apt/keyrings",
		"rm -f",
		"curl -fsSL",
		"| gpg --dearmor -o",
		"chmod 644",
		"containerd.list",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("apt 源脚本缺少 %q:\n%s", want, s)
		}
	}
}

func TestDnfSourceScript(t *testing.T) {
	s := DnfSourceScript(K8sRepos("v1.28"))
	for _, want := range []string{
		"/etc/yum.repos.d/kubernetes.repo",
		"[kubernetes]",
		"rpm --import",
		"REPO",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("dnf 源脚本缺少 %q:\n%s", want, s)
		}
	}
}

func TestBuildPackageList(t *testing.T) {
	deps := []string{"conntrack", "nfs-common"}
	got := BuildPackageList("apt", "v1.27.3", deps, false, "containerd.io", "amd64")
	want := []string{"kubeadm", "kubelet", "kubectl", "containerd.io", "cri-tools", "conntrack", "nfs-common"}
	if len(got) != len(want) {
		t.Fatalf("期望 %d 个包，实际 %d: %v", len(want), len(got), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("位置 %d 期望 %q 实际 %q", i, want[i], got[i])
		}
	}
}

func TestBuildPackageListCustomContainerdPkg(t *testing.T) {
	// openEuler 等系统源场景：containerd 包名为 "containerd"（非 docker-ce 源 containerd.io）。
	got := BuildPackageList("dnf", "v1.35.7", []string{"conntrack"}, false, "containerd", "amd64")
	want := []string{"kubeadm", "kubelet", "kubectl", "containerd", "cri-tools", "conntrack"}
	if len(got) != len(want) {
		t.Fatalf("期望 %d 个包，实际 %d: %v", len(want), len(got), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("位置 %d 期望 %q 实际 %q", i, want[i], got[i])
		}
	}
	// 空值保底默认 containerd.io
	def := BuildPackageList("dnf", "v1.35.7", nil, false, "", "amd64")
	for _, p := range def {
		if p == "containerd" {
			t.Errorf("空 containerdPkg 不应出现 containerd: %v", def)
		}
	}
	hasIO := false
	for _, p := range def {
		if p == "containerd.io" {
			hasIO = true
		}
	}
	if !hasIO {
		t.Errorf("空 containerdPkg 应默认 containerd.io: %v", def)
	}
}

func TestBuildPackageListPin(t *testing.T) {
	apt := BuildPackageList("apt", "v1.27.3", nil, true, "", "amd64")
	if apt[0] != "kubeadm=1.27.3-1.1" || apt[1] != "kubelet=1.27.3-1.1" || apt[2] != "kubectl=1.27.3-1.1" {
		t.Errorf("apt 版本约束异常（应为 X.Y.Z-1.1）: %v", apt[:3])
	}
	dnf := BuildPackageList("dnf", "v1.28.2", nil, true, "", "amd64")
	if dnf[0] != "kubeadm-1.28.2" || dnf[1] != "kubelet-1.28.2" || dnf[2] != "kubectl-1.28.2" {
		t.Errorf("dnf 版本约束异常（应为 X.Y.Z，架构走 repoquery --arch=）: %v", dnf[:3])
	}
	dnfArm := BuildPackageList("dnf", "v1.28.2", nil, true, "", "arm64")
	if dnfArm[0] != "kubeadm-1.28.2" {
		t.Errorf("dnf arm64 包名仍应只钉 version: %v", dnfArm[:3])
	}
	unpin := BuildPackageList("apt", "v1.27.3", nil, false, "", "amd64")
	if unpin[0] != "kubeadm" {
		t.Errorf("默认不 pin 版本: %v", unpin[:3])
	}
	// pinK8s=true 但版本为空（--only-addons 未指定 k8s 版本）：不应生成裸 `pkg=` / `pkg-`
	empty := BuildPackageList("apt", "", nil, true, "", "amd64")
	if empty[0] != "kubeadm" || empty[1] != "kubelet" || empty[2] != "kubectl" {
		t.Errorf("版本为空时不应 pin: %v", empty[:3])
	}
	emptyDnf := BuildPackageList("dnf", "", nil, true, "", "amd64")
	if emptyDnf[0] != "kubeadm" {
		t.Errorf("版本为空时不应 pin（dnf）: %v", emptyDnf[:3])
	}
}

func TestRPMArch(t *testing.T) {
	cases := []struct{ in, want string }{
		{"amd64", "x86_64"},
		{"x86_64", "x86_64"},
		{"arm64", "aarch64"},
		{"aarch64", "aarch64"},
		{"", ""},
		{"riscv64", ""},
	}
	for _, c := range cases {
		if got := RPMArch(c.in); got != c.want {
			t.Errorf("RPMArch(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestCentOS7ExtrasRepos(t *testing.T) {
	repos := CentOS7ExtrasRepos()
	if len(repos) != 1 {
		t.Fatalf("期望 1 个 repo，实际 %d", len(repos))
	}
	r := repos[0]
	for _, want := range []string{
		"[centos7-extras]",
		"mirrors.aliyun.com/centos-vault/7.9.2009/extras/$basearch/",
		"gpgcheck=0",
	} {
		if !strings.Contains(r.DnfRepoBlock, want) {
			t.Errorf("centos7-extras 缺少 %q:\n%s", want, r.DnfRepoBlock)
		}
	}
}

func TestNeedsCentOS7Extras(t *testing.T) {
	cases := []struct {
		distro string
		pkgs   []string
		want   bool
	}{
		{"rhel7", []string{"kubeadm", "docker-ce"}, true},
		{"rhel7", []string{"docker-ce-26.1.4"}, true},
		{"rhel7", []string{"kubeadm", "containerd.io"}, false},
		{"rhel9", []string{"docker-ce"}, false},
		{"", []string{"docker-ce"}, false},
	}
	for _, c := range cases {
		if got := NeedsCentOS7Extras(c.distro, c.pkgs); got != c.want {
			t.Errorf("NeedsCentOS7Extras(%q, %v) = %v, want %v", c.distro, c.pkgs, got, c.want)
		}
	}
}

func TestSplitSystemContainerdAndDockerCE(t *testing.T) {
	// 无需拆分
	p, d := SplitSystemContainerdAndDockerCE([]string{"kubeadm", "containerd", "cri-tools"})
	if d != nil || len(p) != 3 {
		t.Fatalf("无 docker-ce 不应拆分: primary=%v docker=%v", p, d)
	}
	p, d = SplitSystemContainerdAndDockerCE([]string{"kubeadm", "containerd.io", "docker-ce"})
	if d != nil {
		t.Fatalf("containerd.io+docker-ce 同栈不应拆分: docker=%v", d)
	}

	// openEuler：系统 containerd + docker-ce → 两批
	p, d = SplitSystemContainerdAndDockerCE([]string{"kubeadm-1.31.6", "containerd", "cri-tools", "docker-ce", "ipset"})
	if d == nil {
		t.Fatal("应拆出 dockerBatch")
	}
	if !PackageListHasPrefix(p, "containerd") || PackageListHasPrefix(p, "docker-ce") {
		t.Errorf("primary 应含 containerd、不含 docker-ce: %v", p)
	}
	if !PackageListHasPrefix(d, "docker-ce") || !PackageListHasPrefix(d, "containerd.io") {
		t.Errorf("dockerBatch 应含 docker-ce + containerd.io: %v", d)
	}
	if PackageListHasPrefix(d, "kubeadm") {
		t.Errorf("dockerBatch 不应含 kubeadm: %v", d)
	}
}

func TestBuildDownloadScriptDNFSplitsContainerdAndDockerCE(t *testing.T) {
	s := BuildDownloadScript(DownloadScriptOpts{
		PkgManager: "dnf",
		Repos:      append(K8sRepos("v1.31"), CentOS7ExtrasRepos()...),
		Pkgs:       []string{"kubeadm-1.31.6", "containerd", "cri-tools", "docker-ce"},
		Arch:       "amd64",
	})
	if !strings.Contains(s, "分两批下载") {
		t.Errorf("应注明分两批下载:\n%s", s)
	}
	// 两批：第一批含 containerd；第二批含 containerd.io + docker-ce
	if !strings.Contains(s, "for p in kubeadm-1.31.6 containerd cri-tools;") {
		t.Errorf("第一批应含系统 containerd、不含 docker-ce:\n%s", s)
	}
	if !strings.Contains(s, "for p in containerd.io docker-ce;") {
		t.Errorf("第二批应含 containerd.io docker-ce:\n%s", s)
	}
	// downloadonly 应出现两次
	if strings.Count(s, "--downloadonly") != 2 {
		t.Errorf("--downloadonly 应出现 2 次，实际 %d:\n%s", strings.Count(s, "--downloadonly"), s)
	}
}
