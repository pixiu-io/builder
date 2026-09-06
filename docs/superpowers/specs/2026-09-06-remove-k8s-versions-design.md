# 删除顶层 versions：必装包内定、crictl 自动推导

## 目标

去掉 `builder.yaml` 顶层 `versions` 参考清单。必装软件包不写配置、执行即打包；`addon_packages` 仅保留可选附加包。

## 行为

1. **必装包**（不变）：`BuildPackageList` — kubeadm/kubelet/kubectl + containerd + cri-tools + 系统依赖；不出现在 `addon_packages`。
2. **crictl 回退**：cri-tools 源包缺失时，版本一律 `strings.TrimPrefix(k8sVersion, "v")`，不再查清单覆盖。
3. **配置**：删除顶层 `versions`；`Load` 不再要求该节非空；`addon_packages` 语义不变。
4. **兼容**：旧配置若仍含 `versions`，yaml 未知字段可忽略或不再绑定；以删除结构体字段为准（多余键被 yaml.v3 忽略）。

## 非目标

- 不把 cri-tools/containerd 写入 `addon_packages`
- 不保留 per-version crictl 覆盖表
