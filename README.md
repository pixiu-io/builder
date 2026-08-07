# builder

制作 Kubernetes 离线安装包（apt/dnf/yum 软件包 + 运行时 + 镜像）的 Go CLI 工具。

生成的离线包包含：
- **k8s 软件包**：kubeadm / kubelet / kubectl（容器内通过 apt/dnf/yum 递归下载，依赖闭包交给包管理器）
- **容器运行时软件包**：containerd / cri-tools（containerd 按 OS 配置获取：默认 docker 源包名 `containerd.io`，openEuler 等从系统源安装 `containerd`；runc 由 containerd 包内嵌提供，不单独安装；containerd 源 + 系统源）
- **系统依赖软件包**：conntrack、ipvsadm、nfs-common(-utils)、socat、ebtables、chrony
- **镜像**：核心镜像（容器内安装 kubeadm 后生成清单）+ 附加组件镜像（builder.yaml `addon_images` 节）
- **附加安装包**：附加组件所需的宿主系统软件包（builder.yaml 顶层 `addon_packages` 节，name + 可选 version，mode ∈ packages/all 且未跳过附加时并入软件包清单；version 空不锁版本，非空按目标包管理器语法转译）
- **安装脚本**：install.sh（包安装 + preflight）/ load-images.sh
- **完整性清单**：manifest.yaml（可校验）

> 软件源说明：k8s 组件来自官方 `pkgs.k8s.io`（仓库由 k8s 版本自动推导，v1.27.3 → v1.27）；
> containerd 默认来自 docker 官方源 `download.docker.com`（包名 `containerd.io`）。原 `packages.containerd.io` 域名在当前网络与公共 DNS（223.5.5.5 / 8.8.8.8）均 NXDOMAIN 无法解析，已改用 docker 官方源提供 containerd.io 包。
> 该源的 containerd.io 包内嵌 runc 二进制，故不再单独安装 runc 包，避免与独立 runc 包的 `Conflicts: runc` 冲突导致 apt 无法同时解析。
> **openEuler 例外**：docker 官方无 `rhel/7` 仓库（`download.docker.com/linux/rhel/7/` 为 404），openEuler 改用系统源安装 containerd（`containerd_pkg: containerd` + `containerd_repo: none`，从 everything 仓库安装 `containerd` 包），不再配置 download.docker.com 源。

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
| `build` | 构建离线安装包。`--kubernetes-version` 必填（任意 vX.Y.Z；`--only-addons` 时可省略）。`--os` / `--os-version` 在 `--mode all` 或 `packages` 时必填（**任意**取值，不必在 list-os 中；未登记时构建镜像默认为 `{os}:{os-version}`）；`--mode images` 时可省略。可选：`--arch`（amd64/arm64，默认 amd64）、`--mirror`、`--mode`、`--workdir`、`--out`、`--skip-addons`、`--only-addons`、`--dry-run`、`--keep-files`（默认清理中间文件与 docker 中间镜像，置位保留）、`--upload`、`--cos-*` 等。核心软件包/镜像始终使用默认清单，自定义能力由配置 `addon_packages` / `addon_images` + `--mode` + `--only-addons` / `--skip-addons` 提供（见下文"自定义附加组件"）。所有 build 参数均可在配置文件 `build` 节预设（优先级：命令行 > 配置 > 内置默认值），见下文"build 参数配置化" |
| `upload` | 将已有产物 tar.gz 上传到腾讯云 COS。`--file` 可重复；`--cos-*` 覆盖 `cos` 节 |
| `serve` | 加载离线产物，提供本地 OCI registry（`docker pull` 短名）与 yum/dnf/apt HTTP 软件源（纯 Go，无外部工具依赖） |
| `list-os` | 列出参考 OS / 版本（builder.yaml；实际 build 不限于此列表） |
| `list-k8s` | 列出支持的 k8s 版本（含记录用运行时版本） |
| `list-images` | 列出附加组件镜像清单。纯 flag 指定：`--os`（必填）、`--kubernetes-version`（必填）、`--arch`（默认 amd64） |
| `list-serve-images` | 列出 serve 已加载的镜像（查询运行中的 registry，`--registry-addr` 默认 `127.0.0.1:5000`） |
| `verify` | 校验 bundle（目录或 tar.gz）完整性 |
| `version` | 打印版本 |

## 构建模式（`--mode`）

`build` 按 `--mode` 决定构建内容（默认 `all`），适合按需产出的场景：

| 模式 | 构建内容 | 适用场景 |
|------|---------|---------|
| `all`（默认） | 软件包 + 镜像 | 完整离线包，一次性交付 |
| `packages` | 仅软件包（k8s/运行时/系统依赖）+ 脚本 + manifest | 只更新软件包、或本机无 docker / 网络不支持镜像拉取时 |
| `images` | 仅镜像（核心 + 附加组件镜像）+ 脚本 + manifest | 软件包已就绪，只补充镜像；不依赖容器内包下载 |

`--mode packages` 即"仅软件包、跳过镜像阶段"，需要跳过镜像时直接使用该模式。被跳过的步骤在构建步骤汇总中标记为 `skipped`（非失败），Step 3/4/5（脚本/manifest/打包）始终执行，产物目录结构完整。

`--mode all` 时步骤 1（软件包，`builder-packages-*`）与步骤 2（镜像，`builder-images-*`）**并行**执行；仅跑其中一侧（`packages` / `images`）或 `--dry-run` 时仍串行。

```bash
# 仅软件包（跳过镜像阶段）
./builder build --os ubuntu --os-version 22.04 --kubernetes-version v1.27.3 --arch amd64 --mode packages

# 仅镜像（可省略 --os / --os-version；产物名为 pixiu-offline-images-{arch}-{k8sver}）
./builder build --mode images --kubernetes-version v1.27.3 --arch amd64

# 仅镜像且仍绑定发行版（产物名仍含 os/osver）
./builder build --os ubuntu --os-version 22.04 --kubernetes-version v1.27.3 --arch amd64 --mode images
```

## 自定义附加组件（addon_packages / addon_images）

核心软件包（kubeadm/kubelet/kubectl + containerd + cri-tools + 系统依赖）与核心镜像（kubeadm 生成）**始终使用默认清单**，不再支持按包/镜像覆盖或追加参数。containerd 包名与来源由 oses 节 `containerd_pkg` / `containerd_repo` 配置（默认 `containerd.io` + docker 源；openEuler 为 `containerd` + 系统源）；这两个字段**未配置时按发行版代码内推断**（openEuler → `containerd` + `none`，其他 → `containerd.io` + `docker`），因此旧版 builder.yaml 无需更新也能正确获取 openEuler 的 containerd。自定义能力完全由顶层 `addon_packages` / `addon_images` 与 `--mode` / `--only-addons` / `--skip-addons` 提供：

| 参数 | 说明 | 示例 |
|------|------|------|
| `--skip-addons` | 跳过附加组件：`addon_images` 不进镜像清单、`addon_packages` 不并入软件包清单（仅核心） | `--skip-addons` |
| `--only-addons` | 只打包附加组件：核心软件包与核心镜像全去，软件包=`addon_packages`、镜像=`addon_images`；与 `--skip-addons` 互斥。此时 `--kubernetes-version` 可省略（不构建 k8s 核心，无需推导 k8s 版本）；且只配置系统源（addon_packages 全部来自系统源，不再配置 k8s/containerd 源） | `--only-addons` |

```bash
# 默认（mode all）：软件包 = 核心 + addon_packages；镜像 = 核心 + addon_images
./builder build --os ubuntu --os-version 22.04 --kubernetes-version v1.27.3 --dry-run

# 跳过附加组件（仅核心软件包/镜像）
./builder build --os ubuntu --os-version 22.04 --kubernetes-version v1.27.3 --skip-addons --dry-run
```

`--mode` 与附加组件联动：`addon_packages` 在 `--mode packages` / `all` 且未 `--skip-addons` 时并入软件包清单；`addon_images` 在 `--mode images` / `all` 且未 `--skip-addons` 时并入镜像清单。详见下文"附加组件配置与构建模式联动"。

### 附加组件配置与构建模式联动

附加组件由两处顶层配置提供，均与 `--mode` / `--only-addons` / `--skip-addons` 联动：

- **`addon_images`**：附加组件镜像清单（name → image:tag）。`--mode images` / `all` 且未 `--skip-addons` 时并入镜像清单（与核心镜像去重）；`--only-addons` 时为其镜像主体（核心镜像全去）。
- **`addon_packages`**：附加安装包列表（对象格式：name + 可选 version）。`--mode packages` / `all` 且未 `--skip-addons` 时并入软件包下载清单（与核心清单按包名去重）；`--only-addons` 时为其软件包主体（核心软件包全去）。`version` 为空（或省略）不锁版本，透传纯包名；非空按目标包管理器语法转译（apt 系 `name=version`、dnf/yum 系 `name-version`），见下方示例。

```yaml
# 附加组件镜像（仅镜像；不含软件包）
addon_images:
  - name: flannel
    image: "docker.io/flannel/flannel"
    tag: "v0.24.2"
  - name: metrics-server
    image: "registry.k8s.io/metrics-server/metrics-server"
    tag: "v0.6.4"

# 附加安装包（与 addon_images 平级；部署附加组件需要的宿主系统软件包）
# 对象格式：name = 包名（apt/dnf 通用名，须与目标系统源匹配）；version 可选。
#   version 为空（或省略）= 不锁版本，透传纯包名给容器内包管理器；
#   version 非空 = 锁定版本，按目标包管理器语法转译：apt 系 name=version、dnf/yum 系 name-version
#   （版本需与目标系统源匹配，不匹配时由容器内包管理器报错，请自行调整）。
addon_packages:
  - name: conntrack
    version: ""       # 不锁版本：并入清单时透传 conntrack
  - name: vim
    version: "9.0"    # apt 系 → vim=9.0；dnf/yum 系 → vim-9.0
```

`--only-addons` 与 `--skip-addons` 互斥（同时传报错）：

```bash
# 默认（mode all）：核心 + addon_packages 并入软件包；核心 + addon_images 并入镜像
./builder build --os ubuntu --os-version 22.04 --kubernetes-version v1.27.3 --dry-run

# mode packages：软件包 = 核心 + addon_packages；镜像跳过
./builder build --os ubuntu --os-version 22.04 --kubernetes-version v1.27.3 --mode packages --dry-run

# mode images：软件包跳过；镜像 = 核心 + addon_images
./builder build --os ubuntu --os-version 22.04 --kubernetes-version v1.27.3 --mode images --dry-run

# 只打包附加：软件包 = addon_packages、镜像 = addon_images（无核心；--kubernetes-version 可省略）
./builder build --os ubuntu --os-version 22.04 --only-addons --dry-run

# 只打包附加且仅镜像：镜像 = addon_images（无核心、无软件包）
./builder build --os ubuntu --os-version 22.04 --kubernetes-version v1.27.3 --only-addons --mode images --dry-run

# 跳过附加：无 addon_packages / addon_images，仅核心
./builder build --os ubuntu --os-version 22.04 --kubernetes-version v1.27.3 --skip-addons --dry-run
```

## 产物目录结构

```
pixiu-offline-{os}-{osver}-{arch}-{k8sver}/   # 指定 OS 的镜像包（--mode images 且指定 OS）
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

构建完成后会在 `--out` 目录生成同名 `.tar.gz`。软件包产物统一为 `pixiu-offline-packages-{os}-{osver}-{arch}-{k8sver}.tar.gz`（单模式 packages 与 `--mode all` 拆分一致）；`--mode all` 时拆成两个独立产物：
- `pixiu-offline-packages-{os}-{osver}-{arch}-{k8sver}.tar.gz`
- `pixiu-offline-{os}-{osver}-{arch}-{k8sver}-images.tar.gz`

## 配置文件

单份 yaml 默认位于 `/etc/pixiu/builder.yaml`（生产部署，与 pixiu 配置惯例一致），可通过 `--configFile` 或环境变量 `BUILDER_CONFIG_FILE` 指定配置文件。本地开发使用仓库根目录下的 `builder.yaml`：`--configFile ./builder.yaml` 或 `export BUILDER_CONFIG_FILE=./builder.yaml`。文件按顶层分为六节：

| 节 | 内容 |
|------|------|
| `build` | build 子命令默认参数（可选；优先级：命令行 > 配置 > 内置默认值） |
| `oses` | OS 注册表：可用版本、包管理器（apt/dnf）、容器内下载软件包用的构建镜像、架构、apt 版本代号（codename/codenames）、dnf 发行版标识（rpm_distro）、containerd 包名与来源（containerd_pkg / containerd_repo） |
| `versions` | k8s 版本定义；containerd/runc 为记录用，crictl 用于 cri-tools 包缺失时的静态回退 |
| `addon_images` | 附加组件镜像清单（name → image:tag；仅镜像，不含软件包） |
| `addon_packages` | 附加安装包列表（对象格式：name + 可选 version；version 空不锁版本、非空按目标包管理器语法转译 name=version / name-version；mode ∈ packages/all 且未跳过附加时并入软件包清单） |
| `cos` | 可选：产物上传到腾讯云 COS（bucket 含 appid/region/secret_id/secret_key/prefix） |

> `versions` / `build_images` 为初始值，加载器不验证镜像可用性，生产使用请按需核对。

### build 参数配置化

`build` 子命令的所有 `--` 参数都可在配置文件 `build` 节预设，按 **命令行 > 配置文件 > 内置默认值** 的优先级取最终值：

1. 命令行显式传参 → 优先；
2. 未传时回落配置文件 `build` 节（字符串为空则跳过）;
3. 配置也为空 → 使用 flag 内置默认值。

因此像 `--os`、`--kubernetes-version` 这类"必填"参数，命令行或配置 `build` 节任一提供即可。例外：`--only-addons` 时 `--kubernetes-version` 可省略（不构建 k8s 核心，无需 k8s 版本）。配置示例：

```yaml
build:
  os: "ubuntu"
  os_version: "22.04"
  kubernetes_version: "v1.31.1"
  arch: "amd64"          # 命令行 --arch 未传时生效
  mirror: "aliyun"
  workdir: "./work"
  out: "./dist"
  mode: "all"
  skip_addons: false
  only_addons: false     # 只打包附加组件（addon_images / addon_packages），核心软件包与镜像全去；与 skip_addons 互斥
  dry_run: false
  keep_files: false      # 默认 false=构建完成后清理中间文件与 docker 中间镜像；true=保留
  verbose: false         # 默认 false=精简输出；true=打印详细过程日志（镜像下载/pull 进度等）
  kubeadm_mode: "local"  # kubeadm 获取模式：local=本地下载（默认）/ remote=ssh 远端下载+拷回
  kubeadm_remote_host: ""   # remote 模式远端服务器（user@host，免密登录）
  kubeadm_remote_path: ""   # remote 模式远端缓存目录（默认 ~/.builder-kubeadm，含 {version}/{arch} 子目录）
```

例如配置 `arch: "arm64"` 后执行 `builder build ...`（不传 `--arch`）会构建 arm64 产物；命令行传 `--arch amd64` 则仍以命令行优先。

## 上传产物（腾讯云 COS）

构建完成后可用 `--upload` 自动上传产物，或用 `upload` 子命令上传已有 tar.gz。需在 `builder.yaml` 配置 `cos` 节，或通过 `--cos-*` flag 覆盖。

```yaml
cos:
  bucket: mybucket-1250000000   # 桶名-appid
  region: ap-guangzhou
  secret_id: xxx
  secret_key: yyy
  prefix: pixiu-offline/
```

> ⚠️ SecretId/SecretKey 明文写入配置文件有泄露风险，建议 `chmod 600 builder.yaml`。

```bash
# 配置文件填写 cos 节后，构建并上传
./builder build --os ubuntu --os-version 22.04 --kubernetes-version v1.27.3 --upload

# 覆盖 bucket / 前缀
./builder build ... --upload --cos-bucket mybucket-1250000000 --cos-prefix releases/v1.27.3/

# 仅上传已有产物
./builder upload --file ./dist/pixiu-offline-packages-ubuntu-22.04-amd64-v1.27.3.tar.gz

# 上传整个目录（递归子目录），并按文件名忽略部分文件（--skip 可重复）
./builder upload --dir ./dist --skip md5sum.txt --skip checksum.txt \
  --cos-bucket mybucket-1250000000 --cos-region ap-guangzhou \
  --cos-secret-id xxx --cos-secret-key yyy
```

对象键为 `{prefix}{文件名}`，例如 `pixiu-offline/pixiu-offline-packages-ubuntu-22.04-amd64-v1.27.3.tar.gz`。

## 离线源服务（`serve`）

将 builder 产物加载为常驻服务：镜像走本地 registry（短名），软件包走 HTTP yum/dnf/apt 源。不依赖 `createrepo` / `apt-ftparchive`。

```bash
# packages + images 两个 tar 一起加载
./builder serve \
  --bundle ./dist/pixiu-offline-packages-centos-8-amd64-v1.27.3.tar.gz \
  --bundle ./dist/pixiu-offline-centos-8-amd64-v1.27.3-images.tar.gz \
  --advertise-host 192.168.1.10

# 已解压目录
./builder serve --bundle ./work/pixiu-offline-ubuntu-22.04-amd64-v1.27.3

# 指定离线包目录：自动加载其中所有 *.tar.gz，并每 3s 轮询热加载新放入的包
./builder serve --dir ./offline-packages --advertise-host 192.168.1.10
```

`--advertise-host` 默认取**本机非 loopback IP**（打印给客户端），无需显式指定；仅在需要对外暴露固定地址时手动覆盖。

默认端口：
- registry：`0.0.0.0:5000` → `docker pull <host>:5000/kube-apiserver:v1.27.3`
- 软件源：`0.0.0.0:8080` → `http://<host>:8080/rpm`（dnf）或 `/deb`（apt）

客户端示例：

```bash
# 镜像（需将 host:5000 加入 Docker insecure-registries）
docker pull 192.168.1.10:5000/kube-apiserver:v1.27.3
kubeadm init --image-repository 192.168.1.10:5000 ...

# dnf / yum
dnf install --repofrompath=pixiu,http://192.168.1.10:8080/rpm kubeadm

# apt
echo 'deb [trusted=yes] http://192.168.1.10:8080/deb ./' > /etc/apt/sources.list.d/pixiu-offline.list
apt-get update && apt-get install kubeadm
```

## 软件源与包下载

### 源配置（容器内执行）

| 组件 | apt | dnf（yum 复用同一列） |
|------|-----|-----|
| k8s | 包仓库 `.../stable:/{minor}/deb/`；GPG 密钥固定取自 `v1.31` 的 `Release.key`（规避旧版密钥过期） | baseurl 指向目标 `{minor}`；`gpgkey` 取自 `v1.31` |
| containerd | `deb [signed-by=/etc/apt/keyrings/containerd-apt-keyring.gpg] https://download.docker.com/linux/{ubuntu\|debian} {codename} stable` | `[docker-ce-stable]`，baseurl `https://download.docker.com/linux/rhel/{9\|7}/$basearch/stable`，gpgkey `https://download.docker.com/linux/rhel/gpg`；openEuler（`containerd_repo: none`）不配置该源，containerd 由系统源提供 |

- k8s 大版本（如 v1.27）由 k8s 版本自动推导；签名密钥统一使用较新仓库（v1.31）的已续期密钥，避免 `EXPKEYSIG 234654DA9A296436`。codename / rpm_distro 在 `builder.yaml` 的 `oses` 节配置或按约定推导。
- containerd 源已实测：apt（ubuntu noble/jammy、debian bookworm Release）与 rhel/9 repomd.xml、rhel gpg key 均 HTTP 200；gpg key 路径不带版本号（`rhel/gpg`，`rhel/9/gpg` 为 404）。
- **CentOS 7 使用 yum**：`--os centos --os-version 7` 时自动选择 yum 下载（dnf 是 CentOS 8+/Fedora 才有）；yum 的源配置与 dnf 相同（`/etc/yum.repos.d/` + `rpm --import`），下载依赖 downloadonly 插件，失败自动回退 `yumdownloader`。CentOS 8/9 等仍走 dnf。
- **CentOS 7 默认源已自动切换 vault**：CentOS 7 已于 2024-06 停止维护（EOL），官方 `mirrorlist.centos.org` 域名 DNS 已不可解析，容器内 `yum makecache` 必然失败。yum 分支脚本开头会检测 `/etc/centos-release` 的 `release 7.`，命中时移走默认 `CentOS-Base.repo`（其 base/extras/updates 块只配 mirrorlist 无 baseurl，保留会造成 repo 重复且无有效源）并写入 `centos-vault.repo`，将 base/extras/updates 指向 vault.centos.org 归档源（7.9.2009），保证 yum makecache / install 能拉取系统依赖包（vim/conntrack 等）。非 CentOS 7 不受影响。
- **`--only-addons` 不配置 k8s/containerd 源**：只打包附加软件包（addon_packages，全部来自系统源）时，容器内仅配置系统源，不再写入 pkgs.k8s.io / download.docker.com 源，避免失效源（如 CentOS 7 的 download.docker.com rhel/7 已 404）导致 `yum makecache` 失败。CentOS 7 的 vault 系统源修复始终生效（非 only-addons 同样保留）。
- **containerd 按系统配置**：默认 docker 源（`containerd.io` 包，`containerd_repo` 默认 `docker`）；openEuler 等 `containerd_repo: none` 的发行版不配置 download.docker.com 源，containerd 改由系统源（everything 仓库，含 `containerd-1.2.0-315.oe2203sp3`）安装，软件包名为 `containerd`（`containerd_pkg`）。因 docker 官方已停止发布 RHEL7 仓库，`download.docker.com/linux/rhel/7/` 为 404（CentOS 7 走 yum 时同样受该 rhel/7 源限制；`--only-addons` 与 openEuler 系统源场景不配置该源，不受影响）。

### 包下载（容器内一次完成）

在目标系统容器内（`docker run --rm --name builder-packages-<id> -v {packages目录}:/out {build_image} sh -c script`）：

1. 配置源 + 导入 GPG key；
2. `apt-get update` / `dnf makecache` / `yum makecache`；
3. 递归下载：apt `apt-get install -y --download-only --no-install-recommends <pkgs> -o Dir::Cache::archives=/out`；dnf `dnf install -y --downloadonly --downloaddir=/out <pkgs>`（插件缺失回退 `dnf download --resolve --destdir=/out`）；yum `yum install -y --downloadonly --downloaddir=/out <pkgs>`（downloadonly 插件安装失败回退 `yumdownloader --resolve --destdir=/out`）；
4. 依赖闭包验证：`apt-get install --dry-run <pkgs>` / `dnf install --assumeno <pkgs>`；yum 无 `--assumeno`，`--downloadonly` 下载成功即表示依赖可解析。

**cri-tools 例外**：pkgs.k8s.io / download.docker.com 源内通常不存在 `cri-tools` 包，此时容器脚本会写 `cri-tools-missing` 标记，builder 回退从 GitHub release 下载 crictl 静态 tar 并放入 `packages/runtime/`（install.sh 检测到该 tar 时以 `install -m 0755` 安装）。

## 镜像源（mirror）

包模式下 k8s 组件与运行时均走官方包源，因此 `--mirror`（或配置文件 `build.mirror`）仅作用于**镜像阶段**的镜像仓库（`kubeadm config images list --image-repository`）。核心镜像清单、拉取/保存的镜像引用及 manifest 中的 `source_image` 均带该镜像仓库地址：

- `aliyun`（默认）：镜像仓库 `registry.aliyuncs.com/google_containers`。
- `official`：镜像仓库 `registry.k8s.io`。
- `tencent`：镜像仓库 `mirror.cc.tencentyun.com/kubernetes`。

仓库地址需保证可访问且存在对应版本的 k8s 镜像；软件包源始终走官方源，不受 mirror 影响。

**kubeadm 二进制获取**（生成核心镜像清单用）：支持 `local`（默认，本机从 dl.k8s.io/CDN 下载）与 `remote`（ssh 到**免密登录**服务器下载并 scp 拷回）两种模式。remote 模式远端按 `{缓存目录}/{k8s版本}/{架构}/kubeadm` 缓存，已存在则直接拷回，否则在远端下载：

```bash
./builder build --os ubuntu --os-version 22.04 --kubernetes-version v1.27.3 \
  --kubeadm-mode remote --kubeadm-remote-host root@192.168.1.10
```

- 远端默认缓存目录 `~/.builder-kubeadm`，可用 `--kubeadm-remote-path` / 配置 `kubeadm_remote_path` 覆盖
- 远端下载 `curl` 优先、`wget` 兜底；远端服务器需已配置免密登录（ssh-key）

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
- 依赖本机 docker；docker 不可用时 build 直接失败中断，不产出离线包；可用 `--mode packages` 显式跳过镜像阶段。
- 附加组件镜像 flannel 与 dashboard 位于 docker.io（`docker.io/flannel/flannel:v0.24.2`、`docker.io/kubernetesui/dashboard:v2.7.0`，registry.k8s.io 上无可用 tag）；metrics-server 仍为 `registry.k8s.io/metrics-server/metrics-server:v0.6.4`。网络不支持 docker.io 时可用 `--skip-addons` 跳过附加组件（addon_images 与 addon_packages 均不进产物，核心镜像 + 核心软件包仍完整），未显式跳过时附加组件拉取失败仍中断。
- 核心镜像清单通过官方 kubeadm 静态二进制生成（`kubeadm config images list`，Linux 宿主机直跑；其它平台挂载进构建容器）。
- 镜像打包在 `docker:24-cli` 容器内执行（`--name builder-images-<id>`，挂载 `/var/run/docker.sock` 与输出目录）；软件包下载容器名为 `builder-packages-<id>`，便于 `docker ps` 区分阶段。
- 容器内真实执行依赖 docker，本机无 docker 时仅 dry-run 演练 + 单测；k8s 源（pkgs.k8s.io）未实测，containerd 源（download.docker.com）已用 curl 实测可达，openEuler 系统源（everything 仓库）的 containerd 包已确认 repomd 200 可达。
- CentOS 7（yum）依赖 downloadonly 插件与 yumdownloader；CentOS 7 已 EOL，默认源已由脚本自动切换 vault.centos.org（见上文软件源说明），且 containerd 源对应 rhel/7 在 download.docker.com 为 404，非 only-addons 场景的 containerd 包下载存在兼容性风险（系统依赖包经 vault 可正常下载，containerd.io 需 docker 源提供）；`--only-addons` 不配置 k8s/containerd 源，不受该 rhel/7 源限制。
- openEuler 已通过 `containerd_pkg: containerd` + `containerd_repo: none` 从系统源安装 containerd，规避 download.docker.com 无 rhel/7 仓库导致的 `dnf makecache` 失败；即使配置未显式声明这两个字段，也会按发行版自动推断为系统源（containerd + none），旧配置兼容；其他 dnf 系（rocky 等）仍走 docker 源 `containerd.io` 包。
- aliyun / tencent 镜像仓库已支持，仓库地址可用性需按网络环境验证（见上文"镜像源（mirror）"）。
