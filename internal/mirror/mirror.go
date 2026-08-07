// Package mirror 定义镜像仓库源（Mirror）。
// 包模式下 k8s 组件与运行时均走官方包源（pkgs.k8s.io / download.docker.com），
// 因此 Mirror 仅作用于"镜像阶段"的镜像仓库（kubeadm config images list --image-repository）。
package mirror

import (
	"fmt"
	"strings"
)

// Mirror 镜像仓库源。
type Mirror string

// 支持的镜像源。
const (
	Official Mirror = "official"
	Aliyun   Mirror = "aliyun"
	Tencent  Mirror = "tencent"
)

// notes 镜像源说明。
var notes = map[Mirror]string{
	Official: "官方源 registry.k8s.io",
	Aliyun:   "阿里云 registry.aliyuncs.com/google_containers",
	Tencent:  "腾讯 mirror.cc.tencentyun.com/kubernetes",
}

// ParseMirror 解析镜像源参数。
func ParseMirror(s string) (Mirror, error) {
	m := Mirror(strings.ToLower(strings.TrimSpace(s)))
	if _, ok := notes[m]; !ok {
		return "", fmt.Errorf("不支持的镜像源 %q（可选：official/aliyun/tencent）", s)
	}
	return m, nil
}

// IsSupported 判断镜像源是否可用。
// official / aliyun / tencent 均可用：镜像阶段获取 k8s 镜像清单与拉取镜像
// 时使用对应镜像仓库地址（ImageRepository），生成的镜像引用/清单均带该地址。
func (m Mirror) IsSupported() bool {
	switch m {
	case Official, Aliyun, Tencent:
		return true
	default:
		return false
	}
}

// ImageRepository 返回镜像阶段使用的镜像仓库。
// 核心镜像清单（kubeadm config images list --image-repository）与拉取/保存的
// 镜像引用均以此地址为仓库前缀。
func (m Mirror) ImageRepository() string {
	switch m {
	case Aliyun:
		return "registry.aliyuncs.com/google_containers"
	case Tencent:
		return "mirror.cc.tencentyun.com/kubernetes"
	default:
		return "registry.k8s.io"
	}
}

// Note 返回镜像源说明。
func (m Mirror) Note() string { return notes[m] }

// String 实现 Stringer。
func (m Mirror) String() string { return string(m) }
