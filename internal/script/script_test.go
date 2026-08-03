package script

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRenderInstall(t *testing.T) {
	files, err := Render(Data{K8sVersion: "v1.27.3", ImageRepository: "registry.k8s.io"})
	if err != nil {
		t.Fatalf("Render 失败: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("期望 2 个文件，实际 %d", len(files))
	}
	install := files[0]
	if install.Name != "install.sh" {
		t.Errorf("文件名异常: %s", install.Name)
	}
	if install.Mode != 0o755 {
		t.Errorf("install.sh 应可执行，mode=%o", install.Mode)
	}
	for _, want := range []string{
		"#!/usr/bin/env bash",
		"set -eu",
		"v1.27.3",
		"registry.k8s.io",
		// 包安装
		"dpkg -i",
		"apt-get -f install -y",
		"rpm -ivh --nodeps",
		"packages/*.deb",
		"packages/*.rpm",
		// crictl 静态回退
		"packages/runtime/crictl-*.tar.gz",
		"install -m 0755 /tmp/crictl /usr/local/bin/crictl",
		// preflight
		"swapoff -a",
		"modprobe",
		"net.bridge.bridge-nf-call-iptables = 1",
		"net.bridge.bridge-nf-call-ip6tables = 1",
		"net.ipv4.ip_forward = 1",
		"sysctl --system",
		// containerd / kubelet
		"containerd config default",
		"SystemdCgroup = true",
		"systemctl enable --now containerd kubelet",
		// 自检与提示
		"kubeadm version",
		"kubeadm init",
		"load-images.sh",
	} {
		if !strings.Contains(install.Content, want) {
			t.Errorf("install.sh 缺少 %q", want)
		}
	}
	// 已移除静态二进制解压逻辑
	for _, gone := range []string{
		"tar -C /usr/local -xzf",
		"containerd.service",
		"runc.*",
	} {
		if strings.Contains(install.Content, gone) {
			t.Errorf("install.sh 不应再包含 %q", gone)
		}
	}
}

func TestRenderInstallBashSyntax(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash 不可用，跳过 bash -n 校验")
	}
	files, err := Render(Data{K8sVersion: "v1.27.3"})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		cmd := exec.Command("bash", "-n")
		cmd.Stdin = strings.NewReader(f.Content)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("%s bash -n 语法检查失败: %v\n%s", f.Name, err, out)
		}
	}
}

func TestRenderLoadImages(t *testing.T) {
	files, _ := Render(Data{K8sVersion: "v1.28.2"})
	load := files[1]
	if load.Name != "load-images.sh" {
		t.Errorf("文件名异常: %s", load.Name)
	}
	for _, want := range []string{
		"docker load -i",
		"ctr -n k8s.io images import",
		"images/core/*.tar",
		"images/addons/*.tar",
	} {
		if !strings.Contains(load.Content, want) {
			t.Errorf("load-images.sh 缺少 %q", want)
		}
	}
}

func TestWriteDir(t *testing.T) {
	dir := t.TempDir()
	paths, err := WriteDir(dir, Data{K8sVersion: "v1.27.3"})
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 2 {
		t.Fatalf("期望 2 个脚本，实际 %d", len(paths))
	}
	for _, p := range paths {
		st, err := os.Stat(p)
		if err != nil {
			t.Fatalf("脚本未生成: %v", err)
		}
		if st.Mode()&0o111 == 0 {
			t.Errorf("%s 缺少执行权限: %o", p, st.Mode())
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "install.sh")); err != nil {
		t.Error("install.sh 未写入")
	}
}

func TestDefaultImageRepository(t *testing.T) {
	files, _ := Render(Data{K8sVersion: "v1.27.3"})
	if !strings.Contains(files[0].Content, "--image-repository registry.k8s.io") {
		t.Error("默认镜像仓库应为 registry.k8s.io")
	}
}

func TestDefaultGeneratedAt(t *testing.T) {
	files, _ := Render(Data{K8sVersion: "v1.27.3"})
	if !strings.Contains(files[0].Content, "生成时间 : ") {
		t.Error("应包含默认生成时间")
	}
}
