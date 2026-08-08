// Package ghupload 将离线包产物上传到 GitHub Release。
package ghupload

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	apiBase     = "https://api.github.com"
	uploadsBase = "https://uploads.github.com"
)

// Options GitHub Release 上传参数（可由 builder.yaml github 节 + CLI flag 合并）。
type Options struct {
	// Owner 仓库所有者（用户或组织）。
	Owner string
	// Repo 仓库名。
	Repo string
	// Tag Release 对应的 git tag（如 v1.27.3）。
	Tag string
	// Token GitHub PAT 或 GITHUB_TOKEN；为空时尝试环境变量 GITHUB_TOKEN / GH_TOKEN。
	Token string
	// APIBase 可选，覆盖 api.github.com（测试用）。
	APIBase string
	// UploadsBase 可选，覆盖 uploads.github.com（测试用）。
	UploadsBase string
	// HTTPClient 可选；为空时使用带超时的默认客户端。
	HTTPClient *http.Client
	// Progress 可选；非空时下载 asset 会输出实时进度。
	Progress io.Writer
}

// Result 单个文件上传结果。
type Result struct {
	LocalPath  string
	Owner      string
	Repo       string
	Tag        string
	AssetName  string
	BrowserURL string
}

type release struct {
	ID        int64   `json:"id"`
	UploadURL string  `json:"upload_url"`
	Assets    []asset `json:"assets"`
}

type asset struct {
	ID                 int64  `json:"id"`
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

// Validate 校验上传必要参数。
func (o Options) Validate() error {
	if strings.TrimSpace(o.Owner) == "" {
		return fmt.Errorf("github owner 不能为空（配置 github.owner 或 --github-owner）")
	}
	if strings.TrimSpace(o.Repo) == "" {
		return fmt.Errorf("github repo 不能为空（配置 github.repo 或 --github-repo）")
	}
	if strings.TrimSpace(o.Tag) == "" {
		return fmt.Errorf("github tag 不能为空（配置 github.tag 或 --github-tag）")
	}
	if strings.TrimSpace(ResolveToken(o.Token)) == "" {
		return fmt.Errorf("github token 不能为空（配置 github.token、--github-token 或环境变量 GITHUB_TOKEN/GH_TOKEN）")
	}
	return nil
}

// ResolveToken 返回非空 token：显式值优先，否则读 GITHUB_TOKEN / GH_TOKEN。
func ResolveToken(explicit string) string {
	if t := strings.TrimSpace(explicit); t != "" {
		return t
	}
	if t := strings.TrimSpace(os.Getenv("GITHUB_TOKEN")); t != "" {
		return t
	}
	return strings.TrimSpace(os.Getenv("GH_TOKEN"))
}

// AssetName 使用本地文件 basename 作为 Release asset 名。
func AssetName(localPath string) string {
	return filepath.Base(localPath)
}

// DownloadAsset 从指定 tag 的 GitHub Release 下载指定 asset 到本地文件。
func DownloadAsset(ctx context.Context, opts Options, assetName, dst string, mode os.FileMode) error {
	if err := opts.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(assetName) == "" {
		return fmt.Errorf("github release asset 名不能为空")
	}
	if strings.TrimSpace(dst) == "" {
		return fmt.Errorf("github release asset 下载目标路径不能为空")
	}
	c := newClient(opts)
	rel, err := c.getRelease(ctx)
	if err != nil {
		return err
	}
	var found *asset
	for i := range rel.Assets {
		if rel.Assets[i].Name == assetName {
			found = &rel.Assets[i]
			break
		}
	}
	if found == nil {
		return fmt.Errorf("github release %s/%s@%s 缺少 asset %q，请先执行 upload-kubeadm 上传", opts.Owner, opts.Repo, opts.Tag, assetName)
	}
	return c.downloadAsset(ctx, found.ID, dst, mode)
}

// EnsureRelease 确保指定 tag 的 GitHub Release 存在；不存在时自动创建。
func EnsureRelease(ctx context.Context, opts Options) error {
	if err := opts.Validate(); err != nil {
		return err
	}
	c := newClient(opts)
	_, err := c.getOrCreateRelease(ctx)
	return err
}

// UploadFiles 将本地文件上传到指定 tag 的 GitHub Release。
// Release 不存在时直接报错；同名 asset 已存在时先删除再上传。
func UploadFiles(ctx context.Context, opts Options, files []string) ([]Result, error) {
	if err := opts.Validate(); err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("没有可上传的文件")
	}
	for _, f := range files {
		if st, err := os.Stat(f); err != nil {
			return nil, fmt.Errorf("本地文件不可用 %s: %w", f, err)
		} else if st.IsDir() {
			return nil, fmt.Errorf("不能上传目录: %s", f)
		}
	}

	c := newClient(opts)
	rel, err := c.getRelease(ctx)
	if err != nil {
		return nil, err
	}

	var out []Result
	for _, f := range files {
		name := AssetName(f)
		if err := c.deleteAssetByName(ctx, rel, name); err != nil {
			return out, fmt.Errorf("删除已有 asset %s 失败: %w", name, err)
		}
		a, err := c.uploadAsset(ctx, rel, f, name)
		if err != nil {
			return out, fmt.Errorf("上传 %s → %s/%s@%s/%s 失败: %w", f, opts.Owner, opts.Repo, opts.Tag, name, err)
		}
		out = append(out, Result{
			LocalPath:  f,
			Owner:      opts.Owner,
			Repo:       opts.Repo,
			Tag:        opts.Tag,
			AssetName:  name,
			BrowserURL: a.BrowserDownloadURL,
		})
	}
	return out, nil
}

type client struct {
	opts   Options
	token  string
	api    string
	upload string
	http   *http.Client
}

func newClient(opts Options) *client {
	api := opts.APIBase
	if api == "" {
		api = apiBase
	}
	up := opts.UploadsBase
	if up == "" {
		up = uploadsBase
	}
	hc := opts.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Minute}
	}
	return &client{
		opts:   opts,
		token:  ResolveToken(opts.Token),
		api:    strings.TrimRight(api, "/"),
		upload: strings.TrimRight(up, "/"),
		http:   hc,
	}
}

func (c *client) getRelease(ctx context.Context) (*release, error) {
	path := fmt.Sprintf("/repos/%s/%s/releases/tags/%s", c.opts.Owner, c.opts.Repo, url.PathEscape(c.opts.Tag))
	var rel release
	status, err := c.doJSON(ctx, http.MethodGet, c.api+path, nil, "", &rel)
	if err == nil {
		return &rel, nil
	}
	if status == http.StatusNotFound {
		return nil, fmt.Errorf("release %s/%s@%s 不存在，请先在 GitHub 创建对应 Release: %w", c.opts.Owner, c.opts.Repo, c.opts.Tag, err)
	}
	return nil, fmt.Errorf("查询 release %s/%s@%s 失败: %w", c.opts.Owner, c.opts.Repo, c.opts.Tag, err)
}

func (c *client) getOrCreateRelease(ctx context.Context) (*release, error) {
	path := fmt.Sprintf("/repos/%s/%s/releases/tags/%s", c.opts.Owner, c.opts.Repo, url.PathEscape(c.opts.Tag))
	var rel release
	status, err := c.doJSON(ctx, http.MethodGet, c.api+path, nil, "", &rel)
	if err == nil {
		return &rel, nil
	}
	if status != http.StatusNotFound {
		return nil, fmt.Errorf("查询 release %s/%s@%s 失败: %w", c.opts.Owner, c.opts.Repo, c.opts.Tag, err)
	}

	body := map[string]any{
		"tag_name": c.opts.Tag,
		"name":     c.opts.Tag,
	}
	var created release
	_, err = c.doJSON(ctx, http.MethodPost, c.api+fmt.Sprintf("/repos/%s/%s/releases", c.opts.Owner, c.opts.Repo), body, "application/json", &created)
	if err != nil {
		return nil, fmt.Errorf("创建 release %s/%s@%s 失败: %w", c.opts.Owner, c.opts.Repo, c.opts.Tag, err)
	}
	return &created, nil
}

func (c *client) deleteAssetByName(ctx context.Context, rel *release, name string) error {
	for _, a := range rel.Assets {
		if a.Name != name {
			continue
		}
		url := fmt.Sprintf("%s/repos/%s/%s/releases/assets/%d", c.api, c.opts.Owner, c.opts.Repo, a.ID)
		_, err := c.doJSON(ctx, http.MethodDelete, url, nil, "", nil)
		return err
	}
	return nil
}

func (c *client) downloadAsset(ctx context.Context, assetID int64, dst string, mode os.FileMode) error {
	u := fmt.Sprintf("%s/repos/%s/%s/releases/assets/%d", c.api, c.opts.Owner, c.opts.Repo, assetID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/octet-stream")
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("下载 GitHub release asset 失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		msg := strings.TrimSpace(string(data))
		if msg == "" {
			msg = resp.Status
		}
		if resp.StatusCode == http.StatusNotFound {
			msg = msg + "；GitHub asset 下载接口返回 404，常见原因是 token 对该仓库没有 contents:read 权限、fine-grained token 未授权到此仓库，或 asset 不存在"
		}
		return fmt.Errorf("下载 GitHub release asset 失败: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(msg))
	}

	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return fmt.Errorf("创建 GitHub release asset 下载文件失败 %s: %w", tmp, err)
	}
	_, copyErr := copyWithProgress(out, resp.Body, resp.ContentLength, c.opts.Progress)
	closeErr := out.Close()
	if copyErr != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("写入 GitHub release asset 下载文件失败 %s: %w", tmp, copyErr)
	}
	if closeErr != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("关闭 GitHub release asset 下载文件失败 %s: %w", tmp, closeErr)
	}
	if err := os.Chmod(tmp, mode); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("设置 GitHub release asset 文件权限失败 %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("保存 GitHub release asset 下载文件失败 %s: %w", dst, err)
	}
	return nil
}

func (c *client) uploadAssetURL(rel *release, name string) (string, error) {
	raw := strings.TrimSpace(rel.UploadURL)
	if raw == "" {
		raw = fmt.Sprintf("%s/repos/%s/%s/releases/%d/assets", c.upload, c.opts.Owner, c.opts.Repo, rel.ID)
	}
	if i := strings.Index(raw, "{"); i >= 0 {
		raw = raw[:i]
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("解析 release upload_url 失败: %w", err)
	}
	q := u.Query()
	q.Set("name", name)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func (c *client) uploadAsset(ctx context.Context, rel *release, localPath, name string) (*asset, error) {
	f, err := os.Open(localPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		return nil, err
	}

	u, err := c.uploadAssetURL(rel, name)
	if err != nil {
		return nil, err
	}
	// 用纯 Reader 包装，避免 http.Transport 对 *os.File 走 sendfile（部分环境会失败）。
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, struct{ io.Reader }{f})
	if err != nil {
		return nil, err
	}
	req.ContentLength = st.Size()
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg := strings.TrimSpace(string(data))
		if resp.StatusCode == http.StatusNotFound {
			msg = msg + "；GitHub 上传接口返回 404，常见原因是 token 对该仓库没有 contents:write 权限、fine-grained token 未授权到此仓库、仓库 owner/repo 不匹配，或 Release 不可写"
		}
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(msg))
	}
	var a asset
	if err := json.Unmarshal(data, &a); err != nil {
		return nil, fmt.Errorf("解析上传响应失败: %w", err)
	}
	return &a, nil
}

func copyWithProgress(dst io.Writer, src io.Reader, total int64, progress io.Writer) (int64, error) {
	if progress == nil {
		return io.Copy(dst, src)
	}
	pr := &progressReader{r: src, total: total, out: progress, start: time.Now(), last: time.Now()}
	n, err := io.Copy(dst, pr)
	pr.print(true)
	fmt.Fprintln(progress)
	return n, err
}

type progressReader struct {
	r       io.Reader
	total   int64
	out     io.Writer
	start   time.Time
	last    time.Time
	current int64
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	if n > 0 {
		p.current += int64(n)
		now := time.Now()
		if now.Sub(p.last) >= 500*time.Millisecond {
			p.last = now
			p.print(false)
		}
	}
	return n, err
}

func (p *progressReader) print(final bool) {
	elapsed := time.Since(p.start)
	if elapsed <= 0 {
		elapsed = time.Second
	}
	speed := float64(p.current) / elapsed.Seconds()
	percent := "--%"
	eta := "--:--"
	if p.total > 0 {
		pct := float64(p.current) * 100 / float64(p.total)
		if pct > 100 {
			pct = 100
		}
		percent = fmt.Sprintf("%3.0f%%", pct)
		if speed > 0 && p.current < p.total {
			eta = formatDuration(time.Duration(float64(p.total-p.current) / speed * float64(time.Second)))
		} else {
			eta = "00:00"
		}
	}
	if final {
		eta = "00:00"
	}
	fmt.Fprintf(p.out, "\r%s %s %s/s %s", percent, formatBytes(p.current), formatBytes(int64(speed)), eta)
}

func formatBytes(n int64) string {
	units := []string{"B", "KB", "MB", "GB"}
	v := float64(n)
	i := 0
	for v >= 1024 && i < len(units)-1 {
		v /= 1024
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%d%s", n, units[i])
	}
	return fmt.Sprintf("%.1f%s", v, units[i])
}

func formatDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	seconds := int(d.Seconds() + 0.5)
	minutes := seconds / 60
	seconds = seconds % 60
	return fmt.Sprintf("%02d:%02d", minutes, seconds)
}

func (c *client) doJSON(ctx context.Context, method, rawURL string, body any, contentType string, out any) (int, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, rdr)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg := strings.TrimSpace(string(data))
		if msg == "" {
			msg = resp.Status
		}
		return resp.StatusCode, fmt.Errorf("HTTP %d: %s", resp.StatusCode, msg)
	}
	if out != nil && len(data) > 0 && resp.StatusCode != http.StatusNoContent {
		if err := json.Unmarshal(data, out); err != nil {
			return resp.StatusCode, fmt.Errorf("解析响应失败: %w", err)
		}
	}
	return resp.StatusCode, nil
}
