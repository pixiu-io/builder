// Package serve 将 builder 离线产物加载为本地 OCI registry + yum/dnf/apt 软件源。
package serve

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Options 控制 serve 行为。
type Options struct {
	// Bundles 离线包路径（目录或 .tar.gz），可多个。
	// 支持 builder 产物（含 manifest.yaml）与单镜像 docker save 的 .tar.gz，可混放。
	Bundles []string
	// Dir 离线包目录：启动时加载其下所有 *.tar.gz（builder 包与单镜像 save 可混放），并轮询热加载。
	Dir string
	// DataDir 工作目录（解压、repodata、registry blob）。
	DataDir string
	// RegistryAddr registry 监听地址，默认 0.0.0.0:5000。
	RegistryAddr string
	// RegistryPort 覆盖 RegistryAddr 的端口（host 不变，为空表示不覆盖）。
	RegistryPort string
	// RepoAddr 软件源 HTTP 监听地址，默认 0.0.0.0:8080。
	RepoAddr string
	// RepoPort 覆盖 RepoAddr 的端口（host 不变，为空表示不覆盖）。
	RepoPort string
	// AdvertiseHost 打印给客户端的主机名/IP（不含端口），默认 127.0.0.1。
	AdvertiseHost string
	// Namespace registry 发布命名空间（自动导入镜像的引用前缀），默认 pixiu。
	// 用户直接 push 到 serve registry 的镜像不受影响。
	Namespace string
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
	// ImageRepository kubeadm init --image-repository 使用的镜像仓库前缀
	//（RegistryURL + "/" + Namespace，如 192.168.1.10:5000/pixiu）。
	ImageRepository string
}

// Run 加载产物、准备源、监听直到 ctx 取消。
func Run(ctx context.Context, opts Options) (*Result, error) {
	opts, err := normalize(opts)
	if err != nil {
		return nil, err
	}
	if len(opts.Bundles) == 0 && opts.Dir == "" {
		return nil, fmt.Errorf("请通过 --bundle 指定离线包，或 --dir 指定离线包目录")
	}
	if opts.SkipImages && opts.SkipPackages {
		return nil, fmt.Errorf("--skip-images 与 --skip-packages 不能同时设置")
	}

	if err := os.MkdirAll(opts.DataDir, 0o755); err != nil {
		return nil, fmt.Errorf("创建 data-dir 失败: %w", err)
	}

	// 初始 bundle：--bundle 与 --dir 下所有 *.tar.gz 合并。
	initial := append([]string{}, opts.Bundles...)
	if opts.Dir != "" {
		// --dir 不存在时自动创建，便于空目录启动后热加载。
		if err := os.MkdirAll(opts.Dir, 0o755); err != nil {
			return nil, fmt.Errorf("创建离线包目录 %s 失败: %w", opts.Dir, err)
		}
		dirBundles, err := scanTarGz(opts.Dir)
		if err != nil {
			return nil, fmt.Errorf("扫描离线包目录 %s 失败: %w", opts.Dir, err)
		}
		initial = append(initial, dirBundles...)
	}

	loaded := make(map[string]bool, len(initial))
	for _, b := range initial {
		loaded[b] = true
	}

	fmt.Printf("加载离线产物到 %s ...\n", opts.DataDir)
	bundleRoot := filepath.Join(opts.DataDir, "bundles")
	if err := os.MkdirAll(bundleRoot, 0o755); err != nil {
		return nil, fmt.Errorf("创建 bundle 目录失败: %w", err)
	}
	// 逐个解压：单个 bundle 失败（如文件不完整/损坏）跳过，不阻塞 serve 启动，
	// 交由热加载在文件完整后自动重试。
	var roots []string
	for i, b := range initial {
		root, err := resolveBundle(b, filepath.Join(bundleRoot, fmt.Sprintf("b%d", i)))
		if err != nil {
			fmt.Fprintf(os.Stderr, "跳过 %s（加载失败: %v），文件完整后将自动热加载\n", b, err)
			delete(loaded, b)
			continue
		}
		roots = append(roots, root)
	}
	fmt.Printf("  已加载 %d 个 bundle（跳过 %d 个）\n", len(roots), len(initial)-len(roots))

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

	pkgRoot := filepath.Join(opts.DataDir, "repos")
	blobDir := filepath.Join(opts.DataDir, "registry")

	// allRoots 维护已加载 bundle 根目录（热加载时追加，闭包内以 mutex 保护）。
	var mu sync.Mutex
	allRoots := roots

	// reloadPackages 全量重建软件源（含历史与新加载的包）。
	reloadPackages := func() error {
		if opts.SkipPackages {
			return nil
		}
		mu.Lock()
		rpmN, debN, err := prepareRepos(allRoots, pkgRoot)
		mu.Unlock()
		if err != nil {
			return err
		}
		res.RPMPackages = rpmN
		res.DebPackages = debN
		res.RepoEnabled = rpmN > 0 || debN > 0
		return nil
	}

	if !opts.SkipPackages {
		if err := reloadPackages(); err != nil {
			return nil, err
		}
		// 热加载模式（--dir）下即使初始无包也启动源服务器，等待后续热加载。
		if res.RepoEnabled || opts.Dir != "" {
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
			fmt.Printf("软件源已启动: %s （rpm=%d deb=%d）\n", res.RepoURL, res.RPMPackages, res.DebPackages)
		}
	}

	var pushHost string
	if !opts.SkipImages {
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
		pushHost = fmt.Sprintf("127.0.0.1:%d", regPort)
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

		res.ImageRepository = res.RegistryURL + "/" + opts.Namespace
		fmt.Printf("导入镜像到 registry %s ...\n", res.RegistryURL)
		imgs, err := importImages(ctx, allRoots, pushHost, res.RegistryURL, opts.Namespace)
		if err != nil {
			cleanup()
			return nil, err
		}
		res.Images = imgs
		fmt.Printf("  已导入 %d 个镜像\n", len(imgs))
	}

	if !res.RegistryEnabled && !res.RepoEnabled && opts.Dir == "" {
		return nil, fmt.Errorf("bundle 中既无镜像也无软件包，无可提供的服务")
	}

	printReady(res)

	// 热加载：轮询 --dir 目录，发现新 *.tar.gz 自动加载（解压 → 推镜像 → 重建软件源）。
	if opts.Dir != "" {
		go watchDir(ctx, opts.Dir, 3*time.Second, func(newBundles []string) bool {
			return hotLoad(ctx, newBundles, bundleRoot, loaded, &allRoots, &mu, pushHost, res, reloadPackages, opts)
		})
	}

	<-ctx.Done()
	fmt.Println("正在停止 serve ...")
	cleanup()
	return res, nil
}

func normalize(opts Options) (Options, error) {
	// 纯端口 flag 优先合并：只替换地址端口，host 不变。
	if opts.RegistryPort != "" {
		a, err := applyPort(opts.RegistryAddr, opts.RegistryPort)
		if err != nil {
			return opts, err
		}
		opts.RegistryAddr = a
	}
	if opts.RepoPort != "" {
		a, err := applyPort(opts.RepoAddr, opts.RepoPort)
		if err != nil {
			return opts, err
		}
		opts.RepoAddr = a
	}
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
		opts.AdvertiseHost = LocalIP()
	}
	opts.Namespace = sanitizeNamespace(opts.Namespace)
	return opts, nil
}

// applyPort 用 port 覆盖 addr 的端口：成功解析时取 addr 的 host 部分，
// addr 无端口（SplitHostPort 失败）则整段视为 host；addr 为空时 host 为空串。
func applyPort(addr, port string) (string, error) {
	p, err := strconv.Atoi(port)
	if err != nil {
		return "", fmt.Errorf("端口 %q 非法: %w", port, err)
	}
	if p < 0 || p > 65535 {
		return "", fmt.Errorf("端口 %d 超出范围 0-65535", p)
	}
	var host string
	if addr != "" {
		if h, _, err := net.SplitHostPort(addr); err == nil {
			host = h
		} else {
			host = addr
		}
	}
	return net.JoinHostPort(host, port), nil
}

// sanitizeNamespace 清洗 registry 发布命名空间：去首尾空白与斜杠，
// 清洗为空时回退默认 pixiu。单级 namespace，含非法字符（含 /）时按
// registry repo 片段规则替换为 -。
func sanitizeNamespace(ns string) string {
	ns = strings.TrimSpace(ns)
	ns = strings.Trim(ns, "/")
	if ns == "" {
		return "pixiu"
	}
	return sanitizeRepoName(ns)
}

// LocalIP 返回本机非 loopback 的 IPv4 地址（serve --advertise-host 默认值）。
func LocalIP() string {
	// 优先取出口 IP（UDP 只做路由选择，不真正发包）。
	if conn, err := net.Dial("udp", "8.8.8.8:80"); err == nil {
		defer conn.Close()
		if addr, ok := conn.LocalAddr().(*net.UDPAddr); ok && addr.IP.To4() != nil {
			return addr.IP.String()
		}
	}
	// 兜底：遍历接口找第一个非 loopback IPv4。
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs {
			if ipnet, ok := a.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
				if ip4 := ipnet.IP.To4(); ip4 != nil {
					return ip4.String()
				}
			}
		}
	}
	return "127.0.0.1"
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
	printReadyTo(os.Stdout, res)
}

func printReadyTo(w io.Writer, res *Result) {
	fmt.Fprintln(w)
	fmt.Fprintln(w, "========== builder serve 就绪 ==========")
	if res.RegistryEnabled {
		fmt.Fprintf(w, "Registry:  %s（已导入 %d 个镜像）\n", res.RegistryURL, len(res.Images))
	}
	if res.RepoEnabled {
		fmt.Fprintf(w, "Packages:  %s（rpm=%d deb=%d）\n", res.RepoURL, res.RPMPackages, res.DebPackages)
	}
	printConfigDemo(w, res)
	fmt.Fprintln(w, "========================================")
}

// printConfigDemo 在离线包加载完成后输出镜像与自定义安装包的配置示例。
func printConfigDemo(w io.Writer, res *Result) {
	fmt.Fprintln(w)
	fmt.Fprintln(w, "---------- 镜像配置 demo ----------")
	if res.RegistryEnabled {
		pullRef := res.RegistryURL + "/<name>:<tag>"
		if len(res.Images) > 0 {
			pullRef = res.Images[0]
		}
		fmt.Fprintln(w, "# 1) 节点 Docker / containerd 将 registry 加入 insecure-registries:")
		fmt.Fprintf(w, "#    %q\n", res.RegistryURL)
		fmt.Fprintln(w, "# 2) 拉取 / 初始化:")
		fmt.Fprintf(w, "docker pull %s\n", pullRef)
		repo := res.ImageRepository
		if repo == "" {
			repo = res.RegistryURL
		}
		fmt.Fprintf(w, "kubeadm init --image-repository %s ...\n", repo)
	} else {
		fmt.Fprintln(w, "# 当前未启用 registry（--skip-images 或产物无镜像）")
	}
	fmt.Fprintln(w, "# 3) 构建时自定义附加镜像（builder.yaml）:")
	fmt.Fprintln(w, `addon_images:
  - name: my-addon
    image: "example.com/my-org/my-addon"
    tag: "v1.0.0"`)

	fmt.Fprintln(w)
	fmt.Fprintln(w, "---------- 自定义安装包配置 demo ----------")
	if res.RepoEnabled {
		fmt.Fprintln(w, "# 1) 节点使用离线软件源安装（按系统选择其一）:")
		fmt.Fprintln(w, "# dnf / yum:")
		fmt.Fprintf(w, "dnf install --repofrompath=pixiu,%s/rpm kubeadm\n", res.RepoURL)
		fmt.Fprintln(w, "# 或写入 /etc/yum.repos.d/pixiu.repo:")
		fmt.Fprintf(w, "[pixiu]\nname=Pixiu\nbaseurl=%s/rpm\nenabled=1\ngpgcheck=0\n", res.RepoURL)
		fmt.Fprintln(w, "# apt:")
		fmt.Fprintf(w, "echo 'deb [trusted=yes] %s/deb ./' > /etc/apt/sources.list.d/pixiu.list\n", res.RepoURL)
		fmt.Fprintln(w, "apt-get update && apt-get install kubeadm")
	} else {
		fmt.Fprintln(w, "# 当前未启用软件源（--skip-packages 或产物无软件包）")
	}
	fmt.Fprintln(w, "# 2) 构建时自定义附加安装包（builder.yaml）:")
	fmt.Fprintln(w, `addon_packages:
  - name: conntrack
  - name: vim
    version: "9.0"   # 可选；空则不锁版本`)
}

// scanTarGz 扫描目录下所有 *.tar.gz 文件（顶层）。
func scanTarGz(dir string) ([]string, error) {
	matches, err := filepath.Glob(filepath.Join(dir, "*.tar.gz"))
	if err != nil {
		return nil, err
	}
	sort.Strings(matches)
	return matches, nil
}

// watchDir 轮询监控目录，每 interval 扫描一次。新出现的 *.tar.gz 需连续两帧
// 大小一致（视为拷贝完成）才会交给 handler；handler 返回 false（加载失败，如
// 文件仍在写入导致解压 unexpected EOF）时重置稳定态，下一轮重试。
func watchDir(ctx context.Context, dir string, interval time.Duration, handler func(bundles []string) bool) {
	type entry struct {
		size   int64
		stable bool
	}
	seen := map[string]*entry{}
	done := map[string]bool{}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			bundles, err := scanTarGz(dir)
			if err != nil {
				fmt.Fprintf(os.Stderr, "扫描目录 %s 失败: %v\n", dir, err)
				continue
			}
			var news []string
			for _, b := range bundles {
				if done[b] {
					continue
				}
				st, err := os.Stat(b)
				if err != nil {
					continue
				}
				e := seen[b]
				if e == nil {
					// 首次见到：记录大小，等待后续帧确认拷贝完成。
					seen[b] = &entry{size: st.Size()}
					continue
				}
				if st.Size() != e.size {
					// 大小仍在变化：文件还在写入，更新大小。
					e.size = st.Size()
					e.stable = false
					continue
				}
				if !e.stable {
					// 大小稳定一帧。
					e.stable = true
					continue
				}
				// 连续两帧大小一致：视为拷贝完成，交给 handler。
				news = append(news, b)
			}
			if len(news) > 0 {
				if handler(news) {
					for _, b := range news {
						done[b] = true
					}
				} else {
					// 加载失败：重置稳定态，允许下轮重试。
					for _, b := range news {
						if e := seen[b]; e != nil {
							e.stable = false
						}
					}
				}
			}
		}
	}
}

// hotLoad 热加载新出现的离线包：解压 → 推镜像 → 重建软件源。
// 返回是否全部解压成功；任一解压失败返回 false（watchDir 会重置后重试）。
func hotLoad(ctx context.Context, newBundles []string, bundleRoot string, loaded map[string]bool, allRoots *[]string, mu *sync.Mutex, pushHost string, res *Result, reloadPackages func() error, opts Options) bool {
	success := true
	for _, b := range newBundles {
		mu.Lock()
		if loaded[b] {
			mu.Unlock()
			continue
		}
		loaded[b] = true
		mu.Unlock()

		fmt.Printf("检测到新离线包 %s，正在加载 ...\n", b)
		root, err := resolveBundle(b, filepath.Join(bundleRoot, "hot", filepath.Base(b)))
		if err != nil {
			fmt.Fprintf(os.Stderr, "加载 %s 失败: %v\n", b, err)
			mu.Lock()
			delete(loaded, b) // 失败后允许重试（如文件仍在拷贝）
			mu.Unlock()
			success = false
			continue
		}

		mu.Lock()
		*allRoots = append(*allRoots, root)
		mu.Unlock()

		if !opts.SkipImages {
			imgs, err := importImages(ctx, []string{root}, pushHost, res.RegistryURL, opts.Namespace)
			if err != nil {
				fmt.Fprintf(os.Stderr, "导入 %s 镜像失败: %v\n", b, err)
			} else {
				res.Images = append(res.Images, imgs...)
				fmt.Printf("  已导入 %d 个镜像\n", len(imgs))
			}
		}
		if !opts.SkipPackages {
			if err := reloadPackages(); err != nil {
				fmt.Fprintf(os.Stderr, "重建软件源失败: %v\n", err)
			} else {
				fmt.Printf("  软件源已刷新（rpm=%d deb=%d）\n", res.RPMPackages, res.DebPackages)
			}
		}
		fmt.Printf("热加载完成: %s\n", b)
	}
	if success {
		// 热加载成功后再次给出配置 demo（空目录启动后首次有包时尤其有用）。
		printReady(res)
	}
	return success
}
