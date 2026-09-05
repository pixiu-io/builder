package main

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/spf13/cobra"
)

const defaultImagesLimit = 50

func newImagesCmd() *cobra.Command {
	var (
		registryAddr string
		limit        int
	)
	cmd := &cobra.Command{
		Use:   "images",
		Short: "列出 serve registry 中的镜像（类似 docker images）",
		Long: `查询运行中的 serve registry（Docker Registry HTTP API V2），
以表格打印 REPOSITORY / TAG / IMAGE ID。默认最多显示 50 条，可用 --limit 调整。`,
		Example: `  builder images
  builder images --registry-addr 192.168.1.10:5000 --limit 100`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runImages(context.Background(), registryAddr, limit)
		},
	}
	cmd.Flags().StringVar(&registryAddr, "registry-addr", "127.0.0.1:5000", "serve registry 地址")
	cmd.Flags().IntVar(&limit, "limit", defaultImagesLimit, "最多显示的镜像条数（repo:tag）")
	return cmd
}

func newLsCmd() *cobra.Command {
	var registryAddr string
	cmd := &cobra.Command{
		Use:   "ls [repository]",
		Short: "列出指定仓库在 serve registry 中的 tag",
		Example: `  builder ls pixiu/pause
  builder ls kube-apiserver --registry-addr 192.168.1.10:5000`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runLsTags(context.Background(), registryAddr, args[0])
		},
	}
	cmd.Flags().StringVar(&registryAddr, "registry-addr", "127.0.0.1:5000", "serve registry 地址")
	return cmd
}

type imageRow struct {
	Repository string
	Tag        string
	ImageID    string
}

func runImages(ctx context.Context, addr string, limit int) error {
	if limit <= 0 {
		return fmt.Errorf("--limit 必须为正整数，当前 %d", limit)
	}
	rows, err := collectRegistryImages(ctx, addr, limit)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		// 对齐 docker images 空表头，便于脚本/肉眼对照。
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
		fmt.Fprintln(w, "REPOSITORY\tTAG\tIMAGE ID")
		_ = w.Flush()
		return nil
	}
	printImagesTable(rows)
	return nil
}

func collectRegistryImages(ctx context.Context, addr string, limit int) ([]imageRow, error) {
	repos, err := crane.Catalog(addr, crane.Insecure)
	if err != nil {
		return nil, fmt.Errorf("查询 registry %s 失败（serve 是否已启动？）: %w", addr, err)
	}
	sort.Strings(repos)

	var rows []imageRow
	for _, repo := range repos {
		if len(rows) >= limit {
			break
		}
		tags, err := crane.ListTags(addr+"/"+repo, crane.Insecure)
		if err != nil {
			fmt.Fprintf(os.Stderr, "列出 %s tags 失败: %v\n", repo, err)
			continue
		}
		sort.Strings(tags)
		for _, tag := range tags {
			if len(rows) >= limit {
				break
			}
			id := shortImageID(ctx, addr, repo, tag)
			rows = append(rows, imageRow{
				Repository: repo,
				Tag:        tag,
				ImageID:    id,
			})
		}
	}
	return rows, nil
}

func shortImageID(ctx context.Context, addr, repo, tag string) string {
	refStr := fmt.Sprintf("%s/%s:%s", addr, repo, tag)
	ref, err := name.ParseReference(refStr, name.Insecure)
	if err != nil {
		return "<none>"
	}
	desc, err := remote.Head(ref, remote.WithContext(ctx))
	if err != nil {
		return "<none>"
	}
	hex := desc.Digest.Hex
	if len(hex) > 12 {
		hex = hex[:12]
	}
	return hex
}

func printImagesTable(rows []imageRow) {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(w, "REPOSITORY\tTAG\tIMAGE ID")
	for _, r := range rows {
		fmt.Fprintf(w, "%s\t%s\t%s\n", r.Repository, r.Tag, r.ImageID)
	}
	_ = w.Flush()
}

func runLsTags(ctx context.Context, addr, repo string) error {
	repo = strings.TrimSpace(repo)
	repo = strings.TrimPrefix(repo, "/")
	if repo == "" {
		return fmt.Errorf("repository 不能为空")
	}
	// 允许传入 host/repo 或仅 repo。
	if i := strings.Index(repo, "/"); i > 0 {
		hostPart := repo[:i]
		if strings.Contains(hostPart, ".") || strings.Contains(hostPart, ":") || hostPart == "localhost" {
			repo = repo[i+1:]
		}
	}

	tags, err := crane.ListTags(addr+"/"+repo, crane.Insecure)
	if err != nil {
		return fmt.Errorf("列出 %s/%s tags 失败（仓库是否存在？serve 是否已启动？）: %w", addr, repo, err)
	}
	sort.Strings(tags)

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(w, "REPOSITORY\tTAG")
	if len(tags) == 0 {
		_ = w.Flush()
		return nil
	}
	for _, tag := range tags {
		fmt.Fprintf(w, "%s\t%s\n", repo, tag)
	}
	_ = w.Flush()
	return nil
}
