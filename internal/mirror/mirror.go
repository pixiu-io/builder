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
	Official: "官方源，完整支持",
	Aliyun:   "预留：镜像阶段走 aliyun 镜像仓库；软件包源仍为官方",
	Tencent:  "预留：镜像阶段走 tencent 镜像仓库；软件包源仍为官方",
}

// ParseMirror 解析镜像源参数。
func ParseMirror(s string) (Mirror, error) {
	m := Mirror(strings.ToLower(strings.TrimSpace(s)))
	if _, ok := notes[m]; !ok {
		return "", fmt.Errorf("不支持的镜像源 %q（可选：official/aliyun/tencent）", s)
	}
	return m, nil
}

// IsSupported 判断镜像源是否已完整实现。
// 当前仅 official 完整支持。
func (m Mirror) IsSupported() bool {
	return m == Official
}

// ImageRepository 返回镜像阶段使用的镜像仓库。
// 仅 official 为可用的官方仓库；aliyun/tencent 为预留映射（未实测）。
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
