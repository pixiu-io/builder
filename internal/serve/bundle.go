package serve

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"builder/internal/builder"
	"builder/internal/manifest"
)

// loadBundles 解压/定位各个 bundle，返回含 manifest.yaml 的根目录列表。
func loadBundles(inputs []string, destRoot string) ([]string, error) {
	if err := os.MkdirAll(destRoot, 0o755); err != nil {
		return nil, err
	}
	var roots []string
	for i, in := range inputs {
		root, err := resolveBundle(in, filepath.Join(destRoot, fmt.Sprintf("b%d", i)))
		if err != nil {
			return nil, fmt.Errorf("加载 %s 失败: %w", in, err)
		}
		roots = append(roots, root)
	}
	return roots, nil
}

// resolveBundle 加载离线包：支持 builder 产物（含 manifest.yaml）与单镜像
// docker save 的 .tar.gz（含 manifest.json）。二者可经 --bundle / --dir 混放。
func resolveBundle(path, extractDir string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		mf := filepath.Join(path, manifest.ManifestFileName)
		if _, err := os.Stat(mf); err != nil {
			return "", fmt.Errorf("目录下未找到 manifest.yaml")
		}
		return path, nil
	}
	if !strings.HasSuffix(path, ".tar.gz") {
		return "", fmt.Errorf("必须是目录或 .tar.gz")
	}
	kind, err := peekTarGzKind(path)
	if err != nil {
		return "", fmt.Errorf("读取 tar.gz 失败: %w", err)
	}
	if err := os.MkdirAll(extractDir, 0o755); err != nil {
		return "", err
	}
	// 单镜像 docker-save：只 gunzip 成 .tar，避免整包 Untar 占磁盘。
	if kind == "docker-save" {
		return materializeDockerSaveBundle(path, extractDir)
	}
	if err := builder.UntarGz(path, extractDir); err != nil {
		return "", fmt.Errorf("解压失败: %w", err)
	}
	if root := findManifestDir(extractDir); root != "" {
		return root, nil
	}
	if isDockerSaveDir(extractDir) {
		return materializeDockerSaveBundle(path, extractDir)
	}
	return "", fmt.Errorf("tar.gz 既不是 builder 离线包（缺 manifest.yaml），也不是 docker save 镜像（缺 manifest.json）")
}

// peekTarGzKind 扫描 tar.gz 成员名，区分 builder 离线包与 docker save。
// 优先 manifest.yaml（bundle）；否则有 manifest.json 则为 docker-save。
func peekTarGzKind(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return "", err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	hasYAML, hasJSON := false, false
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
		base := filepath.Base(hdr.Name)
		switch base {
		case manifest.ManifestFileName:
			hasYAML = true
		case "manifest.json":
			hasJSON = true
		}
		if hasYAML {
			return "bundle", nil
		}
	}
	if hasJSON {
		return "docker-save", nil
	}
	return "", nil
}

// isDockerSaveDir 判断目录是否为 docker save 解压结果（根目录含 manifest.json）。
func isDockerSaveDir(dir string) bool {
	st, err := os.Stat(filepath.Join(dir, "manifest.json"))
	return err == nil && !st.IsDir()
}

// materializeDockerSaveBundle 将单镜像 docker-save .tar.gz 落成伪 bundle：
// images/addons/<name>.tar（未压缩，供 ImageFromPath / RepoTags 读取）。
func materializeDockerSaveBundle(tarGzPath, extractDir string) (string, error) {
	name := tarGzBaseName(tarGzPath)
	imgDir := filepath.Join(extractDir, "images", "addons")
	if err := os.MkdirAll(imgDir, 0o755); err != nil {
		return "", err
	}
	dest := filepath.Join(imgDir, name+".tar")
	if err := gunzipFile(tarGzPath, dest); err != nil {
		return "", fmt.Errorf("还原 docker-save tar 失败: %w", err)
	}
	return extractDir, nil
}

// tarGzBaseName 返回 foo.tar.gz → foo。
func tarGzBaseName(path string) string {
	base := filepath.Base(path)
	if len(base) >= 7 && strings.EqualFold(base[len(base)-7:], ".tar.gz") {
		base = base[:len(base)-7]
	}
	if strings.TrimSpace(base) == "" || base == "." {
		return "image"
	}
	return base
}

// gunzipFile 将 .gz 文件解压为普通文件（用于 docker-save .tar.gz → .tar）。
func gunzipFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	gz, err := gzip.NewReader(in)
	if err != nil {
		return err
	}
	defer gz.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, gz)
	return err
}

func findManifestDir(root string) string {
	var found string
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || found != "" {
			return nil
		}
		if !info.IsDir() && info.Name() == manifest.ManifestFileName {
			found = filepath.Dir(path)
		}
		return nil
	})
	return found
}

// collectImageTars 收集所有 bundle 中的镜像 tar。
func collectImageTars(roots []string) ([]imageTar, error) {
	var out []imageTar
	seen := map[string]bool{}
	for _, root := range roots {
		mfPath := filepath.Join(root, manifest.ManifestFileName)
		m, err := manifest.Load(mfPath)
		if err != nil {
			// 无 manifest 时扫描目录
			for _, sub := range []string{"images/core", "images/addons"} {
				dir := filepath.Join(root, filepath.FromSlash(sub))
				entries, err := os.ReadDir(dir)
				if err != nil {
					continue
				}
				for _, e := range entries {
					if e.IsDir() || !strings.HasSuffix(e.Name(), ".tar") {
						continue
					}
					name := strings.TrimSuffix(e.Name(), ".tar")
					key := name
					if seen[key] {
						continue
					}
					seen[key] = true
					out = append(out, imageTar{
						Name: name,
						Path: filepath.Join(dir, e.Name()),
					})
				}
			}
			continue
		}
		for _, img := range m.Images {
			if img.Tar == "" {
				continue
			}
			key := img.Name
			if key == "" {
				key = strings.TrimSuffix(filepath.Base(img.Tar), ".tar")
			}
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, imageTar{
				Name:        key,
				SourceImage: img.SourceImage,
				Path:        filepath.Join(root, filepath.FromSlash(img.Tar)),
			})
		}
	}
	return out, nil
}

type imageTar struct {
	Name        string
	SourceImage string
	Path        string
}

// collectPackages 收集 .rpm / .deb。
func collectPackages(roots []string) (rpms, debs []string, err error) {
	seen := map[string]bool{}
	for _, root := range roots {
		pkgDir := filepath.Join(root, "packages")
		err := filepath.Walk(pkgDir, func(path string, info os.FileInfo, walkErr error) error {
			if walkErr != nil {
				if os.IsNotExist(walkErr) {
					return nil
				}
				return walkErr
			}
			if info.IsDir() {
				return nil
			}
			base := info.Name()
			ext := strings.ToLower(filepath.Ext(base))
			if ext != ".rpm" && ext != ".deb" {
				return nil
			}
			// 同名去重（多 bundle 合并）
			if seen[base] {
				return nil
			}
			seen[base] = true
			switch ext {
			case ".rpm":
				rpms = append(rpms, path)
			case ".deb":
				debs = append(debs, path)
			}
			return nil
		})
		if err != nil {
			return nil, nil, err
		}
	}
	return rpms, debs, nil
}

func linkOrCopy(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if _, err := os.Stat(dst); err == nil {
		return nil
	}
	if err := os.Link(src, dst); err == nil {
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}
