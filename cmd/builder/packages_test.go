package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestParseDebPackagesIndex(t *testing.T) {
	content := `Package: kubeadm
Version: 1.27.3-1.1
Architecture: amd64
Size: 10240

Package: chrony
Version: 4.5-1
Architecture: amd64
Size: 2048
`
	rows, err := parseDebPackagesIndex(content)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows", len(rows))
	}
	if rows[0].Type != "deb" || rows[0].Name != "kubeadm" || rows[0].Version != "1.27.3-1.1" {
		t.Fatalf("row0=%+v", rows[0])
	}
}

func TestParseRPMPrimaryXMLGzip(t *testing.T) {
	xmlBody := `<?xml version="1.0" encoding="UTF-8"?>
<metadata xmlns="http://linux.duke.edu/metadata/common" packages="1">
  <package type="rpm">
    <name>kubelet</name>
    <arch>x86_64</arch>
    <version epoch="0" ver="1.27.3" rel="0"/>
    <size package="4096" installed="0" archive="0"/>
  </package>
</metadata>
`
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	if _, err := gw.Write([]byte(xmlBody)); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	rows, err := parseRPMPrimaryXMLGzip(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d", len(rows))
	}
	if rows[0].Type != "rpm" || rows[0].Name != "kubelet" || rows[0].Version != "1.27.3-0" {
		t.Fatalf("row=%+v", rows[0])
	}
	if rows[0].Size != 4096 {
		t.Fatalf("size=%d", rows[0].Size)
	}
}

func TestRunPackages(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/deb/Packages", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `Package: kubeadm
Version: 1.31.6-1.1
Architecture: amd64
Size: 1048576
`)
	})
	mux.HandleFunc("/rpm/repodata/primary.xml.gz", func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	err := runPackages(context.Background(), strings.TrimPrefix(srv.URL, "http://"), 50, "all")
	w.Close()
	os.Stdout = old
	out, _ := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if !strings.Contains(s, "TYPE") || !strings.Contains(s, "kubeadm") || !strings.Contains(s, "deb") {
		t.Fatalf("输出异常: %s", s)
	}
	if !strings.Contains(s, "1.0MB") {
		t.Fatalf("SIZE 格式异常: %s", s)
	}
}

func TestRunPackagesLimit(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/deb/Packages", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `Package: a
Version: 1
Architecture: amd64
Size: 1

Package: b
Version: 1
Architecture: amd64
Size: 1

Package: c
Version: 1
Architecture: amd64
Size: 1
`)
	})
	mux.HandleFunc("/rpm/repodata/primary.xml.gz", func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	oldOut, oldErr := os.Stdout, os.Stderr
	r, w, _ := os.Pipe()
	er, ew, _ := os.Pipe()
	os.Stdout, os.Stderr = w, ew
	err := runPackages(context.Background(), srv.URL, 2, "deb")
	w.Close()
	ew.Close()
	os.Stdout, os.Stderr = oldOut, oldErr
	out, _ := io.ReadAll(r)
	_, _ = io.ReadAll(er)
	if err != nil {
		t.Fatal(err)
	}
	// 表头 + 2 行数据
	lines := 0
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if strings.TrimSpace(line) != "" {
			lines++
		}
	}
	if lines != 3 {
		t.Fatalf("limit=2 应输出表头+2行，got %d\n%s", lines, out)
	}
}

func TestFormatByteSize(t *testing.T) {
	cases := map[int64]string{
		0:        "-",
		500:      "500B",
		2048:     "2.0KB",
		1048576:  "1.0MB",
		1073741824: "1.0GB",
	}
	for n, want := range cases {
		if got := formatByteSize(n); got != want {
			t.Errorf("formatByteSize(%d)=%q want %q", n, got, want)
		}
	}
}

func TestNormalizeRepoBaseURL(t *testing.T) {
	if got := normalizeRepoBaseURL("192.168.1.10:8080"); got != "http://192.168.1.10:8080" {
		t.Fatalf("got %q", got)
	}
	if got := normalizeRepoBaseURL("http://x:8080/"); got != "http://x:8080" {
		t.Fatalf("got %q", got)
	}
}
