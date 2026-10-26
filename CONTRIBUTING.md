# 贡献指南

感谢关注「可达书柜」！欢迎提 issue、报 bug、贡献代码。

## 环境

任意 Linux 都能开发（fnOS NAS 本机也可以），不需要在系统里装 Go：

```bash
git clone https://github.com/Bilibilimoon/keda-shelf.git
cd keda-shelf
./build.sh          # 自动下载 Go 1.23 工具链到 tools/（不装进系统）
```

依赖只有 `curl`、`tar` 和 `fnpack`（飞牛打包工具）。

## 开发流程

1. Fork 并建分支（`git checkout -b feat/xxx`）。
2. 改代码：保持标准库风格与现有注释密度；架构与「为什么这么做」见
   [DEVELOPMENT.md](DEVELOPMENT.md)——尤其 §5 扫描规则、§7 关键决策和
   §12 修改指南，改动前先确认不会破坏系列 ID 的稳定性。
3. 跑测试：

   ```bash
   cd src && ../tools/go/bin/go test .        # 单元测试
   python3 tools/smoke_test.py --help          # HTTP 冒烟测试（用法见 DEVELOPMENT.md §9）
   ```

4. 提交：commit message 用一行主题 + `feat:` / `fix:` / `docs:` /
   `security:` / `chore:` 前缀。
5. 发 PR：描述动机、改动点和验证方式（跑过哪些测试）。

## 约束

- **保持 `CGO_ENABLED=0`、纯 Go 依赖**。引入新第三方库前先开 issue 讨论，
  说明理由（会被静态编译进双架构二进制）。
- **不要在代码、文档、日志、测试数据里写入真实的域名、公网/内网 IP、
  账号或私人路径**——这是公开仓库，用占位符。
- 不修改 fnOS 系统组件（`/usr/trim` 等），不依赖 NAS 上的 Python/Node 运行时。
- 面向用户的改动（安装、配置、管理页行为）请同步更新 [README.md](README.md)；
  架构决策与平台坑同步 [DEVELOPMENT.md](DEVELOPMENT.md)。

## 许可

提交 PR 即表示同意该贡献以 [PolyForm Noncommercial 1.0.0](LICENSE)
随本项目分发。
