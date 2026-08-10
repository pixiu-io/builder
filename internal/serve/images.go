package serve

import (
	"archive/tar"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"

	"builder/internal/images"
)

// importImages 将 docker-save tar 以短名推送到本地 registry。
// pushHost 用于 remote.Write（一般为 127.0.0.1:port）；
// advertiseHostPort 用于返回给用户的引用（如 192.168.1.10:5000）；
// namespace 为 registry 发布命名空间（镜像引用前缀，如 pixiu）。非空时发布
// 路径为 <host>:5000/<namespace>/<repo>:<tag>，空则保持 <host>:5000/<repo>:<tag>。
func importImages(ctx context.Context, roots []string, pushHost, advertiseHostPort, namespace string) ([]string, error) {
	tars, err := collectImageTars(roots)
	if err != nil {
		return nil, err
	}
	if len(tars) == 0 {
		return nil, nil
	}

	var refs []string
	for _, it := range tars {
		select {
		case <-ctx.Done():
			return refs, ctx.Err()
		default:
		}
		if _, err := os.Stat(it.Path); err != nil {
			return refs, fmt.Errorf("镜像 tar 不存在 %s: %w", it.Path, err)
		}

		// repo 名优先取可确认带 tag 的 source_image 短名；旧版 bundle 的 source_image
		// 可能只是 tar 文件名提示（如 kubez-ansible-v2），此时应回退读取 docker-save
		// manifest.json 的 RepoTags，避免把 tar 名误当 registry repo 名。
		short := resolveRepoShortName(it)
		tag := resolveTag(it)
		repoName := sanitizeRepoName(short)

		// 发布路径：namespace 非空时加前缀（如 pixiu/pause），用户指定
		// namespace 时直接 push 到该前缀下；空则保持短名仓库。
		repoPath := repoName
		if strings.TrimSpace(namespace) != "" {
			repoPath = sanitizeRepoName(namespace) + "/" + repoName
		}

		img, err := tarball.ImageFromPath(it.Path, nil)
		if err != nil {
			return refs, fmt.Errorf("读取 docker-save %s 失败: %w", it.Path, err)
		}

		pushRefStr := fmt.Sprintf("%s/%s:%s", pushHost, repoPath, tag)
		ref, err := name.ParseReference(pushRefStr, name.Insecure)
		if err != nil {
			return refs, fmt.Errorf("解析引用 %s 失败: %w", pushRefStr, err)
		}
		if err := remote.Write(ref, img); err != nil {
			return refs, fmt.Errorf("推送 %s 失败: %w", pushRefStr, err)
		}
		adv := fmt.Sprintf("%s/%s:%s", advertiseHostPort, repoPath, tag)
		refs = append(refs, adv)
		fmt.Printf("  + %s\n", adv)
	}
	return refs, nil
}

func resolveRepoShortName(it imageTar) string {
	// 只有 source_image 明确带 tag 时才信任它：新版 manifest 会回填完整 source_image，
	// 旧版 manifest 可能只是 tar 文件名提示（例如 kubez-ansible-v2），不应作为 repo 名。
	if tagFromImageRef(it.SourceImage) != "" {
		if short := images.ShortName(it.SourceImage); short != "" {
			return short
		}
	}
	if tags, err := dockerTarRepoTags(it.Path); err == nil {
		for _, t := range tags {
			if tagFromImageRef(t) != "" {
				if short := images.ShortName(t); short != "" {
					return short
				}
			}
		}
	}
	return it.Name
}

func resolveTag(it imageTar) string {
	if tag := tagFromImageRef(it.SourceImage); tag != "" {
		return tag
	}
	if tags, err := dockerTarRepoTags(it.Path); err == nil {
		for _, t := range tags {
			if tag := tagFromImageRef(t); tag != "" {
				return tag
			}
		}
	}
	return "latest"
}

// tagFromImageRef 从完整镜像引用提取 tag；无 tag 或仅 digest 时返回空。
func tagFromImageRef(ref string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return ""
	}
	// 去掉 digest
	if i := strings.Index(ref, "@"); i >= 0 {
		ref = ref[:i]
	}
	// tag 在最后一个 / 之后的 :
	lastSlash := strings.LastIndex(ref, "/")
	colon := strings.LastIndex(ref, ":")
	if colon < 0 || colon < lastSlash {
		return ""
	}
	return ref[colon+1:]
}

func sanitizeRepoName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "image"
	}
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteRune('-')
		}
	}
	s := b.String()
	s = strings.Trim(s, ".-")
	if s == "" {
		return "image"
	}
	return s
}

type dockerManifestEntry struct {
	RepoTags []string `json:"RepoTags"`
}

func dockerTarRepoTags(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	tr := tar.NewReader(f)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if hdr.Name != "manifest.json" && !strings.HasSuffix(hdr.Name, "/manifest.json") {
			continue
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			return nil, err
		}
		var entries []dockerManifestEntry
		if err := json.Unmarshal(data, &entries); err != nil {
			return nil, err
		}
		var tags []string
		for _, e := range entries {
			tags = append(tags, e.RepoTags...)
		}
		return tags, nil
	}
	return nil, fmt.Errorf("manifest.json not found")
}
