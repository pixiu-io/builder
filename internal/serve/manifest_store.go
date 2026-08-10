package serve

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"
)

// persistedManifest 是持久化到磁盘的 manifest 记录。
// Body 通过 encoding/json 自动 base64 编码；MediaType 用于重启后以相同类型重放。
type persistedManifest struct {
	Repo      string    `json:"repo"`
	Target    string    `json:"target"`
	MediaType string    `json:"media_type"`
	Body      []byte    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
}

// manifestStore 包裹 go-containerregistry 的 registry handler：
//   - PUT manifest 成功后持久化到磁盘（blobDir/_manifests），serve 重启后重放恢复，
//     使 docker push 的镜像在重启后仍可 pull，且 _catalog / tags/list 仍能看到。
//   - 修复 Docker 大层拉取时 open-ended（bytes=N-）/ suffix（bytes=-N）Range 触发
//     "We don't understand your Range" 的兼容问题（base registry 只支持 bytes=start-end）。
type manifestStore struct {
	blobDir string
	dir     string
	base    http.Handler
	mu      sync.Mutex
}

// persist 原子写入一条 manifest 记录。
func (m *manifestStore) persist(rec *persistedManifest) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := os.MkdirAll(m.dir, 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	tmp := filepath.Join(m.dir, fmt.Sprintf(".tmp-%d", time.Now().UnixNano()))
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, m.recordPath(rec.Repo, rec.Target))
}

// remove 删除 repo/target 对应的持久化记录。
func (m *manifestStore) remove(repo, target string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return os.Remove(m.recordPath(repo, target))
}

// recordPath 返回 repo/target 对应记录文件路径（base64 编码避免 tag/digest 特殊字符）。
func (m *manifestStore) recordPath(repo, target string) string {
	key := base64.RawURLEncoding.EncodeToString([]byte(repo + "\n" + target))
	return filepath.Join(m.dir, key+".json")
}

// loadAll 读取全部持久化记录（启动时调用）。
func (m *manifestStore) loadAll() []*persistedManifest {
	m.mu.Lock()
	defer m.mu.Unlock()
	entries, err := os.ReadDir(m.dir)
	if err != nil {
		return nil
	}
	var out []*persistedManifest
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(m.dir, e.Name()))
		if err != nil {
			continue
		}
		var rec persistedManifest
		if err := json.Unmarshal(data, &rec); err != nil {
			continue
		}
		out = append(out, &rec)
	}
	return out
}

// replayAll 启动时把持久化 manifest 重放进内存 registry。
// manifest index/list 可能依赖子 manifest 已存在，采用多轮重放：每轮重放成功即移除，
// 最多 len(records) 轮覆盖最长依赖链；最终仍失败的打印 stderr warning，不阻塞 serve 启动。
func (m *manifestStore) replayAll() {
	pending := m.loadAll()
	total := len(pending)
	for round := 0; round < total && len(pending) > 0; round++ {
		var next []*persistedManifest
		for _, rec := range pending {
			if err := m.replayOne(rec); err != nil {
				next = append(next, rec)
			}
		}
		pending = next
	}
	for _, rec := range pending {
		fmt.Fprintf(os.Stderr, "warning: 恢复 manifest %s:%s 失败，重启后该 tag/digest 拉取将失败\n", rec.Repo, rec.Target)
	}
}

// replayOne 以 HTTP PUT 方式把一条记录重放给 base registry。
func (m *manifestStore) replayOne(rec *persistedManifest) error {
	u := fmt.Sprintf("/v2/%s/manifests/%s", rec.Repo, rec.Target)
	req, err := http.NewRequest(http.MethodPut, u, bytes.NewReader(rec.Body))
	if err != nil {
		return err
	}
	if rec.MediaType != "" {
		req.Header.Set("Content-Type", rec.MediaType)
	}
	rr := httptest.NewRecorder()
	m.base.ServeHTTP(rr, req)
	if rr.Code < 200 || rr.Code >= 300 {
		return fmt.Errorf("replay %s -> %d", u, rr.Code)
	}
	return nil
}

// ServeHTTP 是对外暴露的 registry handler。
func (m *manifestStore) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if isManifestPath(req.URL.Path) {
		switch req.Method {
		case http.MethodPut:
			m.handlePut(w, req)
			return
		case http.MethodDelete:
			m.handleDelete(w, req)
			return
		}
	}
	m.handleRangeAndServe(w, req)
}

func (m *manifestStore) handlePut(w http.ResponseWriter, req *http.Request) {
	body, err := io.ReadAll(req.Body)
	req.Body.Close()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	repo, target := manifestPathParts(req.URL.Path)

	// 把 body 回填给 base，重放本次 PUT。
	req.Body = io.NopCloser(bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	rr := httptest.NewRecorder()
	m.base.ServeHTTP(rr, req)
	if rr.Code >= 200 && rr.Code < 300 {
		if err := m.persist(&persistedManifest{
			Repo:      repo,
			Target:    target,
			MediaType: req.Header.Get("Content-Type"),
			Body:      body,
			CreatedAt: time.Now(),
		}); err != nil {
			fmt.Fprintf(os.Stderr, "warning: 持久化 manifest %s:%s 失败（重启后将丢失）: %v\n", repo, target, err)
		}
	}
	copyResponse(w, rr)
}

func (m *manifestStore) handleDelete(w http.ResponseWriter, req *http.Request) {
	repo, target := manifestPathParts(req.URL.Path)
	rr := httptest.NewRecorder()
	m.base.ServeHTTP(rr, req)
	if rr.Code >= 200 && rr.Code < 300 {
		if err := m.remove(repo, target); err != nil && !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "warning: 删除持久化 manifest %s:%s 失败: %v\n", repo, target, err)
		}
	}
	copyResponse(w, rr)
}

// handleRangeAndServe 修复 blob GET 的 open-ended/suffix Range，然后交给 base。
func (m *manifestStore) handleRangeAndServe(w http.ResponseWriter, req *http.Request) {
	if req.Method == http.MethodGet && req.Header.Get("Range") != "" {
		if newRange, ok := m.rewriteBlobRange(req); ok {
			req.Header.Set("Range", newRange)
		}
	}
	m.base.ServeHTTP(w, req)
}

// rewriteBlobRange 根据磁盘 blob 大小把 open-ended（bytes=N-）和 suffix（bytes=-N）
// Range 改写为 base 支持的 bytes=start-end。解析失败 / 文件不存在时原样返回（交给 base）。
func (m *manifestStore) rewriteBlobRange(req *http.Request) (string, bool) {
	elems := strings.Split(req.URL.Path, "/")
	elems = elems[1:]
	if len(elems) < 3 || elems[len(elems)-2] != "blobs" {
		return "", false
	}
	digest := elems[len(elems)-1]
	h, err := v1.NewHash(digest)
	if err != nil {
		return "", false
	}
	size, err := m.blobSize(h)
	if err != nil {
		return "", false
	}
	return rewriteRangeHeader(req.Header.Get("Range"), size)
}

func (m *manifestStore) blobSize(h v1.Hash) (int64, error) {
	fi, err := os.Stat(filepath.Join(m.blobDir, h.Algorithm, h.Hex))
	if err != nil {
		return 0, err
	}
	return fi.Size(), nil
}

// rewriteRangeHeader 将 range 头改写为 base 支持的格式。返回 ok=false 表示无需 / 无法改写，
// 原样交给 base（base 对不支持的 range 返回 416）。
func rewriteRangeHeader(rangeHeader string, size int64) (string, bool) {
	if size <= 0 || rangeHeader == "" || !strings.HasPrefix(rangeHeader, "bytes=") {
		return rangeHeader, false
	}
	spec := strings.TrimPrefix(rangeHeader, "bytes=")
	if strings.Contains(spec, ",") {
		return rangeHeader, false
	}
	dash := strings.Index(spec, "-")
	if dash < 0 {
		return rangeHeader, false
	}
	startStr, endStr := spec[:dash], spec[dash+1:]

	if startStr == "" {
		// suffix range: bytes=-N
		if endStr == "" {
			return rangeHeader, false
		}
		n, err := strconv.ParseInt(endStr, 10, 64)
		if err != nil || n <= 0 {
			return rangeHeader, false
		}
		start := size - n
		if start < 0 {
			start = 0
		}
		return fmt.Sprintf("bytes=%d-%d", start, size-1), true
	}

	start, err := strconv.ParseInt(startStr, 10, 64)
	if err != nil {
		return rangeHeader, false
	}
	if endStr == "" {
		// open-ended range: bytes=N-
		if start >= size {
			return rangeHeader, false
		}
		return fmt.Sprintf("bytes=%d-%d", start, size-1), true
	}
	// closed range bytes=N-M：base 原生支持，原样放行。
	return rangeHeader, false
}

// isManifestPath 判断路径是否为 /v2/<repo>/manifests/<target>（与 base registry 一致）。
func isManifestPath(p string) bool {
	elems := strings.Split(p, "/")
	elems = elems[1:]
	if len(elems) < 4 {
		return false
	}
	return elems[len(elems)-2] == "manifests"
}

// manifestPathParts 从 /v2/<repo>/manifests/<target> 提取 repo 与 target。
func manifestPathParts(p string) (repo, target string) {
	elems := strings.Split(p, "/")
	elems = elems[1:]
	target = elems[len(elems)-1]
	repo = strings.Join(elems[1:len(elems)-2], "/")
	return
}

// copyResponse 把 recorder 的结果完整拷回真实 ResponseWriter。
func copyResponse(w http.ResponseWriter, rr *httptest.ResponseRecorder) {
	for k, vv := range rr.Header() {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(rr.Code)
	_, _ = w.Write(rr.Body.Bytes())
}
