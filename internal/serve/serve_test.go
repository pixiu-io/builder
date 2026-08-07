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
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"
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

// writeMinimalDebXz 构造 control.tar.xz 的 .deb（模拟 Debian 12/Ubuntu 24.04 新 dpkg 产物）。
func writeMinimalDebXz(path, control string) error {
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
	var xzBuf bytes.Buffer
	xw, err := xz.NewWriter(&xzBuf)
	if err != nil {
		return err
	}
	if _, err := xw.Write(controlTar.Bytes()); err != nil {
		return err
	}
	if err := xw.Close(); err != nil {
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
	return writeAR("control.tar.xz", xzBuf.Bytes())
}

// writeMinimalDebZst 构造 control.tar.zst 的 .deb（模拟 Ubuntu 23.10+ / Debian 12+ 新 dpkg 产物）。
func writeMinimalDebZst(path, control string) error {
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
	var zstBuf bytes.Buffer
	zw, err := zstd.NewWriter(&zstBuf)
	if err != nil {
		return err
	}
	if _, err := zw.Write(controlTar.Bytes()); err != nil {
		return err
	}
	if err := zw.Close(); err != nil {
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
	return writeAR("control.tar.zst", zstBuf.Bytes())
}

// TestReadDebControlXz 验证 control.tar.xz 的 deb 能正常解析（现代 dpkg 产物）。
func TestReadDebControlXz(t *testing.T) {
	dir := t.TempDir()
	debPath := filepath.Join(dir, "kubeadm_1.27.3_amd64.deb")
	control := "Package: kubeadm\nVersion: 1.27.3-00\nArchitecture: amd64\nDescription: test\n"
	if err := writeMinimalDebXz(debPath, control); err != nil {
		t.Fatal(err)
	}
	got, err := readDebControl(debPath)
	if err != nil {
		t.Fatalf("readDebControl(xz deb): %v", err)
	}
	if !strings.Contains(got, "Package: kubeadm") {
		t.Fatalf("control 内容异常: %s", got)
	}
	// 端到端：writeDebRepo 应成功生成 Packages
	repo := filepath.Join(dir, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeDebRepo(repo, []string{debPath}); err != nil {
		t.Fatalf("writeDebRepo(xz deb): %v", err)
	}
	data, err := os.ReadFile(filepath.Join(repo, "Packages"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "Package: kubeadm") {
		t.Fatalf("Packages 缺少 kubeadm:\n%s", data)
	}
}

// TestReadDebControlZst 验证 control.tar.zst 的 deb（Ubuntu 24.04 等新 dpkg 产物）能正常解析。
func TestReadDebControlZst(t *testing.T) {
	dir := t.TempDir()
	debPath := filepath.Join(dir, "chrony_4.5-1ubuntu4.2_amd64.deb")
	control := "Package: chrony\nVersion: 4.5-1ubuntu4.2\nArchitecture: amd64\nDescription: test\n"
	if err := writeMinimalDebZst(debPath, control); err != nil {
		t.Fatal(err)
	}
	got, err := readDebControl(debPath)
	if err != nil {
		t.Fatalf("readDebControl(zst deb): %v", err)
	}
	if !strings.Contains(got, "Package: chrony") {
		t.Fatalf("control 内容异常: %s", got)
	}
	// 端到端：writeDebRepo 应成功生成 Packages
	repo := filepath.Join(dir, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeDebRepo(repo, []string{debPath}); err != nil {
		t.Fatalf("writeDebRepo(zst deb): %v", err)
	}
	data, err := os.ReadFile(filepath.Join(repo, "Packages"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "Package: chrony") {
		t.Fatalf("Packages 缺少 chrony:\n%s", data)
	}
}

func TestScanTarGz(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []string{"a.tar.gz", "b.tar.gz", "c.txt", "sub/d.tar.gz"} {
		p := filepath.Join(dir, filepath.FromSlash(f))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := scanTarGz(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 top-level *.tar.gz, got %v", got)
	}
	// 排序稳定
	if got[0] != filepath.Join(dir, "a.tar.gz") || got[1] != filepath.Join(dir, "b.tar.gz") {
		t.Fatalf("unexpected order: %v", got)
	}
}

// TestLocalIP 验证 LocalIP 返回合法非 loopback IPv4（serve --advertise-host 默认值）。
func TestLocalIP(t *testing.T) {
	ip := LocalIP()
	parsed := net.ParseIP(ip)
	if parsed == nil || parsed.To4() == nil {
		t.Fatalf("LocalIP 应返回合法 IPv4: %q", ip)
	}
}

// TestServeHotLoad 端到端验证 --dir 热加载：服务启动后向目录拷贝新 bundle，
// 轮询检测到后自动重建软件源。
func TestServeHotLoad(t *testing.T) {
	dir := t.TempDir()
	pkgDir := filepath.Join(dir, "pkgs")
	dataDir := filepath.Join(dir, "data")
	if err := os.MkdirAll(pkgDir, 0o755); err != nil {
		t.Fatal(err)
	}

	makeBundle := func(name, debName string) string {
		bundleDir := filepath.Join(dir, "src-"+name)
		if err := os.MkdirAll(filepath.Join(bundleDir, "packages"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(bundleDir, "manifest.yaml"), []byte("schema_version: 1\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := writeMinimalDeb(filepath.Join(bundleDir, "packages", debName),
			"Package: "+debName+"\nVersion: 1.0\nArchitecture: amd64\nDescription: test\n"); err != nil {
			t.Fatal(err)
		}
		out := filepath.Join(dir, name+".tar.gz")
		if err := tarGzDir(bundleDir, out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	bundleA := makeBundle("a", "aa.deb")
	bundleB := makeBundle("b", "bb.deb")

	// 初始只放入 a
	if err := os.Rename(bundleA, filepath.Join(pkgDir, "a.tar.gz")); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		_, err := Run(ctx, Options{
			Dir:        pkgDir,
			DataDir:    dataDir,
			RepoAddr:   "127.0.0.1:0",
			SkipImages: true,
		})
		errCh <- err
	}()

	packagesFile := filepath.Join(dataDir, "repos", "deb", "Packages")
	waitFor(t, 10*time.Second, func() bool {
		b, err := os.ReadFile(packagesFile)
		return err == nil && strings.Contains(string(b), "Package: aa.deb")
	}, "initial Packages 含 aa.deb")

	// 热加载：把 b 拷入目录
	if err := os.Rename(bundleB, filepath.Join(pkgDir, "b.tar.gz")); err != nil {
		t.Fatal(err)
	}

	// 轮询 3s，等待新包进入源（最多 15s）
	waitFor(t, 15*time.Second, func() bool {
		b, err := os.ReadFile(packagesFile)
		return err == nil && strings.Contains(string(b), "Package: bb.deb")
	}, "热加载后 Packages 含 bb.deb")

	cancel()
	if err := <-errCh; err != nil {
		t.Fatalf("Run 返回错误: %v", err)
	}
}

// TestServeHotLoadIncompleteFile 验证热加载对"拷贝未完成"的文件不误加载：
// 先放入不完整 tar.gz，serve 解压失败后自动重试，待文件补全后成功加载。
func TestServeHotLoadIncompleteFile(t *testing.T) {
	dir := t.TempDir()
	pkgDir := filepath.Join(dir, "pkgs")
	dataDir := filepath.Join(dir, "data")
	if err := os.MkdirAll(pkgDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// 构造完整 bundle（含 bb.deb）
	bundleDir := filepath.Join(dir, "bundle")
	if err := os.MkdirAll(filepath.Join(bundleDir, "packages"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundleDir, "manifest.yaml"), []byte("schema_version: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeMinimalDeb(filepath.Join(bundleDir, "packages", "bb.deb"),
		"Package: bb.deb\nVersion: 1.0\nArchitecture: amd64\nDescription: test\n"); err != nil {
		t.Fatal(err)
	}
	fullTar := filepath.Join(dir, "full.tar.gz")
	if err := tarGzDir(bundleDir, fullTar); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(fullTar)
	if err != nil {
		t.Fatal(err)
	}

	// 先放入"不完整"文件（前一半字节），模拟拷贝中
	target := filepath.Join(pkgDir, "b.tar.gz")
	if err := os.WriteFile(target, content[:len(content)/2], 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		_, err := Run(ctx, Options{Dir: pkgDir, DataDir: dataDir, RepoAddr: "127.0.0.1:0", SkipImages: true})
		errCh <- err
	}()

	// 等 serve 至少两帧看到不完整文件（触发一次解压失败并重试），再补全
	time.Sleep(7 * time.Second)
	if err := os.WriteFile(target, content, 0o644); err != nil {
		t.Fatal(err)
	}

	// 等待补全后最终加载成功
	packagesFile := filepath.Join(dataDir, "repos", "deb", "Packages")
	waitFor(t, 20*time.Second, func() bool {
		b, err := os.ReadFile(packagesFile)
		return err == nil && strings.Contains(string(b), "Package: bb.deb")
	}, "补全后热加载 bb.deb")

	cancel()
	if err := <-errCh; err != nil {
		t.Fatalf("Run 返回错误: %v", err)
	}
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatalf("等待超时: %s", msg)
}

// tarGzDir 将目录打包为 tar.gz（不含顶层目录本身）。
func tarGzDir(srcDir, outPath string) error {
	f, err := os.Create(outPath)
	if err != nil {
		return err
	}
	defer f.Close()
	gw := gzip.NewWriter(f)
	defer gw.Close()
	tw := tar.NewWriter(gw)
	defer tw.Close()
	return filepath.Walk(srcDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(srcDir, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if info.IsDir() {
			return tw.WriteHeader(&tar.Header{Name: rel + "/", Typeflag: tar.TypeDir, Mode: 0o755})
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := tw.WriteHeader(&tar.Header{Name: rel, Mode: int64(info.Mode()), Size: int64(len(b))}); err != nil {
			return err
		}
		_, err = tw.Write(b)
		return err
	})
}
