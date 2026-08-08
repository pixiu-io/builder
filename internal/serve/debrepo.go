package serve

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"
)

// writeDebRepo 生成简易 apt 源：Packages + Packages.gz（deb [trusted=yes] http://host/deb ./）。
func writeDebRepo(debDir string, debFiles []string) error {
	var blocks []string
	for _, path := range debFiles {
		ctrl, err := readDebControl(path)
		if err != nil {
			return fmt.Errorf("解析 %s: %w", path, err)
		}
		st, err := os.Stat(path)
		if err != nil {
			return err
		}
		sum, err := fileSHA256(path)
		if err != nil {
			return err
		}
		ctrl = upsertControlField(ctrl, "Filename", filepath.Base(path))
		ctrl = upsertControlField(ctrl, "Size", fmt.Sprintf("%d", st.Size()))
		ctrl = upsertControlField(ctrl, "SHA256", sum)
		blocks = append(blocks, strings.TrimSpace(ctrl))
	}
	content := strings.Join(blocks, "\n\n") + "\n"
	if err := os.WriteFile(filepath.Join(debDir, "Packages"), []byte(content), 0o644); err != nil {
		return err
	}
	gz, err := gzipBytes([]byte(content))
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(debDir, "Packages.gz"), gz, 0o644)
}

func upsertControlField(control, key, value string) string {
	// 真实 deb 的 control 文件总以换行符结尾，strings.Split 会在末尾产生
	// 空字符串；若不先去掉尾部换行，append 后 join 会在原内容与新字段之间
	// 插入空行，把该包的 stanza 拆成两段（后段无 Package: 头），导致
	// apt-get update 报 E: Encountered a section with no Package: header。
	control = strings.TrimRight(control, "\n")
	prefix := key + ": "
	lines := strings.Split(control, "\n")
	found := false
	for i, line := range lines {
		if strings.HasPrefix(strings.ToLower(line), strings.ToLower(key)+":") {
			lines[i] = prefix + value
			found = true
			break
		}
	}
	if !found {
		lines = append(lines, prefix+value)
	}
	return strings.Join(lines, "\n")
}

// readDebControl 从 .deb (ar) 中读取 control.tar.* 内的 control 文件。
func readDebControl(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	magic := make([]byte, 8)
	if _, err := io.ReadFull(f, magic); err != nil {
		return "", err
	}
	if string(magic) != "!<arch>\n" {
		return "", fmt.Errorf("不是有效的 deb/ar 文件")
	}

	for {
		hdr, err := readARHeader(f)
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
		name := strings.TrimSpace(hdr.Name)
		data := make([]byte, hdr.Size)
		if _, err := io.ReadFull(f, data); err != nil {
			return "", err
		}
		if hdr.Size%2 == 1 {
			var pad [1]byte
			_, _ = io.ReadFull(f, pad[:])
		}

		switch {
		case name == "control.tar.gz" || strings.HasPrefix(name, "control.tar.gz"):
			return extractControlFromTarGz(data)
		case name == "control.tar.xz" || strings.HasPrefix(name, "control.tar.xz"):
			return extractControlFromTarXz(data)
		case name == "control.tar.zst" || strings.HasPrefix(name, "control.tar.zst"):
			return extractControlFromTarZst(data)
		case name == "control.tar":
			// 注意：必须精确匹配未压缩的 control.tar，否则会误匹配
			// control.tar.zst / .xz / .gz 等压缩变体。
			return extractControlFromTar(bytes.NewReader(data))
		}
	}
	return "", fmt.Errorf("未找到 control.tar.*（支持 gz / xz / zst / 未压缩）")
}

type arHeader struct {
	Name string
	Size int64
}

func readARHeader(r io.Reader) (arHeader, error) {
	buf := make([]byte, 60)
	if _, err := io.ReadFull(r, buf); err != nil {
		return arHeader{}, err
	}
	name := strings.TrimSpace(string(buf[0:16]))
	sizeStr := strings.TrimSpace(string(buf[48:58]))
	var size int64
	if _, err := fmt.Sscanf(sizeStr, "%d", &size); err != nil {
		return arHeader{}, fmt.Errorf("无效 ar size: %q", sizeStr)
	}
	if strings.HasPrefix(name, "#1/") {
		n := 0
		fmt.Sscanf(name[3:], "%d", &n)
		nameBytes := make([]byte, n)
		if _, err := io.ReadFull(r, nameBytes); err != nil {
			return arHeader{}, err
		}
		name = strings.TrimRight(string(nameBytes), "\x00")
		size -= int64(n)
	}
	return arHeader{Name: name, Size: size}, nil
}

func extractControlFromTarGz(data []byte) (string, error) {
	gr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	defer gr.Close()
	return extractControlFromTar(gr)
}

// extractControlFromTarXz 读取 control.tar.xz 内的 control 文件。
func extractControlFromTarXz(data []byte) (string, error) {
	xr, err := xz.NewReader(bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	return extractControlFromTar(xr)
}

// extractControlFromTarZst 读取 control.tar.zst 内的 control 文件
// （Ubuntu 23.10+ / Debian 12+ 新 dpkg 默认 zstd 压缩）。
func extractControlFromTarZst(data []byte) (string, error) {
	zr, err := zstd.NewReader(bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	defer zr.Close()
	return extractControlFromTar(zr)
}

func extractControlFromTar(r io.Reader) (string, error) {
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
		if filepath.Base(hdr.Name) == "control" {
			b, err := io.ReadAll(tr)
			if err != nil {
				return "", err
			}
			return string(b), nil
		}
	}
	return "", fmt.Errorf("control.tar 中未找到 control 文件")
}
