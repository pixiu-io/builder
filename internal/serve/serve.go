// Package serve 将 builder 离线产物加载为本地 OCI registry + yum/dnf/apt 软件源。
package serve

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// Options 控制 serve 行为。
type Options struct {
	// Bundles 离线包路径（目录或 .tar.gz），可多个（packages + images）。
	Bundles []string
	// DataDir 工作目录（解压、repodata、registry blob）。
	DataDir string
	// RegistryAddr registry 监听地址，默认 0.0.0.0:5000。
	RegistryAddr string
	// RepoAddr 软件源 HTTP 监听地址，默认 0.0.0.0:8080。
	RepoAddr string
	// AdvertiseHost 打印给客户端的主机名/IP（不含端口），默认 127.0.0.1。
	AdvertiseHost string
	// SkipImages 不启动 registry / 不导入镜像。
	SkipImages bool
	// SkipPackages 不生成 / 不提供软件源。
	SkipPackages bool
}

// Result 启动后的对外信息。
type Result struct {
	DataDir         string
	RegistryURL     string
	RepoURL         string
	Images          []string
	RPMPackages     int
	DebPackages     int
	RegistryEnabled bool
	RepoEnabled     bool
}

// Run 加载产物、准备源、监听直到 ctx 取消。
func Run(ctx context.Context, opts Options) (*Result, error) {
	opts = normalize(opts)
	if len(opts.Bundles) == 0 {
		return nil, fmt.Errorf("请通过 --bundle 指定至少一个离线包目录或 tar.gz")
	}
	if opts.SkipImages && opts.SkipPackages {
		return nil, fmt.Errorf("--skip-images 与 --skip-packages 不能同时设置")
	}

	if err := os.MkdirAll(opts.DataDir, 0o755); err != nil {
		return nil, fmt.Errorf("创建 data-dir 失败: %w", err)
	}

	fmt.Printf("加载离线产物到 %s ...\n", opts.DataDir)
	roots, err := loadBundles(opts.Bundles, filepath.Join(opts.DataDir, "bundles"))
	if err != nil {
		return nil, err
	}
	fmt.Printf("  已加载 %d 个 bundle\n", len(roots))

	res := &Result{DataDir: opts.DataDir}
	var servers []*http.Server
	var listeners []net.Listener

	cleanup := func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		for _, s := range servers {
			_ = s.Shutdown(shutdownCtx)
		}
		for _, ln := range listeners {
			_ = ln.Close()
		}
	}

	if !opts.SkipPackages {
		pkgRoot := filepath.Join(opts.DataDir, "repos")
		rpmN, debN, err := prepareRepos(roots, pkgRoot)
		if err != nil {
			return nil, err
		}
		res.RPMPackages = rpmN
		res.DebPackages = debN
		res.RepoEnabled = rpmN > 0 || debN > 0

		if res.RepoEnabled {
			ln, err := net.Listen("tcp", opts.RepoAddr)
			if err != nil {
				return nil, fmt.Errorf("监听软件源 %s 失败: %w", opts.RepoAddr, err)
			}
			listeners = append(listeners, ln)
			repoPort := ln.Addr().(*net.TCPAddr).Port
			res.RepoURL = fmt.Sprintf("http://%s:%d", opts.AdvertiseHost, repoPort)

			mux := http.NewServeMux()
			mux.Handle("/", http.FileServer(http.Dir(pkgRoot)))
			srv := &http.Server{Handler: mux}
			servers = append(servers, srv)
			go func() {
				if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
					fmt.Fprintf(os.Stderr, "软件源服务异常: %v\n", err)
				}
			}()
			fmt.Printf("软件源已启动: %s （rpm=%d deb=%d）\n", res.RepoURL, rpmN, debN)
		}
	}

	if !opts.SkipImages {
		blobDir := filepath.Join(opts.DataDir, "registry")
		if err := os.MkdirAll(blobDir, 0o755); err != nil {
			cleanup()
			return nil, err
		}
		ln, err := net.Listen("tcp", opts.RegistryAddr)
		if err != nil {
			cleanup()
			return nil, fmt.Errorf("监听 registry %s 失败: %w", opts.RegistryAddr, err)
		}
		listeners = append(listeners, ln)
		regPort := ln.Addr().(*net.TCPAddr).Port
		pushHost := fmt.Sprintf("127.0.0.1:%d", regPort)
		res.RegistryURL = fmt.Sprintf("%s:%d", opts.AdvertiseHost, regPort)
		res.RegistryEnabled = true

		h := newRegistryHandler(blobDir)
		srv := &http.Server{Handler: h}
		servers = append(servers, srv)
		go func() {
			if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
				fmt.Fprintf(os.Stderr, "registry 服务异常: %v\n", err)
			}
		}()

		if err := waitHTTP(ctx, "http://"+pushHost+"/v2/"); err != nil {
			cleanup()
			return nil, fmt.Errorf("registry 未就绪: %w", err)
		}

		fmt.Printf("导入镜像到 registry %s ...\n", res.RegistryURL)
		imgs, err := importImages(ctx, roots, pushHost, res.RegistryURL)
		if err != nil {
			cleanup()
			return nil, err
		}
		res.Images = imgs
		fmt.Printf("  已导入 %d 个镜像\n", len(imgs))
	}

	if !res.RegistryEnabled && !res.RepoEnabled {
		return nil, fmt.Errorf("bundle 中既无镜像也无软件包，无可提供的服务")
	}

	printReady(res)
	<-ctx.Done()
	fmt.Println("正在停止 serve ...")
	cleanup()
	return res, nil
}

func normalize(opts Options) Options {
	if opts.DataDir == "" {
		opts.DataDir = "./serve-data"
	}
	if opts.RegistryAddr == "" {
		opts.RegistryAddr = "0.0.0.0:5000"
	}
	if opts.RepoAddr == "" {
		opts.RepoAddr = "0.0.0.0:8080"
	}
	if opts.AdvertiseHost == "" {
		opts.AdvertiseHost = "127.0.0.1"
	}
	return opts
}

func waitHTTP(ctx context.Context, url string) error {
	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(15 * time.Second)
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		resp, err := client.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode < 500 {
				return nil
			}
		}
		if time.Now().After(deadline) {
			if err != nil {
				return err
			}
			return fmt.Errorf("超时")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func printReady(res *Result) {
	fmt.Println()
	fmt.Println("========== builder serve 就绪 ==========")
	if res.RegistryEnabled {
		fmt.Printf("Registry:  %s\n", res.RegistryURL)
		fmt.Println("  示例:")
		if len(res.Images) > 0 {
			fmt.Printf("    docker pull %s\n", res.Images[0])
			fmt.Printf("    kubeadm init --image-repository %s ...\n", res.RegistryURL)
		} else {
			fmt.Printf("    docker pull %s/<name>:<tag>\n", res.RegistryURL)
		}
		fmt.Printf("  Docker insecure-registries 需包含: %q\n", res.RegistryURL)
	}
	if res.RepoEnabled {
		fmt.Printf("Packages:  %s\n", res.RepoURL)
		if res.RPMPackages > 0 {
			fmt.Printf("  dnf:\n")
			fmt.Printf("    dnf install --repofrompath=pixiu,%s/rpm kubeadm\n", res.RepoURL)
			fmt.Printf("  或写入 /etc/yum.repos.d/pixiu-offline.repo:\n")
			fmt.Printf("    [pixiu-offline]\n    name=Pixiu Offline\n    baseurl=%s/rpm\n    enabled=1\n    gpgcheck=0\n", res.RepoURL)
		}
		if res.DebPackages > 0 {
			fmt.Printf("  apt:\n")
			fmt.Printf("    echo 'deb [trusted=yes] %s/deb ./' > /etc/apt/sources.list.d/pixiu-offline.list\n", res.RepoURL)
			fmt.Printf("    apt-get update && apt-get install kubeadm\n")
		}
	}
	fmt.Println("按 Ctrl+C 停止")
	fmt.Println("========================================")
}