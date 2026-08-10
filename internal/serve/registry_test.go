package serve

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// TestRegistryPushPersistsManifestAndRestoresAfterRestart 验证：
//  1. 用户 push 的镜像在存活期间可拉取；
//  2. 以同一 blobDir 重建 handler（模拟 serve 重启）后，manifest / tag 仍然可用，
//     _catalog 与 tags/list 能看到对应 repo/tag。
func TestRegistryPushPersistsManifestAndRestoresAfterRestart(t *testing.T) {
	blobDir := t.TempDir()
	img, err := random.Image(1024, 1)
	if err != nil {
		t.Fatal(err)
	}

	// 第一次启动：push 一个镜像。
	ts := httptest.NewServer(newRegistryHandler(blobDir))
	host := strings.TrimPrefix(ts.URL, "http://")
	ref, err := name.ParseReference(host+"/testrepo:testtag", name.Insecure)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(ref, img); err != nil {
		t.Fatal(err)
	}
	assertManifest(t, ts.URL, "testrepo", "testtag")
	assertTags(t, ts.URL, "testrepo", "testtag")
	ts.Close()

	// 重启：同一 blobDir 重新构造 handler。
	ts2 := httptest.NewServer(newRegistryHandler(blobDir))
	defer ts2.Close()
	assertManifest(t, ts2.URL, "testrepo", "testtag")
	assertTags(t, ts2.URL, "testrepo", "testtag")
	assertCatalog(t, ts2.URL, "testrepo")

	// 持久化记录已落盘。
	files, err := filepath.Glob(filepath.Join(blobDir, "_manifests", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no persisted manifest files")
	}
}

// TestRegistryManifestListPersistsAndRestores 验证 manifest list（多架构 index）在
// 重启后能恢复：index 依赖其子 manifest 已就绪，多轮 replay 必须先把子 manifest 恢复。
func TestRegistryManifestListPersistsAndRestores(t *testing.T) {
	blobDir := t.TempDir()
	idx, err := random.Index(1024, 1, 2)
	if err != nil {
		t.Fatal(err)
	}

	ts := httptest.NewServer(newRegistryHandler(blobDir))
	host := strings.TrimPrefix(ts.URL, "http://")
	ref, err := name.ParseReference(host+"/multirepo:multi", name.Insecure)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.WriteIndex(ref, idx); err != nil {
		t.Fatal(err)
	}
	ts.Close()

	// 重启后 index 与 tag 都应可用。
	ts2 := httptest.NewServer(newRegistryHandler(blobDir))
	defer ts2.Close()
	assertManifest(t, ts2.URL, "multirepo", "multi")
	assertTags(t, ts2.URL, "multirepo", "multi")
	assertCatalog(t, ts2.URL, "multirepo")
}

// TestRegistryOpenEndedRange 验证 Docker 大层拉取时 open-ended（bytes=N-）与
// suffix（bytes=-N）Range 都能正常返回 206 + 正确 Content-Range/body，
// 不再报 "We don't understand your Range"。
func TestRegistryOpenEndedRange(t *testing.T) {
	blobDir := t.TempDir()
	content := []byte("hello world") // 11 字节
	sum := sha256.Sum256(content)
	hexDigest := hex.EncodeToString(sum[:])
	algDir := filepath.Join(blobDir, "sha256")
	if err := os.MkdirAll(algDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(algDir, hexDigest), content, 0o644); err != nil {
		t.Fatal(err)
	}

	ts := httptest.NewServer(newRegistryHandler(blobDir))
	defer ts.Close()
	blobURL := ts.URL + "/v2/testrepo/blobs/sha256:" + hexDigest

	// open-ended range: bytes=2-
	resp := getWithRange(t, blobURL, "bytes=2-")
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("bytes=2- status=%d body=%q", resp.StatusCode, body)
	}
	if got := resp.Header.Get("Content-Range"); got != "bytes 2-10/11" {
		t.Fatalf("Content-Range=%q want %q", got, "bytes 2-10/11")
	}
	if string(body) != "llo world" {
		t.Fatalf("bytes=2- body=%q", body)
	}

	// suffix range: bytes=-3
	resp2 := getWithRange(t, blobURL, "bytes=-3")
	body2, err := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if resp2.StatusCode != http.StatusPartialContent {
		t.Fatalf("bytes=-3 status=%d body=%q", resp2.StatusCode, body2)
	}
	if got := resp2.Header.Get("Content-Range"); got != "bytes 8-10/11" {
		t.Fatalf("Content-Range=%q want %q", got, "bytes 8-10/11")
	}
	if string(body2) != "rld" {
		t.Fatalf("bytes=-3 body=%q", body2)
	}
}

func getWithRange(t *testing.T, url, rng string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Range", rng)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func assertManifest(t *testing.T, baseURL, repo, target string) {
	t.Helper()
	resp, err := http.Get(baseURL + "/v2/" + repo + "/manifests/" + target)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET manifest %s/%s: status %d", repo, target, resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct == "" {
		t.Fatalf("GET manifest %s/%s: empty Content-Type", repo, target)
	}
}

func assertTags(t *testing.T, baseURL, repo string, wantTags ...string) {
	t.Helper()
	resp, err := http.Get(baseURL + "/v2/" + repo + "/tags/list")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("tags/list: status %d", resp.StatusCode)
	}
	var l struct {
		Name string   `json:"name"`
		Tags []string `json:"tags"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&l); err != nil {
		t.Fatal(err)
	}
	for _, w := range wantTags {
		if !contains(l.Tags, w) {
			t.Fatalf("tags/list %v missing %q", l.Tags, w)
		}
	}
}

func assertCatalog(t *testing.T, baseURL string, wantRepos ...string) {
	t.Helper()
	resp, err := http.Get(baseURL + "/v2/_catalog")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("_catalog: status %d", resp.StatusCode)
	}
	var c struct {
		Repos []string `json:"repositories"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&c); err != nil {
		t.Fatal(err)
	}
	for _, w := range wantRepos {
		if !contains(c.Repos, w) {
			t.Fatalf("_catalog %v missing %q", c.Repos, w)
		}
	}
}

func contains(items []string, want string) bool {
	for _, it := range items {
		if it == want {
			return true
		}
	}
	return false
}
