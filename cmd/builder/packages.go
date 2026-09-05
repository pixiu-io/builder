package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
)

const defaultPackagesLimit = 50

func newPackagesCmd() *cobra.Command {
	var (
		repoAddr string
		limit    int
		pkgType  string
	)
	cmd := &cobra.Command{
		Use:   "packages",
		Short: "列出 serve 软件源中的安装包（类似 images）",
		Long: `查询运行中的 serve HTTP 软件源，汇总 deb（/deb/Packages）与 rpm（/rpm/repodata/primary.xml.gz），
以表格打印 TYPE / NAME / VERSION / ARCH / SIZE。默认最多显示 50 条，可用 --limit 调整。`,
		Example: `  builder packages
  builder packages --repo-addr 192.168.1.10:8080 --limit 100
  builder packages --type deb`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPackages(context.Background(), repoAddr, limit, pkgType)
		},
	}
	cmd.Flags().StringVar(&repoAddr, "repo-addr", "127.0.0.1:8080", "serve 软件源地址（host:port 或 URL）")
	cmd.Flags().IntVar(&limit, "limit", defaultPackagesLimit, "最多显示的安装包条数")
	cmd.Flags().StringVar(&pkgType, "type", "all", "包类型过滤：all / deb / rpm")
	return cmd
}

type packageRow struct {
	Type    string
	Name    string
	Version string
	Arch    string
	Size    int64
}

func runPackages(ctx context.Context, repoAddr string, limit int, pkgType string) error {
	if limit <= 0 {
		return fmt.Errorf("--limit 必须为正整数，当前 %d", limit)
	}
	pkgType = strings.ToLower(strings.TrimSpace(pkgType))
	switch pkgType {
	case "all", "deb", "rpm":
	default:
		return fmt.Errorf("--type 仅支持 all / deb / rpm，当前 %q", pkgType)
	}

	base := normalizeRepoBaseURL(repoAddr)
	rows, errs := collectServePackages(ctx, base, pkgType)
	if len(rows) == 0 {
		if len(errs) > 0 {
			return fmt.Errorf("查询软件源 %s 失败（serve 是否已启动且含软件包？）:\n  %s", base, strings.Join(errs, "\n  "))
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
		fmt.Fprintln(w, "TYPE\tNAME\tVERSION\tARCH\tSIZE")
		_ = w.Flush()
		return nil
	}

	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Type != rows[j].Type {
			return rows[i].Type < rows[j].Type
		}
		if rows[i].Name != rows[j].Name {
			return rows[i].Name < rows[j].Name
		}
		return rows[i].Version < rows[j].Version
	})
	if len(rows) > limit {
		rows = rows[:limit]
	}
	printPackagesTable(rows)
	for _, e := range errs {
		fmt.Fprintf(os.Stderr, "warning: %s\n", e)
	}
	return nil
}

func normalizeRepoBaseURL(addr string) string {
	addr = strings.TrimSpace(addr)
	addr = strings.TrimRight(addr, "/")
	if addr == "" {
		return "http://127.0.0.1:8080"
	}
	if strings.HasPrefix(addr, "http://") || strings.HasPrefix(addr, "https://") {
		return addr
	}
	return "http://" + addr
}

func collectServePackages(ctx context.Context, base, pkgType string) ([]packageRow, []string) {
	var rows []packageRow
	var errs []string
	if pkgType == "all" || pkgType == "deb" {
		debRows, err := fetchDebPackages(ctx, base+"/deb/Packages")
		if err != nil {
			errs = append(errs, "deb: "+err.Error())
		} else {
			rows = append(rows, debRows...)
		}
	}
	if pkgType == "all" || pkgType == "rpm" {
		rpmRows, err := fetchRPMPackages(ctx, base+"/rpm/repodata/primary.xml.gz")
		if err != nil {
			errs = append(errs, "rpm: "+err.Error())
		} else {
			rows = append(rows, rpmRows...)
		}
	}
	return rows, errs
}

func fetchDebPackages(ctx context.Context, url string) ([]packageRow, error) {
	body, err := httpGetBody(ctx, url)
	if err != nil {
		return nil, err
	}
	return parseDebPackagesIndex(string(body))
}

func parseDebPackagesIndex(content string) ([]packageRow, error) {
	var rows []packageRow
	for _, st := range strings.Split(content, "\n\n") {
		st = strings.TrimSpace(st)
		if st == "" {
			continue
		}
		fields := map[string]string{}
		sc := bufio.NewScanner(strings.NewReader(st))
		for sc.Scan() {
			line := sc.Text()
			if line == "" || strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
				continue
			}
			i := strings.IndexByte(line, ':')
			if i <= 0 {
				continue
			}
			fields[strings.TrimSpace(line[:i])] = strings.TrimSpace(line[i+1:])
		}
		name := fields["Package"]
		if name == "" {
			continue
		}
		size, _ := strconv.ParseInt(fields["Size"], 10, 64)
		rows = append(rows, packageRow{
			Type:    "deb",
			Name:    name,
			Version: fields["Version"],
			Arch:    fields["Architecture"],
			Size:    size,
		})
	}
	return rows, nil
}

func fetchRPMPackages(ctx context.Context, url string) ([]packageRow, error) {
	body, err := httpGetBody(ctx, url)
	if err != nil {
		return nil, err
	}
	return parseRPMPrimaryXMLGzip(body)
}

func parseRPMPrimaryXMLGzip(gzData []byte) ([]packageRow, error) {
	gr, err := gzip.NewReader(bytes.NewReader(gzData))
	if err != nil {
		return nil, fmt.Errorf("解压 primary.xml.gz 失败: %w", err)
	}
	defer gr.Close()
	return parseRPMPrimaryXML(gr)
}

type rpmPrimaryMeta struct {
	Packages []rpmPrimaryPkg `xml:"package"`
}

type rpmPrimaryPkg struct {
	Name    string `xml:"name"`
	Arch    string `xml:"arch"`
	Version struct {
		Ver string `xml:"ver,attr"`
		Rel string `xml:"rel,attr"`
	} `xml:"version"`
	Size struct {
		Package int64 `xml:"package,attr"`
	} `xml:"size"`
}

func parseRPMPrimaryXML(r io.Reader) ([]packageRow, error) {
	var meta rpmPrimaryMeta
	if err := xml.NewDecoder(r).Decode(&meta); err != nil {
		return nil, fmt.Errorf("解析 primary.xml 失败: %w", err)
	}
	rows := make([]packageRow, 0, len(meta.Packages))
	for _, p := range meta.Packages {
		ver := p.Version.Ver
		if p.Version.Rel != "" {
			ver = p.Version.Ver + "-" + p.Version.Rel
		}
		rows = append(rows, packageRow{
			Type:    "rpm",
			Name:    p.Name,
			Version: ver,
			Arch:    p.Arch,
			Size:    p.Size.Package,
		})
	}
	return rows, nil
}

func httpGetBody(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

func printPackagesTable(rows []packageRow) {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(w, "TYPE\tNAME\tVERSION\tARCH\tSIZE")
	for _, r := range rows {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", r.Type, r.Name, r.Version, r.Arch, formatByteSize(r.Size))
	}
	_ = w.Flush()
}

func formatByteSize(n int64) string {
	if n <= 0 {
		return "-"
	}
	const (
		kb = 1024
		mb = 1024 * kb
		gb = 1024 * mb
	)
	switch {
	case n >= gb:
		return fmt.Sprintf("%.1fGB", float64(n)/float64(gb))
	case n >= mb:
		return fmt.Sprintf("%.1fMB", float64(n)/float64(mb))
	case n >= kb:
		return fmt.Sprintf("%.1fKB", float64(n)/float64(kb))
	default:
		return fmt.Sprintf("%dB", n)
	}
}
