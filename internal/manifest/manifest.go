// Package manifest 生成离线包的 manifest.yaml 元数据，
// 并提供 verify 校验：逐个核对文件存在且 sha256 匹配。
package manifest

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// 当前 manifest schema 版本。
const SchemaVersion = 1

// ManifestFileName manifest 文件名。
const ManifestFileName = "manifest.yaml"

// Meta bundle 元数据。
type Meta struct {
	OS         string `yaml:"os"`
	OSVersion  string `yaml:"os_version"`
	Arch       string `yaml:"arch"`
	K8sVersion string `yaml:"k8s_version"`
	Mirror     string `yaml:"mirror"`
	HostArch   string `yaml:"host_arch"`
}

// File 通用文件条目（packages 与 scripts）。
type File struct {
	Path   string `yaml:"path"`
	Size   int64  `yaml:"size"`
	SHA256 string `yaml:"sha256"`
}

// Image 镜像 tar 条目。
type Image struct {
	Name        string `yaml:"name"`
	SourceImage string `yaml:"source_image"`
	Tar         string `yaml:"tar"`
	Size        int64  `yaml:"size"`
	SHA256      string `yaml:"sha256"`
}

// Script 脚本条目。
type Script struct {
	Path string `yaml:"path"`
}

// Manifest 离线包清单。
type Manifest struct {
	SchemaVersion int      `yaml:"schema_version"`
	GeneratedAt   string   `yaml:"generated_at"`
	Meta          Meta     `yaml:"meta"`
	Files         []File   `yaml:"files"`
	Images        []Image  `yaml:"images"`
	Scripts       []Script `yaml:"scripts"`
}

// Generate 遍历 bundle 目录生成 manifest。
// 自动将文件归类为 packages/scripts 与镜像 tar。
func Generate(bundleRoot string, meta Meta) (*Manifest, error) {
	m := &Manifest{
		SchemaVersion: SchemaVersion,
		GeneratedAt:   time.Now().Format("2006-01-02 15:04:05"),
		Meta:          meta,
	}

	err := filepath.Walk(bundleRoot, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(bundleRoot, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == ManifestFileName {
			return nil
		}

		sum, size, err := fileInfo(path)
		if err != nil {
			return fmt.Errorf("读取文件 %s 失败: %w", rel, err)
		}

		switch {
		case strings.HasPrefix(rel, "images/"):
			name := strings.TrimSuffix(filepath.Base(rel), ".tar")
			m.Images = append(m.Images, Image{
				Name:        name,
				SourceImage: sourceImageHint(name),
				Tar:         rel,
				Size:        size,
				SHA256:      sum,
			})
		case strings.HasPrefix(rel, "install/"):
			m.Scripts = append(m.Scripts, Script{Path: rel})
			m.Files = append(m.Files, File{Path: rel, Size: size, SHA256: sum})
		default:
			m.Files = append(m.Files, File{Path: rel, Size: size, SHA256: sum})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	// 排序保证输出确定性
	sort.Slice(m.Files, func(i, j int) bool { return m.Files[i].Path < m.Files[j].Path })
	sort.Slice(m.Images, func(i, j int) bool { return m.Images[i].Tar < m.Images[j].Tar })
	sort.Slice(m.Scripts, func(i, j int) bool { return m.Scripts[i].Path < m.Scripts[j].Path })

	return m, nil
}

// Write 写入 manifest 到指定路径。
func (m *Manifest) Write(path string) error {
	data, err := yaml.Marshal(m)
	if err != nil {
		return fmt.Errorf("序列化 manifest 失败: %w", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("写入 manifest 失败: %w", err)
	}
	return nil
}

// Load 读取 manifest 文件。
func Load(path string) (*Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取 manifest 失败: %w", err)
	}
	var m Manifest
	if err := yaml.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("解析 manifest 失败: %w", err)
	}
	return &m, nil
}

// Verify 校验 bundle 完整性：逐个核对文件存在 + size + sha256。
// bundleRoot 为 manifest.yaml 所在目录。
func (m *Manifest) Verify(bundleRoot string) error {
	var errs []string

	// 校验通用文件
	for _, f := range m.Files {
		if err := verifyFile(bundleRoot, f.Path, f.Size, f.SHA256); err != nil {
			errs = append(errs, err.Error())
		}
	}
	// 校验镜像 tar
	for _, img := range m.Images {
		if err := verifyFile(bundleRoot, img.Tar, img.Size, img.SHA256); err != nil {
			errs = append(errs, err.Error())
		}
	}
	// 脚本（已在 Files 中校验 sha256，这里补存在性检查）
	for _, s := range m.Scripts {
		path := filepath.Join(bundleRoot, filepath.FromSlash(s.Path))
		if _, err := os.Stat(path); err != nil {
			errs = append(errs, fmt.Sprintf("脚本不存在: %s", s.Path))
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("manifest 校验失败（%d 项）:\n  - %s", len(errs), strings.Join(errs, "\n  - "))
	}
	return nil
}

func verifyFile(root, rel string, wantSize int64, wantSHA string) error {
	path := filepath.Join(root, filepath.FromSlash(rel))
	st, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("文件缺失: %s (%v)", rel, err)
	}
	if st.IsDir() {
		return fmt.Errorf("期望文件实为目录: %s", rel)
	}
	if st.Size() != wantSize {
		return fmt.Errorf("大小不匹配 %s: 期望 %d 实际 %d", rel, wantSize, st.Size())
	}
	if wantSHA != "" {
		sum, err := fileSHA256(path)
		if err != nil {
			return fmt.Errorf("计算 sha256 失败 %s: %v", rel, err)
		}
		if sum != wantSHA {
			return fmt.Errorf("sha256 不匹配 %s: 期望 %s 实际 %s", rel, wantSHA, sum)
		}
	}
	return nil
}

// sourceImageHint 从 tar 文件名反推镜像名（仅作提示，manifest 中的 source_image
// 对 addon 是精确值，对 core 镜像由镜像列表回填）。
func sourceImageHint(name string) string {
	return name
}

func fileInfo(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", 0, err
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), st.Size(), nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
