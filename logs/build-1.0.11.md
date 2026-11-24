# 1.0.11 构建记录

- 变更：`app/web/index.html` 顶部按钮整合进“设置”；`app/main.go` 版本号 1.0.11；`fpk/src/manifest` 版本与 changelog。
- 校验：`gofmt -l .` 无输出，`go vet ./...`、`go test ./...` 通过。
- 构建：Go 1.24 交叉编译 `linux/amd64`（`CGO_ENABLED=0`，`-trimpath -ldflags "-s -w"`）→ `fnpack build` → `tools/patch-fpk.py`。
- 产物：`mister-downloader_1.0.11_x86.fpk`，52,486,601 字节，app.tgz MD5 `d1408b1d063d7c83e543daeaeb29ac9d`。
- 校验：`tools/validate-fpk.py mister-downloader_1.0.11_x86.fpk 1.0.11` → RESULT: OK。
- SHA-256：`152f504e38a70a1778bf7e359e074b4a12f0310ce5999cadcfe91226f284481e`。