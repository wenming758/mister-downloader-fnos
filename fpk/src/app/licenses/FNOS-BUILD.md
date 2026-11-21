# 关于本安装包

本安装包由第三方（wenming758 / wenming760）打包，**非 Mister Download Manager 官方发布**。

- 上游程序：Mister Download Manager 0.5.2，作者 Ahmed Nefzaoui，许可证 GPL-3.0（见同目录 `LICENSE`）。
- 第三方素材与各自许可证：见同目录 `ATTRIBUTION.md`。
- 对应源码：同目录 `upstream-source-0.5.2.tar.gz` 为上游 0.5.2 源码快照；上游仓库 <https://github.com/anefzaoui/misterdownloadmanager>。
- 本安装包新增的封装层（Go 适配服务、Web 外壳、飞牛生命周期脚本）源码：<https://gitee.com/wenming760/mister-downloader-fnos> 或 <https://github.com/wenming758/mister-downloader-fnos>，同样以 GPL-3.0 发布。

安装包内的 `misterdownloadmanager` 与 `misterdownloadmanager-cli` 为上游发布的 Linux x86_64 二进制，未做修改；`mdm-fnos-server` 为上述封装层程序。
