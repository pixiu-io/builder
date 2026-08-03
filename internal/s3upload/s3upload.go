// Package s3upload 将离线包产物上传到 S3（含 MinIO 等 S3 兼容存储）。
// 凭证走标准 AWS 链：环境变量 / shared credentials / IAM role，不在配置文件中写密钥。
package s3upload

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// Options 上传参数（可由 builder.yaml s3 节 + CLI flag 合并）。
type Options struct {
	// Bucket 目标桶（必填）。
	Bucket string
	// Region AWS 区域；自定义 endpoint（MinIO）时可填任意合法值，默认 us-east-1。
	Region string
	// Endpoint 可选自定义 endpoint（如 http://127.0.0.1:9000）；空则走 AWS 公有云。
	Endpoint string
	// Prefix 对象键前缀（如 pixiu-offline/ 或 releases/v1.27.3/）。
	Prefix string
	// ForcePathStyle 强制 path-style（MinIO 常用）。
	ForcePathStyle bool
	// AccessKey / SecretKey 可选显式凭证；皆空时使用默认凭证链。
	AccessKey string
	SecretKey string
	// SessionToken 可选临时凭证。
	SessionToken string
}

// Result 单个文件上传结果。
type Result struct {
	LocalPath string
	Bucket    string
	Key       string
	URI       string // s3://bucket/key 或自定义 endpoint 可读形式
}

// Validate 校验上传必要参数。
func (o Options) Validate() error {
	if strings.TrimSpace(o.Bucket) == "" {
		return fmt.Errorf("s3 bucket 不能为空（配置 s3.bucket 或 --s3-bucket）")
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

// UploadFiles 依次上传本地文件到 S3，返回每个文件的上传结果。
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

	client, err := newClient(ctx, opts)
	if err != nil {
		return nil, err
	}

	var out []Result
	for _, f := range files {
		key := ObjectKey(opts.Prefix, f)
		if err := putFile(ctx, client, opts.Bucket, key, f); err != nil {
			return out, fmt.Errorf("上传 %s → s3://%s/%s 失败: %w", f, opts.Bucket, key, err)
		}
		out = append(out, Result{
			LocalPath: f,
			Bucket:    opts.Bucket,
			Key:       key,
			URI:       formatURI(opts, key),
		})
	}
	return out, nil
}

func formatURI(opts Options, key string) string {
	if opts.Endpoint != "" {
		ep := strings.TrimRight(opts.Endpoint, "/")
		return fmt.Sprintf("%s/%s/%s", ep, opts.Bucket, key)
	}
	return fmt.Sprintf("s3://%s/%s", opts.Bucket, key)
}

func newClient(ctx context.Context, opts Options) (*s3.Client, error) {
	region := opts.Region
	if region == "" {
		region = "us-east-1"
	}

	loadOpts := []func(*awsconfig.LoadOptions) error{
		awsconfig.WithRegion(region),
	}
	if opts.AccessKey != "" && opts.SecretKey != "" {
		loadOpts = append(loadOpts, awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(opts.AccessKey, opts.SecretKey, opts.SessionToken),
		))
	}

	cfg, err := awsconfig.LoadDefaultConfig(ctx, loadOpts...)
	if err != nil {
		return nil, fmt.Errorf("加载 AWS 配置失败: %w", err)
	}

	var s3Opts []func(*s3.Options)
	if opts.Endpoint != "" {
		ep := opts.Endpoint
		s3Opts = append(s3Opts, func(o *s3.Options) {
			o.BaseEndpoint = aws.String(ep)
			o.UsePathStyle = true // MinIO / 自定义 endpoint 使用 path-style
		})
	} else if opts.ForcePathStyle {
		s3Opts = append(s3Opts, func(o *s3.Options) {
			o.UsePathStyle = true
		})
	}

	return s3.NewFromConfig(cfg, s3Opts...), nil
}

func putFile(ctx context.Context, client *s3.Client, bucket, key, localPath string) error {
	f, err := os.Open(localPath)
	if err != nil {
		return err
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		return err
	}

	_, err = client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(bucket),
		Key:           aws.String(key),
		Body:          f,
		ContentLength: aws.Int64(st.Size()),
	})
	return err
}
