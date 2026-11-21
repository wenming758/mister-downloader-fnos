# 构建记录 1.0.0（x86_64）

- 日期：2026-11-21
- 宿主：Windows 10 x64（Git Bash）
- Go：1.27.4 windows/amd64 交叉编译 linux/amd64，CGO 关闭
- fnpack：1.2.3（Windows 版；打包后用 tools/patch-fpk.py 修正 0755、重算 checksum，并把 manifest 换行统一为 LF）
- 上游引擎：Mister Download Manager 0.5.2 Linux x86_64（依赖 glibc ≥ 2.35）

## 产物

| 文件 | 字节 | SHA-256 |
| --- | --- | --- |
| `fpk/src/app/mdm-fnos-server` | 7839904 | `170e409aae499340971c367cca3c52862a9edc9c784d06439ced9422da7d5aeb` |
| `fpk/src/app/misterdownloadmanager` | 40464536 | `ff50a1404a6b2108cf7d2317738df1d08ea42ac02854bc51cf1f946aa92e7990` |
| `fpk/src/app/misterdownloadmanager-cli` | 317200 | `eb3aedd6e3f0a0e8fd7cbc97d4078173ba59372d3c3327bbf01a3c5a0395514e` |
| `mister-downloader_1.0.0_x86.fpk` | 52343913 | `ddab84ae7ba31889eabc26041cb9245009c696fc5173862b22557bd947a51bfb` |

## 步骤

1. `GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w"` 构建 `mdm-fnos-server`。
2. 复制上游 `misterdownloadmanager`、`misterdownloadmanager-cli`；三者均确认为 ELF64 x86-64，Go 程序无动态依赖。
3. `fnpack build -d src` 生成 `fpk/mister-downloader.fpk`。
4. `python tools/patch-fpk.py`：`cmd/*` 与三个二进制设为 0755，重算 `app.tgz` MD5 写入 manifest，manifest 统一 LF。
5. 校验见 `validate-1.0.0.txt`：结构、权限、架构、网关入口、图标、checksum 全部通过。

## 未验证项

- 未在真实飞牛 fnOS 设备/虚拟机上安装运行（构建环境无 Linux 主机或 Docker）。
- 上游二进制依赖 glibc ≥ 2.35，需 fnOS 1.x（Debian 12 基线）。
