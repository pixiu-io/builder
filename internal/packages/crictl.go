package packages

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// FetchCrictlFallback 当容器内 cri-tools 包不可用时，回退从 GitHub release
// 下载 crictl 静态 tar 并放入 destDir/runtime 之外的 destDir 中（调用方传入
// OutDir/runtime）。
//
// 例外说明：pkgs.k8s.io 与 download.docker.com 源内通常不存在 cri-tools 包，
// 因此这是包模式的兜底方案（代码注释与 README 均已标注该例外）。
func FetchCrictlFallback(ctx context.Context, crictlVersion, arch, destDir, baseURL string) (FileInfo, error) {
	if crictlVersion == "" {
		return FileInfo{}, fmt.Errorf("crictl 版本为空")
	}
	if arch == "" {
		arch = "amd64"
	}
	if baseURL == "" {
		baseURL = "https://github.com/kubernetes-sigs/cri-tools/releases/download"
	}
	fileName := fmt.Sprintf("crictl-v%s-linux-%s.tar.gz", crictlVersion, arch)
	url := fmt.Sprintf("%s/v%s/%s", strings.TrimSuffix(baseURL, "/"), crictlVersion, fileName)

	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return FileInfo{}, fmt.Errorf("创建 crictl 目录失败: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return FileInfo{}, fmt.Errorf("构造 crictl 请求失败: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return FileInfo{}, fmt.Errorf("请求 %s 失败: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return FileInfo{}, fmt.Errorf("下载 %s 返回状态码 %d", url, resp.StatusCode)
	}

	dest := filepath.Join(destDir, fileName)
	f, err := os.Create(dest)
	if err != nil {
		return FileInfo{}, fmt.Errorf("创建 %s 失败: %w", dest, err)
	}
	defer f.Close()

	if _, err := io.Copy(f, resp.Body); err != nil {
		return FileInfo{}, fmt.Errorf("写入 %s 失败: %w", dest, err)
	}
	st, err := f.Stat()
	if err != nil {
		return FileInfo{}, fmt.Errorf("读取 %s 状态失败: %w", dest, err)
	}
	sum, err := fileSHA256(dest)
	if err != nil {
		return FileInfo{}, fmt.Errorf("计算 %s sha256 失败: %w", dest, err)
	}
	return FileInfo{
		Path:    dest,
		RelPath: filepath.ToSlash(filepath.Join("runtime", fileName)),
		Name:    fileName,
		Size:    st.Size(),
		SHA256:  sum,
	}, nil
}
