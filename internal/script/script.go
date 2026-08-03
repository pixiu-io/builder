// Package script 渲染离线安装包内的 install.sh 与 load-images.sh。
// 脚本遵循 set -e，优先 docker load，回退 ctr import。
package script

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"text/template"
	"time"
)

// Data 脚本渲染数据。
type Data struct {
	K8sVersion      string
	ImageRepository string
	GeneratedAt     string
}

// File 生成的脚本文件。
type File struct {
	Name    string
	Content string
	Mode    os.FileMode
}

const installTpl = `#!/usr/bin/env bash
set -eu
# =========================================================
# builder 离线包安装脚本（apt/dnf 软件包模式）
#   k8s 版本 : {{.K8sVersion}}
#   镜像仓库 : {{.ImageRepository}}
#   生成时间 : {{.GeneratedAt}}
# 运行前提  : 以 root 或 sudo 执行；目标机无需外网
# =========================================================

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BUNDLE_ROOT="$(dirname "$SCRIPT_DIR")"

log() { echo "[install] $*"; }
die() { echo "[install][error] $*" >&2; exit 1; }

# ---------- 1. 检测操作系统与包管理器 ----------
detect_pkg_manager() {
  if [ -r /etc/os-release ]; then
    # shellcheck disable=SC1091
    . /etc/os-release
    case "${ID:-}" in
      ubuntu|debian) echo "apt" ;;
      rocky|centos|almalinux|fedora|openEuler) echo "dnf" ;;
      *) echo "unknown:${ID:-}" ;;
    esac
  else
    echo "unknown"
  fi
}
PKG_MANAGER="$(detect_pkg_manager)"
log "检测到包管理器: $PKG_MANAGER"
case "$PKG_MANAGER" in
  apt|dnf) ;;
  unknown:*) die "无法识别的系统（/etc/os-release ID=$PKG_MANAGER），请手动安装依赖包" ;;
esac

# ---------- 2. preflight 准备 ----------
log "preflight: 关闭并禁用 swap"
swapoff -a 2>/dev/null || true
if [ -f /etc/fstab ]; then
  sed -i '/\sswap\s/ s/^/#/' /etc/fstab 2>/dev/null || true
fi

log "preflight: 加载内核模块"
for m in overlay br_netfilter ip_vs ip_vs_rr ip_vs_wrr ip_vs_sh nf_conntrack; do
  modprobe "$m" 2>/dev/null || true
done

log "preflight: 写入内核参数"
cat > /etc/sysctl.d/99-kubernetes.conf <<'SYSCTL'
net.bridge.bridge-nf-call-iptables = 1
net.bridge.bridge-nf-call-ip6tables = 1
net.ipv4.ip_forward = 1
net.ipv6.conf.all.forwarding = 1
SYSCTL
sysctl --system >/dev/null 2>&1 || true

# ---------- 3. 安装软件包 ----------
if ls "$BUNDLE_ROOT"/packages/*.deb >/dev/null 2>&1; then
  log "安装 apt 软件包（dpkg -i，失败自动修复依赖）"
  dpkg -i "$BUNDLE_ROOT"/packages/*.deb 2>/dev/null || apt-get -f install -y
elif ls "$BUNDLE_ROOT"/packages/*.rpm >/dev/null 2>&1; then
  log "安装 rpm 软件包（rpm -ivh --nodeps）"
  rpm -ivh --nodeps "$BUNDLE_ROOT"/packages/*.rpm
else
  log "未找到离线软件包，跳过"
fi

# crictl 静态回退（cri-tools 包源不可用时由 builder 生成的兜底 tar）
CRICTL_TAR="$(ls "$BUNDLE_ROOT"/packages/runtime/crictl-*.tar.gz 2>/dev/null | head -n1 || true)"
if [ -n "$CRICTL_TAR" ]; then
  log "安装 crictl（静态回退）: $(basename "$CRICTL_TAR")"
  tar -xzf "$CRICTL_TAR" -C /tmp
  install -m 0755 /tmp/crictl /usr/local/bin/crictl
fi

# ---------- 4. 配置 containerd（systemd cgroup）并启用服务 ----------
if command -v containerd >/dev/null 2>&1; then
  mkdir -p /etc/containerd
  containerd config default > /etc/containerd/config.toml
  sed -i 's/SystemdCgroup = false/SystemdCgroup = true/' /etc/containerd/config.toml 2>/dev/null || true
  log "containerd 已配置 SystemdCgroup=true"
fi
systemctl enable --now containerd kubelet 2>/dev/null || true
log "containerd / kubelet 已启用"

# ---------- 5. 导入镜像 ----------
if [ -x "$(command -v bash)" ]; then
  log "导入镜像..."
  bash "$SCRIPT_DIR/load-images.sh" || log "警告: 镜像导入未完全成功，可稍后手动执行 load-images.sh"
else
  log "警告: 缺少 bash，跳过镜像导入"
fi

# ---------- 6. 自检与提示 ----------
log "kubeadm 自检:"
kubeadm version 2>/dev/null || true

cat <<'PROMPT'
========================================================
 安装完成，接下来请初始化集群（示例）:

   kubeadm init \\
     --kubernetes-version {{.K8sVersion}} \\
     --image-repository {{.ImageRepository}}

 如需指定 Pod 网段，可追加:
     --pod-network-cidr=10.244.0.0/16
========================================================
PROMPT
`

const loadImagesTpl = `#!/usr/bin/env bash
set -eu
# =========================================================
# 镜像导入脚本：优先 docker load，失败回退 ctr import
#   k8s 版本 : {{.K8sVersion}}
#   生成时间 : {{.GeneratedAt}}
# =========================================================

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BUNDLE_ROOT="$(dirname "$SCRIPT_DIR")"
log() { echo "[load-images] $*"; }

# ---------- 选择导入工具 ----------
LOADER=""
if command -v docker >/dev/null 2>&1 && docker info >/dev/null 2>&1; then
  LOADER="docker"
elif command -v ctr >/dev/null 2>&1; then
  LOADER="ctr"
else
  log "错误: 未找到 docker 或 ctr，无法导入镜像"
  exit 1
fi
log "使用 $LOADER 导入镜像"

# ---------- 遍历导入 ----------
count=0
failed=0
for tar in "$BUNDLE_ROOT"/images/core/*.tar "$BUNDLE_ROOT"/images/addons/*.tar; do
  [ -f "$tar" ] || continue
  name="$(basename "$tar")"
  count=$((count+1))
  if [ "$LOADER" = "docker" ]; then
    if docker load -i "$tar" >/dev/null 2>&1; then
      log "导入成功: $name"
    elif ctr -n k8s.io images import "$tar" >/dev/null 2>&1; then
      log "docker load 失败，ctr import 成功: $name"
    else
      log "警告: 导入失败 $name"
      failed=$((failed+1))
    fi
  else
    if ctr -n k8s.io images import "$tar" >/dev/null 2>&1; then
      log "导入成功: $name"
    else
      log "警告: 导入失败 $name"
      failed=$((failed+1))
    fi
  fi
done

log "镜像导入完成：共 $count 个，失败 $failed 个"
[ "$failed" -eq 0 ] || exit 1
`

// Render 渲染全部脚本文件。
func Render(d Data) ([]File, error) {
	if d.GeneratedAt == "" {
		d.GeneratedAt = time.Now().Format("2006-01-02 15:04:05")
	}
	if d.ImageRepository == "" {
		d.ImageRepository = "registry.k8s.io"
	}

	var files []File
	for _, f := range []struct {
		name string
		tpl  string
		mode os.FileMode
	}{
		{"install.sh", installTpl, 0o755},
		{"load-images.sh", loadImagesTpl, 0o755},
	} {
		t, err := template.New(f.name).Parse(f.tpl)
		if err != nil {
			return nil, fmt.Errorf("解析脚本模板 %s 失败: %w", f.name, err)
		}
		var buf bytes.Buffer
		if err := t.Execute(&buf, d); err != nil {
			return nil, fmt.Errorf("渲染脚本模板 %s 失败: %w", f.name, err)
		}
		files = append(files, File{Name: f.name, Content: buf.String(), Mode: f.mode})
	}
	return files, nil
}

// WriteDir 将渲染的脚本写入 dir 目录。
func WriteDir(dir string, d Data) ([]string, error) {
	files, err := Render(d)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(files))
	for _, f := range files {
		path := filepath.Join(dir, f.Name)
		if err := os.WriteFile(path, []byte(f.Content), f.Mode); err != nil {
			return nil, fmt.Errorf("写入脚本 %s 失败: %w", path, err)
		}
		paths = append(paths, path)
	}
	return paths, nil
}
