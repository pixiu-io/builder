package ghupload

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCopyWithProgress(t *testing.T) {
	var dst strings.Builder
	var progress strings.Builder
	_, err := copyWithProgress(&dst, strings.NewReader("hello"), 5, &progress)
	if err != nil {
		t.Fatal(err)
	}
	if dst.String() != "hello" {
		t.Fatalf("dst = %q", dst.String())
	}
	out := progress.String()
	if !strings.Contains(out, "100%") || !strings.Contains(out, "/s") || !strings.Contains(out, "00:00") {
		t.Fatalf("unexpected progress %q", out)
	}
}

func TestValidate(t *testing.T) {
	if err := (Options{}).Validate(); err == nil {
		t.Fatal("空 Options 应报错")
	}
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "")
	opts := Options{Owner: "acme", Repo: "builder", Tag: "v1.0.0", Token: "ghp_xxx"}
	if err := opts.Validate(); err != nil {
		t.Fatalf("合法 Options 应通过: %v", err)
	}
	if err := (Options{Owner: "acme", Repo: "builder", Tag: "v1.0.0"}).Validate(); err == nil {
		t.Fatal("缺 token 应报错")
	}
	if err := (Options{Owner: "acme", Repo: "builder", Token: "t"}).Validate(); err == nil {
		t.Fatal("缺 tag 应报错")
	}
}

func TestResolveToken(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "")
	if got := ResolveToken(" explicit "); got != "explicit" {
		t.Fatalf("explicit token: got %q", got)
	}
	t.Setenv("GITHUB_TOKEN", "from-github")
	if got := ResolveToken(""); got != "from-github" {
		t.Fatalf("GITHUB_TOKEN: got %q", got)
	}
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "from-gh")
	if got := ResolveToken(""); got != "from-gh" {
		t.Fatalf("GH_TOKEN: got %q", got)
	}
}

func TestAssetName(t *testing.T) {
	if got := AssetName("/tmp/dist/foo.tar.gz"); got != "foo.tar.gz" {
		t.Fatalf("got %q", got)
	}
}

func TestUploadFilesReplaceExistingAsset(t *testing.T) {
	var (
		deleted  bool
		uploaded string
	)
	mux := http.NewServeMux()
	apiSrv := httptest.NewServer(mux)
	t.Cleanup(apiSrv.Close)
	upSrv := httptest.NewServer(mux)
	t.Cleanup(upSrv.Close)

	mux.HandleFunc("/repos/acme/builder/releases/tags/v1.0.0", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Fatalf("method %s", r.Method)
		}
		_ = json.NewEncoder(w).Encode(release{
			ID:        42,
			UploadURL: upSrv.URL + "/repos/acme/builder/releases/42/assets{?name,label}",
			Assets:    []asset{{ID: 7, Name: "pkg.tar.gz", BrowserDownloadURL: "https://example/old"}},
		})
	})
	mux.HandleFunc("/repos/acme/builder/releases", func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("不应创建 release: %s %s", r.Method, r.URL.Path)
	})
	mux.HandleFunc("/repos/acme/builder/releases/assets/7", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Fatalf("method %s", r.Method)
		}
		deleted = true
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/repos/acme/builder/releases/42/assets", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("method %s", r.Method)
		}
		uploaded = r.URL.Query().Get("name")
		body, _ := io.ReadAll(r.Body)
		if string(body) != "hello" {
			t.Fatalf("body %q", body)
		}
		_ = json.NewEncoder(w).Encode(asset{
			ID:                 9,
			Name:               uploaded,
			BrowserDownloadURL: "https://example/pkg.tar.gz",
		})
	})

	dir := t.TempDir()
	path := filepath.Join(dir, "pkg.tar.gz")
	if err := os.WriteFile(path, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := UploadFiles(context.Background(), Options{
		Owner:       "acme",
		Repo:        "builder",
		Tag:         "v1.0.0",
		Token:       "tok",
		APIBase:     apiSrv.URL,
		UploadsBase: upSrv.URL,
		HTTPClient:  apiSrv.Client(),
	}, []string{path})
	if err != nil {
		t.Fatal(err)
	}
	if !deleted || uploaded != "pkg.tar.gz" {
		t.Fatalf("deleted=%v uploaded=%q", deleted, uploaded)
	}
	if len(res) != 1 || !strings.Contains(res[0].BrowserURL, "pkg.tar.gz") {
		t.Fatalf("result %#v", res)
	}
}

func TestDownloadAsset(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/builder/releases/tags/v1.0.0", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Fatalf("method %s", r.Method)
		}
		_ = json.NewEncoder(w).Encode(release{
			ID:     42,
			Assets: []asset{{ID: 9, Name: "kubeadm-v1.0.0-linux-amd64"}},
		})
	})
	mux.HandleFunc("/repos/acme/builder/releases/assets/9", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Fatalf("method %s", r.Method)
		}
		if got := r.Header.Get("Accept"); got != "application/octet-stream" {
			t.Fatalf("Accept = %q", got)
		}
		_, _ = w.Write([]byte("kubeadm"))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	path := filepath.Join(t.TempDir(), "kubeadm")
	err := DownloadAsset(context.Background(), Options{
		Owner:      "acme",
		Repo:       "builder",
		Tag:        "v1.0.0",
		Token:      "tok",
		APIBase:    srv.URL,
		HTTPClient: srv.Client(),
	}, "kubeadm-v1.0.0-linux-amd64", path, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "kubeadm" {
		t.Fatalf("data = %q", data)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o755 {
		t.Fatalf("mode = %v", st.Mode().Perm())
	}
}

func TestDownloadAssetMissing(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/builder/releases/tags/v1.0.0", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(release{ID: 42})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	err := DownloadAsset(context.Background(), Options{
		Owner:      "acme",
		Repo:       "builder",
		Tag:        "v1.0.0",
		Token:      "tok",
		APIBase:    srv.URL,
		HTTPClient: srv.Client(),
	}, "kubeadm-v1.0.0-linux-amd64", filepath.Join(t.TempDir(), "kubeadm"), 0o755)
	if err == nil || !strings.Contains(err.Error(), "缺少 asset") {
		t.Fatalf("expected missing asset error, got %v", err)
	}
}

func TestEnsureReleaseCreatesMissingRelease(t *testing.T) {
	var created bool
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/builder/releases/tags/v1.0.0", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Fatalf("method %s", r.Method)
		}
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
	})
	mux.HandleFunc("/repos/acme/builder/releases", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("method %s", r.Method)
		}
		created = true
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["tag_name"] != "v1.0.0" || body["name"] != "v1.0.0" {
			t.Fatalf("unexpected body %#v", body)
		}
		_ = json.NewEncoder(w).Encode(release{ID: 42})
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	err := EnsureRelease(context.Background(), Options{
		Owner:      "acme",
		Repo:       "builder",
		Tag:        "v1.0.0",
		Token:      "tok",
		APIBase:    srv.URL,
		HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("expected release creation")
	}
}

func TestListReleasesAndTags(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/builder/releases", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") != "1" {
			_ = json.NewEncoder(w).Encode([]any{})
			return
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{
				"id":       1,
				"tag_name": "v1.31.0",
				"assets":   []map[string]any{{"id": 11, "name": "kubeadm-v1.31.0-linux-amd64"}},
			},
			{
				"id":       2,
				"tag_name": "v1.32.0",
				"assets":   []any{},
			},
		})
	})
	mux.HandleFunc("/repos/kubernetes/kubernetes/tags", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") != "1" {
			_ = json.NewEncoder(w).Encode([]any{})
			return
		}
		_ = json.NewEncoder(w).Encode([]map[string]string{
			{"name": "v1.32.0"},
			{"name": "v1.31.0-rc.1"},
			{"name": "v1.31.0"},
		})
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	opts := Options{Owner: "acme", Repo: "builder", Token: "tok", APIBase: srv.URL, HTTPClient: srv.Client()}

	rels, err := ListReleases(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(rels) != 2 || !rels[0].HasAsset("kubeadm-v1.31.0-linux-amd64") || rels[1].HasAsset("x") {
		t.Fatalf("unexpected releases %#v", rels)
	}

	tags, err := ListTags(context.Background(), opts, "kubernetes", "kubernetes")
	if err != nil {
		t.Fatal(err)
	}
	if len(tags) != 3 || tags[0] != "v1.32.0" {
		t.Fatalf("unexpected tags %#v", tags)
	}
}

func TestUploadFilesMissingReleaseDoesNotCreate(t *testing.T) {
	var gotCreate bool
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/builder/releases/tags/v1.0.0", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Fatalf("method %s", r.Method)
		}
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
	})
	mux.HandleFunc("/repos/acme/builder/releases", func(w http.ResponseWriter, r *http.Request) {
		gotCreate = true
		http.Error(w, "unexpected create", http.StatusInternalServerError)
	})

	apiSrv := httptest.NewServer(mux)
	t.Cleanup(apiSrv.Close)

	dir := t.TempDir()
	path := filepath.Join(dir, "pkg.tar.gz")
	if err := os.WriteFile(path, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := UploadFiles(context.Background(), Options{
		Owner:       "acme",
		Repo:        "builder",
		Tag:         "v1.0.0",
		Token:       "tok",
		APIBase:     apiSrv.URL,
		UploadsBase: apiSrv.URL,
		HTTPClient:  apiSrv.Client(),
	}, []string{path})
	if err == nil || !strings.Contains(err.Error(), "不存在") {
		t.Fatalf("期望 release 不存在错误，实际 %v", err)
	}
	if gotCreate {
		t.Fatal("release 不存在时不应调用创建 API")
	}
}
