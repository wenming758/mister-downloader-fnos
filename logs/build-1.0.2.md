# 构建记录 1.0.2（x86_64）

- 日期：2026-11-21
- 宿主：Windows 10 x64（Git Bash）
- Go：1.27.4 交叉编译 linux/amd64，CGO 关闭
- fnpack：1.2.3；打包后由 tools/patch-fpk.py 修正 0755、重算 checksum、manifest 统一 LF
- 上游引擎：Mister Download Manager 0.5.2 Linux x86_64

## 变更

- 修复通过 HTTP 打开飞牛应用时会话 Cookie 带 Secure 被浏览器丢弃、初始化后无法进入的问题。
- 登录与初始化响应同时返回会话令牌，请求头 `X-MDM-Session` 作为 Cookie 失效时的兜底。
- 初始化遇到 409 时前端自动切换到登录页，不再停在初始化表单。

## 本机会话测试（Windows + 同一适配层）

- HTTP 下 `Set-Cookie` 不再带 `Secure`（携带 X-Trim-* 头时同样正确）。
- 仅带 `X-MDM-Session` 请求头访问 `/fnapi/me` 返回 200；坏 Cookie + 好请求头同样返回 200。
- 重复初始化返回 409；Cookie 登录后访问 `/fnapi/me` 返回 200。

## 产物

| 文件 | 字节 | SHA-256 |
| --- | --- | --- |
| `fpk/src/app/mdm-fnos-server` | 7839904 | `0d2bae8420ecf2b1a427eaa150a83ff477bc0cac5a04a55166c3be7c109c82be` |
| `fpk/src/app/misterdownloadmanager` | 40464536 | `ff50a1404a6b2108cf7d2317738df1d08ea42ac02854bc51cf1f946aa92e7990` |
| `fpk/src/app/misterdownloadmanager-cli` | 317200 | `eb3aedd6e3f0a0e8fd7cbc97d4078173ba59372d3c3327bbf01a3c5a0395514e` |
| `mister-downloader_1.0.2_x86.fpk` | 52346126 | `3f8e4cd4a5341190e3db83ff1abff0da517d6ff75edde6b6dc1f596ff863cef3` |

## 未验证项

- 未在真实飞牛 fnOS 设备/虚拟机上安装运行。
- 上游二进制依赖 glibc ≥ 2.35，需 fnOS 1.x（Debian 12 基线）。
