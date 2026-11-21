# 构建记录 1.0.1（x86_64）

- 日期：2026-11-21
- 宿主：Windows 10 x64（Git Bash）
- Go：1.27.4 windows/amd64 交叉编译 linux/amd64，CGO 关闭
- fnpack：1.2.3（Windows 版；打包后用 tools/patch-fpk.py 修正 0755、重算 checksum，并把 manifest 换行统一为 LF）
- 上游引擎：Mister Download Manager 0.5.2 Linux x86_64（依赖 glibc ≥ 2.35）

## 变更

- 取消“首次初始化仅限飞牛管理员”的校验，任何能打开应用的用户都可创建首个管理员账户。
- 版本号与界面版本标识更新为 1.0.1。

## 产物

| 文件 | 字节 | SHA-256 |
| --- | --- | --- |
| `fpk/src/app/mdm-fnos-server` | 7839904 | `b0c70e4e56aac858ae3aac5ea06845ebd6d5829ad14ebee17d1d6ddbe2e4e327` |
| `fpk/src/app/misterdownloadmanager` | 40464536 | `ff50a1404a6b2108cf7d2317738df1d08ea42ac02854bc51cf1f946aa92e7990` |
| `fpk/src/app/misterdownloadmanager-cli` | 317200 | `eb3aedd6e3f0a0e8fd7cbc97d4078173ba59372d3c3327bbf01a3c5a0395514e` |
| `mister-downloader_1.0.1_x86.fpk` | 52346246 | `157c3d22f92dca1cb16d1c440da55b7d6c20bd8d7c0cda2b3b335fb2187d3459` |

## 验证

详见 `validate-1.0.1.txt`：结构、权限、架构、网关入口、图标、checksum 全部通过，且二进制中已不含管理员限制提示。

## 未验证项

- 未在真实飞牛 fnOS 设备/虚拟机上安装运行（构建环境无 Linux 主机或 Docker）。
- 上游二进制依赖 glibc ≥ 2.35，需 fnOS 1.x（Debian 12 基线）。
