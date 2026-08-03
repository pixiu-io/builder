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
	// 软件包仓库仍指向目标小版本；签名密钥改从已续期的 v1.31 拉取（规避 EXPKEYSIG）
	for _, want := range []string{
		"https://pkgs.k8s.io/core:/stable:/v1.27/deb/ /",
		"kubernetes-apt-keyring.gpg",
		"https://pkgs.k8s.io/core:/stable:/v1.31/deb/Release.key",
		"[kubernetes]",
		"https://pkgs.k8s.io/core:/stable:/v1.27/rpm/",
		"https://pkgs.k8s.io/core:/stable:/v1.31/rpm/repodata/repomd.xml.key",
	} {
		if !strings.Contains(r.AptLine+r.AptKeyURL+r.DnfRepoBlock+r.DnfKeyURL, want) {
			t.Errorf("k8s repo 缺少 %q\nAptKeyURL=%s\nDnfKeyURL=%s", want, r.AptKeyURL, r.DnfKeyURL)
		}
	}
	if strings.Contains(r.AptKeyURL, "v1.27") {
		t.Errorf("AptKeyURL 不应再使用过期的 v1.27 Release.key: %s", r.AptKeyURL)
	}
}

func TestContainerdRepos(t *testing.T) {
	repos := ContainerdRepos("ubuntu", "jammy", "rhel9")
	if len(repos) != 1 {
		t.Fatalf("期望 1 个 repo，实际 %d", len(repos))
	}
	r := repos[0]
	for _, want := range []string{
		"https://download.docker.com/linux/ubuntu jammy stable",
		"containerd-apt-keyring.gpg",
		"https://download.docker.com/linux/ubuntu/gpg",
		"[docker-ce-stable]",
		"https://download.docker.com/linux/rhel/9/$basearch/stable",
		"https://download.docker.com/linux/rhel/gpg",
	} {
		if !strings.Contains(r.AptLine+r.AptKeyURL+r.DnfRepoBlock+r.DnfKeyURL, want) {
			t.Errorf("containerd repo 缺少 %q", want)
		}
	}

	debian := ContainerdRepos("debian", "bookworm", "")
	if !strings.Contains(debian[0].AptLine, "https://download.docker.com/linux/debian bookworm stable") {
		t.Errorf("debian 源异常: %q", debian[0].AptLine)
	}
}

func TestAptSourceScript(t *testing.T) {
	s := AptSourceScript(ContainerdRepos("ubuntu", "jammy", ""))
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
	got := BuildPackageList("apt", "v1.27.3", deps, false)
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

func TestBuildPackageListPin(t *testing.T) {
	apt := BuildPackageList("apt", "v1.27.3", nil, true)
	if apt[0] != "kubeadm=1.27.3" {
		t.Errorf("apt 版本约束异常: %v", apt[:3])
	}
	dnf := BuildPackageList("dnf", "v1.28.2", nil, true)
	if dnf[0] != "kubeadm-1.28.2" {
		t.Errorf("dnf 版本约束异常: %v", dnf[:3])
	}
	unpin := BuildPackageList("apt", "v1.27.3", nil, false)
	if unpin[0] != "kubeadm" {
		t.Errorf("默认不 pin 版本: %v", unpin[:3])
	}
}
