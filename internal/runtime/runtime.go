// Package runtime 抽象 build 阶段的容器运行时：docker 或 containerd（ctr）。
// 默认 containerd：packages 用 ctr run；images 用 go-containerregistry 直拉并写成 docker-save。
package runtime

import (
	"fmt"
	"strings"
)

const (
	// Docker 使用宿主机 docker CLI（packages: docker run；images: pack 容器 + sock）。
	Docker = "docker"
	// Containerd 使用宿主机 ctr（packages: ctr run --net-host；images: ctr pull/export）。
	Containerd = "containerd"

	DefaultDockerBin         = "docker"
	DefaultCtrBin            = "ctr"
	DefaultDockerSock        = "/var/run/docker.sock"
	DefaultContainerdAddress = "/run/containerd/containerd.sock"
	DefaultNamespace         = "default"
)

// Normalize 规范化 runtime 名称；空值视为默认 containerd。
func Normalize(s string) (string, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return Containerd, nil
	}
	switch s {
	case Docker, Containerd:
		return s, nil
	default:
		return "", fmt.Errorf("不支持的 runtime %q（可选 containerd/docker）", s)
	}
}

// Resolve 合并显式 runtime 与测试场景：未指定时默认 containerd；
// 若注入了非默认 DockerBin（单测 fake），未显式指定时回落 docker。
func Resolve(explicit, dockerBin string) (string, error) {
	if strings.TrimSpace(explicit) != "" {
		return Normalize(explicit)
	}
	if dockerBin != "" && dockerBin != DefaultDockerBin {
		return Docker, nil
	}
	return Containerd, nil
}

// Config 运行时调用参数。
type Config struct {
	Runtime string
	// DockerBin docker 命令，默认 docker。
	DockerBin string
	// CtrBin ctr 命令，默认 ctr。
	CtrBin string
	// DockerSock 仅 docker 镜像打包容器挂载，默认 /var/run/docker.sock。
	DockerSock string
	// ContainerdAddress ctr --address，默认 /run/containerd/containerd.sock。
	ContainerdAddress string
	// Namespace ctr -n，默认 default。
	Namespace string
	// PackImage 仅 docker 模式镜像打包工具镜像。
	PackImage string
}

func (c Config) withDefaults() (Config, error) {
	rt, err := Resolve(c.Runtime, c.DockerBin)
	if err != nil {
		return c, err
	}
	c.Runtime = rt
	if c.DockerBin == "" {
		c.DockerBin = DefaultDockerBin
	}
	if c.CtrBin == "" {
		c.CtrBin = DefaultCtrBin
	}
	if c.DockerSock == "" {
		c.DockerSock = DefaultDockerSock
	}
	if c.ContainerdAddress == "" {
		c.ContainerdAddress = DefaultContainerdAddress
	}
	if c.Namespace == "" {
		c.Namespace = DefaultNamespace
	}
	return c, nil
}

// Available 检查当前 runtime 是否可用。
func Available(c Config) (bool, string) {
	cfg, err := c.withDefaults()
	if err != nil {
		return false, err.Error()
	}
	switch cfg.Runtime {
	case Docker:
		return dockerAvailable(cfg.DockerBin)
	case Containerd:
		return ctrAvailable(cfg.CtrBin, cfg.ContainerdAddress, cfg.Namespace)
	default:
		return false, fmt.Sprintf("未知 runtime %s", cfg.Runtime)
	}
}
