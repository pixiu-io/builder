package serve

import (
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
	if err := os.MkdirAll(extractDir, 0o755); err != nil {
		return "", err
	}
	if err := builder.UntarGz(path, extractDir); err != nil {
		return "", fmt.Errorf("解压失败: %w", err)
	}
	root := findManifestDir(extractDir)
	if root == "" {
		return "", fmt.Errorf("tar.gz 中未找到 manifest.yaml")
	}
	return root, nil
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
