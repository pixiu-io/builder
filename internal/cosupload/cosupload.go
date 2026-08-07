// Package cosupload 将离线包产物上传到腾讯云 COS（对象存储）。
package cosupload

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/tencentyun/cos-go-sdk-v5"
)

// Options COS 上传参数（可由 builder.yaml cos 节 + CLI flag 合并）。
type Options struct {
	// Bucket 存储桶，需为「桶名-appid」格式（如 mybucket-1250000000）。
	Bucket string
	// Region 地域（如 ap-guangzhou、ap-beijing）。
	Region string
	// SecretID / SecretKey 腾讯云 API 密钥。
	SecretID  string
	SecretKey string
	// Prefix 对象键前缀（如 pixiu-offline/ 或 releases/v1.27.3/）。
	Prefix string
}

// Result 单个文件上传结果。
type Result struct {
	LocalPath string
	Bucket    string
	Key       string
	URI       string // cos://bucket.cos.region.myqcloud.com/key
}

// Validate 校验上传必要参数。
func (o Options) Validate() error {
	if strings.TrimSpace(o.Bucket) == "" {
		return fmt.Errorf("cos bucket 不能为空（配置 cos.bucket 或 --cos-bucket，需含 appid）")
	}
	if strings.TrimSpace(o.Region) == "" {
		return fmt.Errorf("cos region 不能为空（配置 cos.region 或 --cos-region）")
	}
	if strings.TrimSpace(o.SecretID) == "" || strings.TrimSpace(o.SecretKey) == "" {
		return fmt.Errorf("cos secret_id/secret_key 不能为空（配置 cos.secret_id/cos.secret_key 或 --cos-secret-id/--cos-secret-key）")
	}
	return nil
}

// ObjectKey 根据前缀与本地文件名生成对象键。
func ObjectKey(prefix, localPath string) string {
	base := filepath.Base(localPath)
	p := strings.TrimSpace(prefix)
	p = strings.TrimLeft(p, "/")
	if p == "" {
		return base
	}
	if !strings.HasSuffix(p, "/") {
		p += "/"
	}
	return p + base
}

// UploadFiles 依次上传本地文件到 COS，返回每个文件的上传结果。
func UploadFiles(ctx context.Context, opts Options, files []string) ([]Result, error) {
	if err := opts.Validate(); err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("没有可上传的文件")
	}
	for _, f := range files {
		if st, err := os.Stat(f); err != nil {
			return nil, fmt.Errorf("本地文件不可用 %s: %w", f, err)
		} else if st.IsDir() {
			return nil, fmt.Errorf("不能上传目录: %s", f)
		}
	}

	client, err := newClient(opts)
	if err != nil {
		return nil, err
	}

	var out []Result
	for _, f := range files {
		key := ObjectKey(opts.Prefix, f)
		if err := putFile(ctx, client, key, f); err != nil {
			return out, fmt.Errorf("上传 %s → %s 失败: %w", f, objectURI(opts, key), err)
		}
		out = append(out, Result{
			LocalPath: f,
			Bucket:    opts.Bucket,
			Key:       key,
			URI:       objectURI(opts, key),
		})
	}
	return out, nil
}

func objectURI(opts Options, key string) string {
	return fmt.Sprintf("https://%s.cos.%s.myqcloud.com/%s", opts.Bucket, opts.Region, key)
}

func newClient(opts Options) (*cos.Client, error) {
	u, err := url.Parse(fmt.Sprintf("https://%s.cos.%s.myqcloud.com", opts.Bucket, opts.Region))
	if err != nil {
		return nil, fmt.Errorf("解析 COS bucket URL 失败: %w", err)
	}
	return cos.NewClient(&cos.BaseURL{BucketURL: u}, &http.Client{
		Transport: &cos.AuthorizationTransport{
			SecretID:  opts.SecretID,
			SecretKey: opts.SecretKey,
		},
	}), nil
}

func putFile(ctx context.Context, client *cos.Client, key, localPath string) error {
	f, err := os.Open(localPath)
	if err != nil {
		return err
	}
	defer f.Close()

	_, err = client.Object.Put(ctx, key, f, nil)
	return err
}
