package serve

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestTagFromImageRef(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"registry.k8s.io/kube-apiserver:v1.27.3", "v1.27.3"},
		{"docker.io/flannel/flannel:v0.24.2", "v0.24.2"},
		{"registry.k8s.io/coredns/coredns:v1.10.1", "v1.10.1"},
		{"kube-apiserver", ""},
		{"registry.k8s.io/pause@sha256:abc", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := tagFromImageRef(c.in); got != c.want {
			t.Errorf("tagFromImageRef(%q)=%q want %q", c.in, got, c.want)
		}
	}
}

func TestSanitizeRepoName(t *testing.T) {
	if got := sanitizeRepoName("Kube_API"); got != "kube_api" {
		t.Fatalf("got %q", got)
	}
	if got := sanitizeRepoName(""); got != "image" {
		t.Fatalf("empty -> %q", got)
	}
}

func TestBuildRepomdAndPrimary(t *testing.T) {
	pkgs := []rpmPkg{{
		Name: "kubeadm", Arch: "x86_64", Epoch: "0", Version: "1.27.3", Release: "0",
		Summary: "kubeadm", Description: "desc", PkgID: strings.Repeat("a", 64),
		Href: "kubeadm-1.27.3-0.x86_64.rpm", SizePkg: 10, HdrStart: 96, HdrEnd: 200,
		Provides: []rpmEntry{{Name: "kubeadm", Flags: "EQ", Epoch: "0", Ver: "1.27.3", Rel: "0"}},
	}}
	primary := buildPrimaryXML(pkgs)
	if !bytes.Contains(primary, []byte("<name>kubeadm</name>")) {
		t.Fatalf("primary missing name: %s", primary)
	}
	filelists := buildFilelistsXML(pkgs)
	if !bytes.Contains(filelists, []byte("filelists")) {
		t.Fatal("filelists broken")
	}
	metas := []repoMetaBlob{{
		Type: "primary", Checksum: "aa", OpenChecksum: "bb",
		Location: "repodata/primary.xml.gz", Timestamp: 1, Size: 2, OpenSize: 3,
	}}
	repomd := buildRepomdXML(metas)
	if !bytes.Contains(repomd, []byte(`type="primary"`)) {
		t.Fatalf("repomd: %s", repomd)
	}
}

func TestWriteDebRepo(t *testing.T) {
	dir := t.TempDir()
	debPath := filepath.Join(dir, "kubeadm_1.27.3_amd64.deb")
	if err := writeMinimalDeb(debPath, "Package: kubeadm\nVersion: 1.27.3-00\nArchitecture: amd64\nDescription: test\n"); err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(dir, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(repo, filepath.Base(debPath))
	if err := linkOrCopy(debPath, dst); err != nil {
		t.Fatal(err)
	}
	if err := writeDebRepo(repo, []string{dst}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(repo, "Packages"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	for _, want := range []string{"Package: kubeadm", "Filename: kubeadm_1.27.3_amd64.deb", "SHA256:"} {
		if !strings.Contains(s, want) {
			t.Fatalf("Packages missing %q:\n%s", want, s)
		}
	}
	if _, err := os.Stat(filepath.Join(repo, "Packages.gz")); err != nil {
		t.Fatal(err)
	}
}

func TestCollectPackagesAndImages(t *testing.T) {
	root := t.TempDir()
	mustMk := func(p string) {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	pkg := filepath.Join(root, "packages", "a.rpm")
	mustMk(pkg)
	if err := os.WriteFile(pkg, []byte("rpm"), 0o644); err != nil {
		t.Fatal(err)
	}
	deb := filepath.Join(root, "packages", "b.deb")
	if err := os.WriteFile(deb, []byte("deb"), 0o644); err != nil {
		t.Fatal(err)
	}
	img := filepath.Join(root, "images", "core", "pause.tar")
	mustMk(img)
	if err := os.WriteFile(img, []byte("tar"), 0o644); err != nil {
		t.Fatal(err)
	}
	// manifest so collectImageTars uses it
	mf := filepath.Join(root, "manifest.yaml")
	if err := os.WriteFile(mf, []byte(`
schema_version: 1
meta: {}
images:
  - name: pause
    source_image: registry.k8s.io/pause:3.9
    tar: images/core/pause.tar
    size: 3
    sha256: ""
`), 0o644); err != nil {
		t.Fatal(err)
	}

	rpms, debs, err := collectPackages([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	if len(rpms) != 1 || len(debs) != 1 {
		t.Fatalf("rpms=%d debs=%d", len(rpms), len(debs))
	}
	tars, err := collectImageTars([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	if len(tars) != 1 || tars[0].Name != "pause" {
		t.Fatalf("tars=%+v", tars)
	}
}

func TestImportImagesShortName(t *testing.T) {
	out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", "github.com/google/go-containerregistry").Output()
	if err != nil {
		t.Skipf("go list containerregistry: %v", err)
	}
	tarPath := filepath.Join(strings.TrimSpace(string(out)), "pkg/v1/tarball/testdata/test_image_1.tar")
	if _, err := os.Stat(tarPath); err != nil {
		t.Skip(err)
	}

	root := t.TempDir()
	imgDir := filepath.Join(root, "images", "core")
	if err := os.MkdirAll(imgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(imgDir, "test-image.tar")
	if err := linkOrCopy(tarPath, dst); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "manifest.yaml"), []byte(`
schema_version: 1
meta: {}
images:
  - name: test-image
    source_image: example.com/test-image:v1
    tar: images/core/test-image.tar
    size: 1
    sha256: ""
`), 0o644); err != nil {
		t.Fatal(err)
	}

	blobDir := t.TempDir()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	srv := &http.Server{Handler: newRegistryHandler(blobDir)}
	go srv.Serve(ln)
	defer srv.Close()

	port := ln.Addr().(*net.TCPAddr).Port
	host := fmt.Sprintf("127.0.0.1:%d", port)
	ctx := context.Background()
	if err := waitHTTP(ctx, "http://"+host+"/v2/"); err != nil {
		t.Fatal(err)
	}
	refs, err := importImages(ctx, []string{root}, host, host)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 {
		t.Fatalf("refs=%v", refs)
	}
	want := host + "/test-image:v1"
	if refs[0] != want {
		t.Fatalf("got %q want %q", refs[0], want)
	}
}

func TestWriteRPMRepo(t *testing.T) {
	src := rpmTestdata(t, "simple-1.0.1-1.i386.rpm")
	dir := t.TempDir()
	dst := filepath.Join(dir, filepath.Base(src))
	if err := linkOrCopy(src, dst); err != nil {
		t.Fatal(err)
	}
	if err := writeRPMRepo(dir, []string{dst}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{
		"repodata/repomd.xml",
		"repodata/primary.xml.gz",
		"repodata/filelists.xml.gz",
		"repodata/other.xml.gz",
	} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("missing %s: %v", name, err)
		}
	}
	raw, err := os.ReadFile(filepath.Join(dir, "repodata/repomd.xml"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte(`type="primary"`)) {
		t.Fatalf("bad repomd: %s", raw)
	}
}

func rpmTestdata(t *testing.T, name string) string {
	t.Helper()
	out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", "github.com/sassoftware/go-rpmutils").Output()
	if err != nil {
		t.Skipf("go list rpmutils: %v", err)
	}
	p := filepath.Join(strings.TrimSpace(string(out)), "testdata", name)
	if _, err := os.Stat(p); err != nil {
		t.Skipf("rpm testdata missing: %v", err)
	}
	return p
}

func writeMinimalDeb(path, control string) error {
	var controlTar bytes.Buffer
	tw := tar.NewWriter(&controlTar)
	body := []byte(control)
	if err := tw.WriteHeader(&tar.Header{Name: "./control", Mode: 0o644, Size: int64(len(body))}); err != nil {
		return err
	}
	if _, err := tw.Write(body); err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	var gzBuf bytes.Buffer
	gw := gzip.NewWriter(&gzBuf)
	if _, err := gw.Write(controlTar.Bytes()); err != nil {
		return err
	}
	if err := gw.Close(); err != nil {
		return err
	}

	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.WriteString("!<arch>\n"); err != nil {
		return err
	}
	writeAR := func(name string, data []byte) error {
		hdr := fmt.Sprintf("%-16s%-12d%-6d%-6d%-8o%-10d`\n", name, 0, 0, 0, 0o644, len(data))
		if _, err := f.WriteString(hdr); err != nil {
			return err
		}
		if _, err := f.Write(data); err != nil {
			return err
		}
		if len(data)%2 == 1 {
			if _, err := f.Write([]byte{'\n'}); err != nil {
				return err
			}
		}
		return nil
	}
	if err := writeAR("debian-binary", []byte("2.0\n")); err != nil {
		return err
	}
	return writeAR("control.tar.gz", gzBuf.Bytes())
}
