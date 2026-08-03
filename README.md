# builder

制作 Kubernetes 离线安装包（apt/dnf 软件包 + 运行时 + 镜像）的 Go CLI 工具。

生成的离线包包含：
- **k8s 软件包**：kubeadm / kubelet / kubectl（容器内通过 apt/dnf 递归下载，依赖闭包交给包管理器）
- **容器运行时软件包**：containerd.io / cri-tools（runc 由 containerd.io 包内嵌提供，不单独安装；containerd 源 + 系统源）
- **系统依赖软件包**：conntrack、ipvsadm、nfs-common(-utils)、socat、ebtables、chrony
- **镜像**：核心镜像（容器内安装 kubeadm 后生成清单）+ 附加组件镜像（builder.yaml addons 节）
- **安装脚本**：install.sh（包安装 + preflight）/ load-images.sh
- **完整性清单**：manifest.yaml（可校验）

> 软件源说明：k8s 组件来自官方 `pkgs.k8s.io`（仓库由 k8s 版本自动推导，v1.27.3 → v1.27）；
> containerd.io 来自 docker 官方源 `download.docker.com`。原 `packages.containerd.io` 域名在当前网络与公共 DNS（223.5.5.5 / 8.8.8.8）均 NXDOMAIN 无法解析，已改用 docker 官方源提供 containerd.io 包。
> 该源的 containerd.io 包内嵌 runc 二进制，故不再单独安装 runc 包，避免与独立 runc 包的 `Conflicts: runc` 冲突导致 apt 无法同时解析。

## 快速开始

```bash
# 构建 CLI
go build -o builder ./cmd/builder

# 查看支持的操作系统 / k8s 版本（均为参考列表；build 支持任意 OS/版本与任意 vX.Y.Z）
./builder list-os
./builder list-k8s

# 查看附加组件镜像
./builder list-images --os ubuntu --kubernetes-version v1.27.3

# 构建离线包（dry-run 演练，不执行真实下载/拉取）
./builder build --os ubuntu --os-version 22.04 --kubernetes-version v1.27.3 --arch amd64 --dry-run

# 真实构建（需联网 + 本机 docker；docker 不可用时构建直接中断，不产出离线包）
./builder build --os ubuntu --os-version 22.04 --kubernetes-version v1.27.3 --arch amd64 --out ./dist

# 仅镜像（可不指定 --os / --os-version）
./builder build --mode images --kubernetes-version v1.27.3 --arch amd64 --out ./dist

# 校验离线包（目录或 tar.gz）
./builder verify --bundle ./dist/pixiu-offline-ubuntu-22.04-amd64-v1.27.3.tar.gz
```

`build` 执行时按 5 步管线实时输出 `[builder]` 前缀日志（步骤开始 / 完成 / 跳过 / 失败，dry-run 额外标注），便于跟踪长耗时构建；结束后打印构建步骤汇总表。

## 命令一览

| 命令 | 说明 |
|------|------|
| `build` | 构建离线安装包。`--kubernetes-version` 必填（任意 vX.Y.Z）。`--os` / `--os-version` 在 `--mode all` 或 `packages` 时必填（**任意**取值，不必在 list-os 中；未登记时构建镜像默认为 `{os}:{os-version}`）；`--mode images` 时可省略。可选：`--arch`（amd64/arm64，默认 amd64）、`--mirror`、`--mode`、`--workdir`、`--out`、`--skip-images`、`--skip-addons`、`--dry-run`、`--upload`、`--s3-bucket` 等 |
| `upload` | 将已有产物 tar.gz 上传到 S3/MinIO。`--file` 可重复；bucket/prefix/endpoint/region 可覆盖配置文件 `s3` 节 |
| `list-os` | 列出参考 OS / 版本（builder.yaml；实际 build 不限于此列表） |
| `list-k8s` | 列出支持的 k8s 版本（含记录用运行时版本） |
| `list-images` | 列出附加组件镜像清单。纯 flag 指定：`--os`（必填）、`--kubernetes-version`（必填）、`--arch`（默认 amd64） |
| `verify` | 校验 bundle（目录或 tar.gz）完整性 |
| `version` | 打印版本 |

## 构建模式（`--mode`）

`build` 按 `--mode` 决定构建内容（默认 `all`），适合按需产出的场景：

| 模式 | 构建内容 | 适用场景 |
|------|---------|---------|
| `all`（默认） | 软件包 + 镜像 | 完整离线包，一次性交付 |
| `packages` | 仅软件包（k8s/运行时/系统依赖）+ 脚本 + manifest | 只更新软件包、或本机无 docker / 网络不支持镜像拉取时 |
| `images` | 仅镜像（核心 + addons）+ 脚本 + manifest | 软件包已就绪，只补充镜像；不依赖容器内包下载 |

`--mode packages` 等价旧 `--skip-images`（兼容保留）；同时传 `--mode` 与 `--skip-images` 时以 `--mode` 确定的内容为准（`--mode all` 配合 `--skip-images` 视为 `packages`）。被跳过的步骤在构建步骤汇总中标记为 `skipped`（非失败），Step 3/4/5（脚本/manifest/打包）始终执行，产物目录结构完整。

`--mode all` 时步骤 1（软件包，`builder-packages-*`）与步骤 2（镜像，`builder-images-*`）**并行**执行；仅跑其中一侧（`packages` / `images`）或 `--dry-run` 时仍串行。

```bash
# 仅软件包（等价 --skip-images）
./builder build --os ubuntu --os-version 22.04 --kubernetes-version v1.27.3 --arch amd64 --mode packages

# 仅镜像（可省略 --os / --os-version；产物名为 pixiu-offline-images-{arch}-{k8sver}）
./builder build --mode images --kubernetes-version v1.27.3 --arch amd64

# 仅镜像且仍绑定发行版（产物名仍含 os/osver）
./builder build --os ubuntu --os-version 22.04 --kubernetes-version v1.27.3 --arch amd64 --mode images
```

## 产物目录结构

```
pixiu-offline-{os}-{osver}-{arch}-{k8sver}/   # 完整包 / 指定 OS 的镜像包
pixiu-offline-images-{arch}-{k8sver}/         # --mode images 且未指定 OS
├── packages/
│   ├── *.deb / *.rpm    # k8s + 运行时 + 系统依赖（容器内 apt/dnf 递归下载）
│   └── runtime/         # crictl 静态 tar（仅 cri-tools 包源不可用时的回退产物）
├── images/
│   ├── core/            # 核心镜像 tar（kube-apiserver.tar 等）
│   └── addons/          # 附加组件镜像 tar（flannel.tar 等）
├── install/
│   ├── install.sh       # 目标机一键安装脚本（包安装 + preflight）
│   └── load-images.sh   # 镜像导入（docker load → ctr import 回退）
└── manifest.yaml        # 完整性清单（path/size/sha256）
```

构建完成后会在 `--out` 目录生成同名 `.tar.gz`。`--mode all` 时拆成两个独立产物：
- `pixiu-offline-{os}-{osver}-{arch}-{k8sver}-packages.tar.gz`
- `pixiu-offline-{os}-{osver}-{arch}-{k8sver}-images.tar.gz`

## 配置文件

单份 yaml 默认位于 `/etc/pixiu/builder.yaml`（生产部署，与 pixiu 配置惯例一致），可通过 `--configFile` 或环境变量 `BUILDER_CONFIG_FILE` 指定配置文件。本地开发使用仓库根目录下的 `builder.yaml`：`--configFile ./builder.yaml` 或 `export BUILDER_CONFIG_FILE=./builder.yaml`。文件按顶层分为三节：

| 节 | 内容 |
|------|------|
| `oses` | OS 注册表：可用版本、包管理器（apt/dnf）、容器内下载软件包用的构建镜像、架构、apt 版本代号（codename/codenames）、dnf 发行版标识（rpm_distro） |
| `versions` | k8s 版本定义；containerd/runc 为记录用，crictl 用于 cri-tools 包缺失时的静态回退 |
| `addons` | 附加组件镜像清单（name → image:tag） |
| `s3` | 可选：产物上传到 S3/MinIO（bucket/region/endpoint/prefix）；凭证用环境变量，不写密钥 |

> `versions` / `build_images` 为初始值，加载器不验证镜像可用性，生产使用请按需核对。

## 上传到 S3

构建完成后可用 `--upload` 自动上传产物，或用 `upload` 子命令上传已有 tar.gz。

凭证（任选其一）：
- 环境变量 `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY`（可选 `AWS_SESSION_TOKEN`）
- 本机 `~/.aws/credentials` / IAM 角色

```bash
# 配置文件填写 s3.bucket 后，构建并上传
export AWS_ACCESS_KEY_ID=xxx
export AWS_SECRET_ACCESS_KEY=yyy
./builder build --os ubuntu --os-version 22.04 --kubernetes-version v1.27.3 --upload

# 覆盖 bucket / 前缀
./builder build ... --upload --s3-bucket my-bucket --s3-prefix releases/v1.27.3/

# MinIO
./builder build ... --upload --s3-endpoint http://127.0.0.1:9000 --s3-bucket pixiu

# 仅上传已有产物
./builder upload --file ./dist/pixiu-offline-ubuntu-22.04-amd64-v1.27.3-packages.tar.gz
```

对象键为 `{prefix}{文件名}`，例如 `pixiu-offline/pixiu-offline-ubuntu-22.04-amd64-v1.27.3-packages.tar.gz`。

## 软件源与包下载

### 源配置（容器内执行）

| 组件 | apt | dnf |
|------|-----|-----|
| k8s | 包仓库 `.../stable:/{minor}/deb/`；GPG 密钥固定取自 `v1.31` 的 `Release.key`（规避旧版密钥过期） | baseurl 指向目标 `{minor}`；`gpgkey` 取自 `v1.31` |
| containerd | `deb [signed-by=/etc/apt/keyrings/containerd-apt-keyring.gpg] https://download.docker.com/linux/{ubuntu\|debian} {codename} stable` | `[docker-ce-stable]`，baseurl `https://download.docker.com/linux/rhel/{9\|7}/$basearch/stable`，gpgkey `https://download.docker.com/linux/rhel/gpg` |

- k8s 大版本（如 v1.27）由 k8s 版本自动推导；签名密钥统一使用较新仓库（v1.31）的已续期密钥，避免 `EXPKEYSIG 234654DA9A296436`。codename / rpm_distro 在 `builder.yaml` 的 `oses` 节配置或按约定推导。
- containerd 源已实测：apt（ubuntu noble/jammy、debian bookworm Release）与 rhel/9 repomd.xml、rhel gpg key 均 HTTP 200；gpg key 路径不带版本号（`rhel/gpg`，`rhel/9/gpg` 为 404）。
- **openEuler 风险**：rpm_distro 为 `rhel7`，而 `download.docker.com/linux/rhel/7/` 实测 404（docker 官方已停止发布 RHEL7 仓库），openEuler 场景的 containerd dnf 源存在兼容性风险。

### 包下载（容器内一次完成）

在目标系统容器内（`docker run --rm --name builder-packages-<id> -v {packages目录}:/out {build_image} sh -c script`）：

1. 配置源 + 导入 GPG key；
2. `apt-get update` / `dnf makecache`；
3. 递归下载：apt `apt-get install -y --download-only --no-install-recommends <pkgs> -o Dir::Cache::archives=/out`；dnf `dnf install -y --downloadonly --downloaddir=/out <pkgs>`（插件缺失回退 `dnf download --resolve --destdir=/out`）；
4. 依赖闭包验证：`apt-get install --dry-run <pkgs>` / `dnf install --assumeno <pkgs>`。

**cri-tools 例外**：pkgs.k8s.io / download.docker.com 源内通常不存在 `cri-tools` 包，此时容器脚本会写 `cri-tools-missing` 标记，builder 回退从 GitHub release 下载 crictl 静态 tar 并放入 `packages/runtime/`（install.sh 检测到该 tar 时以 `install -m 0755` 安装）。

## 镜像源（mirror）

包模式下 k8s 组件与运行时均走官方包源，因此 `--mirror` 仅作用于**镜像阶段**的镜像仓库（`kubeadm config images list --image-repository`）：

- `official`：完整支持（默认），镜像仓库 `registry.k8s.io`。
- `aliyun` / `tencent`：预留镜像仓库映射（未实测），传此参数会报错并提示。

## 安装（目标机使用）

将离线包传到目标机后：

```bash
cd pixiu-offline-ubuntu-22.04-amd64-v1.27.3
sudo bash install/install.sh
```

install.sh 会：
1. 解析 `/etc/os-release` 检测包管理器；
2. preflight：`swapoff -a` 并注释 /etc/fstab swap 行；加载内核模块（overlay/br_netfilter/ip_vs 系列/nf_conntrack）；写入 sysctl 并 `sysctl --system`；
3. 安装 `packages/` 下所有包（apt：`dpkg -i *.deb` 失败自动 `apt-get -f install -y`；rpm：`rpm -ivh --nodeps *.rpm`）；
4. 配置 containerd `SystemdCgroup=true`，`systemctl enable --now containerd kubelet`；若存在 crictl 静态回退 tar 则安装 crictl；
5. 调用 `load-images.sh` 导入镜像（优先 `docker load`，失败回退 `ctr -n k8s.io images import`）；
6. `kubeadm version` 自检并输出 `kubeadm init` 示例命令。

## 开发

```bash
go build ./...   # 编译
go vet ./...     # 静态检查
go test ./...    # 单元测试
```

## 已知限制

- 镜像阶段按本机架构拉取（容器内 `docker pull`，经挂载的 docker.sock 操作宿主机 daemon）；若 `--arch` 与本机不同会打印 warning，无法交叉拉取。
- 依赖本机 docker；docker 不可用时 build 直接失败中断，不产出离线包；可用 `--mode packages`（或旧 `--skip-images`）显式跳过镜像阶段。
- 附加组件镜像 flannel 与 dashboard 位于 docker.io（`docker.io/flannel/flannel:v0.24.2`、`docker.io/kubernetesui/dashboard:v2.7.0`，registry.k8s.io 上无可用 tag）；metrics-server 仍为 `registry.k8s.io/metrics-server/metrics-server:v0.6.4`。网络不支持 docker.io 时可用 `--skip-addons` 跳过附加组件（核心镜像 + 软件包仍完整），未显式跳过时附加组件拉取失败仍中断。
- 核心镜像清单通过官方 kubeadm 静态二进制生成（`kubeadm config images list`，Linux 宿主机直跑；其它平台挂载进构建容器）。
- 镜像打包在 `docker:24-cli` 容器内执行（`--name builder-images-<id>`，挂载 `/var/run/docker.sock` 与输出目录）；软件包下载容器名为 `builder-packages-<id>`，便于 `docker ps` 区分阶段。
- 容器内真实执行依赖 docker，本机无 docker 时仅 dry-run 演练 + 单测；k8s 源（pkgs.k8s.io）未实测，containerd 源（download.docker.com）已用 curl 实测可达。
- aliyun / tencent 镜像仓库未实现，见上文。
