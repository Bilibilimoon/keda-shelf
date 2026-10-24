# 可达书柜（mangagate）开发记录（面向后续维护者 / AI）

最后更新：2026-10-23，对应版本 **0.1.2**。

这份文档记录「为什么这么做」和「踩过哪些坑」，README.md 只讲用户怎么装怎么用。
接手前请先通读本文，再改代码。

---

## 1. 项目是什么

飞牛 fnOS 应用「可达书柜」（appname `mangagate`，显示名已从「漫画网关」更名）。它把 NAS 上的漫画目录
变成 OPDS 1.2 书架，让 iOS 的「可达阅读器」直接连接阅读。
单进程、单端口、自包含二进制，不依赖 Node/Python/Docker/PHP。

明确不做的事：

- 不做 SMB 服务
- 不提供 WebDAV（0.1.2 起移除，App 走 OPDS）
- 不写漫画文件，不做在线编辑
- 不做漫画源抓取、不做在线下载
- 不通过 fnOS 统一网关给 App 提供接口（原因见 §7）

## 2. 快速开始

```bash
cd ~/keda-shelf

# 构建（会自动下载 Go 到 tools/go，不会装进系统）
./build.sh

# 产物
pkg/mangagate/mangagate.fpk

# 本机安装
sudo appcenter-cli install-fpk pkg/mangagate/mangagate.fpk -e /tmp/mg.env
# /tmp/mg.env 内容：admin_user / admin_pass / port

# 协议自检（需要账号密码）
python3 tools/smoke_test.py --base http://127.0.0.1:5080 \
    --user admin --pass test123456
```

**注意**：`appcenter-cli install-fpk` 对已安装应用不会升级（见 §8.1）。
改代码后要先 `uninstall` 再装，否则跑的还是旧二进制。

## 3. 运行时拓扑

```
                    iOS 可达阅读器
                          │ Basic Auth, HTTP
                          ▼
        ┌─────────────────────────────────────┐
        │ mangagate 进程（用户 mangagate）      │
        │                                     │
        │  TCP :5080  ──► requireAuth(mux)    │  OPDS / 管理页 / API（HTTP + 可选 HTTPS）
        │                                     │
        │  Unix sock  ──► gatewayAuth(mux)    │  仅 fnOS 桌面 iframe 管理页
        │  ${TRIM_APPDEST}/mangagate.sock     │
        └─────────────────────────────────────┘
                          │ 只读
                          ▼
               授权目录（TRIM_DATA_ACCESSIBLE_PATHS）
```

- TCP 监听 `0.0.0.0:5080`，`ReadHeaderTimeout=10s`、`IdleTimeout=120s`，**不设
  WriteTimeout**（大文件下载不能超时）。
- Unix socket 只给统一网关用，网关先校验 NAS 登录，再注入
  `X-Trim-Username` / `X-Trim-Isadmin` / `X-Trim-Userid` 后转发。
- 两个监听共用同一个 `routes()` mux，socket 侧多一层 `/app/mangagate` 前缀剥离。

### 目录映射

| 用途 | 路径 |
|---|---|
| 安装文件/二进制 | `/vol1/@appcenter/mangagate/`（`/var/apps/mangagate/target`） |
| 配置、users.json | `/vol1/@appconf/mangagate/`（`TRIM_PKGETC`） |
| 运行数据、封面缓存、日志 | `/vol1/@appdata/mangagate/`（`TRIM_PKGVAR`） |
| 应用 home（测试库放这里） | `/vol1/@apphome/mangagate/`（`TRIM_PKGHOME`） |
| 生命周期脚本 | `/var/apps/mangagate/cmd/` |

## 4. 代码地图（src/，约 2300 行）

| 文件 | 职责 | 关键函数 |
|---|---|---|
| `main.go` | 参数解析、目录准备、用户初始化、库扫描、两个 listener、优雅退出、路由注册 | `main`, `routes`, `adminOnly`, `stripPrefix`, `resolvePort`, `ensureUsers` |
| `config.go` | `config.json` / `users.json` 读写、授权路径解析、根目录选择 | `loadConfig`, `saveConfig`, `resolveRoots`, `pickRoot`, `loadUsers`, `UserStore.*` |
| `auth.go` | `server` 结构体、Basic Auth、网关信任、按 IP 限速 | `requireAuth`, `gatewayAuth`, `rateLimiter` |
| `library.go` | 目录扫描、`Series`/`Book` 模型、页数统计、自然排序 | `library.scan`, `scanDir`, `makeBook`, `countZipPages`, `naturalLess` |
| `opds.go` | OPDS 1.2 feed 生成、下载（Range / 动态 zip）、封面路由、搜索 | `handleOPDS`, `serveRootFeed`, `seriesEntry`, `bookEntry`, `serveBookFile`, `serveSearch` |
| `cover.go` | 封面提取（zip 首图 / 目录首图）、320px JPEG 缓存、占位图 | `coverJPEG`, `firstImageInZip`, `firstImageInDir`, `encodeCover` |
| `tls.go` | 同端口 HTTP/HTTPS 协议识别、证书加载 | `loadTLSConfig`, `sniffListener` |
| `admin.go` | 管理页与 JSON API、扫描状态、限速/CSRF 辅助 | `handleAdminPage`, `apiStatus`, `api*User*`, `apiSetRoot`, `apiRescan`, `startScan` |
| `admin.html` | 管理页前端（内嵌 `go:embed`，相对路径请求） | — |
| `util.go` | MIME、路径安全、ID、LAN IP 探测 | `hashID`, `safeJoin`, `detectLANIP`, `defaultRouteIP`, `naturalLess` 的配套 |

`admin.go` 末尾的 `unameArch()` 与 `coverPath()` 目前没有被调用，可以删。

### 数据模型

```go
Book   { ID, Title, Rel, Path, Format, Size, Pages, ModTime, SeriesID }
Series { ID, Name, Rel, Books []*Book, Cover }
library{ root, name, series []*Series, books map[string]*Book, scannedAt }
```

- `ID = sha1(prefix + rel path)[:16]`，`book:` / `series:` 前缀区分。路径不变则 ID 稳定。
- `Rel` 是相对授权根目录的路径，`Path` 是绝对路径（只在内存里，JSON 不导出）。
- 扫描是**同步**的，发生在监听端口之前；大库会让启动变慢，但端口不会半开。

## 5. 扫描与识别规则

规则集中在 `library.go:scanDir`：

1. 跳过隐藏项（`.` 开头）、`@eaDir`、`#recycle`、`$RECYCLE.BIN`、
   `System Volume Information`、`lost+found`、`Thumbs.db`、`desktop.ini`。
2. 跳过符号链接（`os.ModeSymlink`），防止逃出授权目录。
3. 目录内有压缩包（`.cbz/.zip/.cbr/.rar/.cb7/.pdf`）→ 该目录是一个 **Series**，
   每个压缩包是一本 **Book**。
4. 目录内只有图片且没有压缩包 → 整个目录是一本 **Book**（`Format="dir"`），
   归到父级 Series。
5. 有子目录 → 递归（最大深度 8），子目录各自成 Series。
6. 排序用 `naturalLess`，保证「第2卷」在「第10卷」之前。
7. zip 的页数在扫描时统计（`countZipPages`），RAR 页数为 0（不解压）。

## 6. 协议契约

### 6.1 端点

| 路径 | 说明 |
|---|---|
| `/healthz` | 免认证，返回 `{status,ready,series,books,uptime}` |
| `/opds`、`/opds/` | 与 `/opds/v1.2/catalog` 相同的根 feed |
| `/opds/v1.2/catalog` | 根导航 feed：`全部系列` + 最多 300 个系列 |
| `/opds/v1.2/series` | 全部系列（navigation） |
| `/opds/v1.2/series/{id}` | 某系列的书籍（acquisition） |
| `/opds/v1.2/series/{id}/thumbnail` | 系列封面（取第一本书） |
| `/opds/v1.2/books/{id}` | 单书 feed |
| `/opds/v1.2/books/{id}/file` | 下载；压缩包走 `http.ServeContent`（支持 Range），`dir` 动态打 zip |
| `/opds/v1.2/books/{id}/thumbnail` | 封面 JPEG |
| `/opds/v1.2/search?q=` | 标题/系列名子串搜索，最多 300 条 |
| `/opds/v1.2/opensearch.xml` | OpenSearch 描述 |
| `/api/status` | 登录即可读（Basic Auth 或网关） |
| `/api/users`、`/api/users/add`、`/api/users/delete`、`/api/users/password` | 仅 NAS 管理员（网关） |
| `/api/root`、`/api/rescan` | 仅 NAS 管理员（网关） |
| `/` | 管理页（相对路径请求 API，兼容网关前缀） |

### 6.2 认证与限速

- HTTP Basic Auth，密码用 bcrypt 存储，`users.json` 权限 0600。
- **应用账号一律只读**：`requireAuth` 注入的 `User` 强制 `Admin=false`、`Gateway=false`，
  `users.json` 里的历史 `admin` 字段不再生效。
- **管理权限只认飞牛网关**：只有 `gatewayAuth` 注入且 `X-Trim-Isadmin: true` 的请求
  才能调用管理 API（`isAdmin` 要求 `Gateway && Admin`）。
- 未登录返回 `401` + `WWW-Authenticate: Basic realm="可达书柜"`。
- **同一 IP 每分钟失败 3 次后**，第 4 次请求（无论密码对错）返回
  `429` + `Retry-After: 60`。计数在内存里，进程重启清空。
  这是用户明确要求的强度（原为 10 次）。
- 所有 POST API 要求请求头 `X-Requested-With: mangagate`，否则 403（防跨站表单）。

### 6.3 OPDS 细节

- 命名空间 `http://www.w3.org/2005/Atom`，类型
  `application/atom+xml;profile=opds-catalog;kind=navigation|acquisition`。
- acquisition link：`rel="http://opds-spec.org/acquisition"`，
  type 为 `application/vnd.comicbook+zip` / `application/vnd.comicbook-rar` /
  `application/pdf` / `application/zip`。
- 封面：`rel="http://opds-spec.org/image"` 与 `.../image/thumbnail`。
- 所有 href 用 `baseURL(r)` 生成绝对地址（`http://Host:Port/...`），
  不依赖反向代理头，除 `X-Forwarded-Proto: https` 外不解析别的。
- **Range 是硬需求**：可达阅读器的「流式阅读」依赖 206 响应，改动下载逻辑时
  必须保留 `http.ServeContent` 或等价实现。

## 7. 关键决策与原因

### 7.1 App 走独立端口，不走统一网关

实测 fnOS 1.2.0701：请求 `/app/{appname}/` 由 `/var/run/trim_http_cgi.socket`
处理，没有 NAS 会话时返回 `200 invalid token`；带 Basic Auth、Bearer、假 cookie
都不放行。iOS App 是原生 HTTP 客户端，拿不到 NAS 会话，因此不可能穿过网关。

> 不要尝试改 `/usr/trim/nginx` 或 `trim_http_cgi` 放行：属于修改系统组件，
> 升级会丢失，而且影响全局。

统一网关只用于桌面 iframe 管理页（浏览器有登录态）。这是刻意的分工。

### 7.2 Go 静态双架构单包

- 一个 fpk 同时放 `mangagate-arm64` 与 `mangagate-amd64`，
  `cmd/main` 用 `uname -m` 选择，manifest 写 `platform=all`。
- 好处：发给别人时不用区分设备；无需在 NAS 上装运行时。
- 约束：**保持 `CGO_ENABLED=0`**。当前依赖（bcrypt、image）全是纯 Go；
  想做 RAR 解压/HEIC 解码会引入 CGO 或大依赖，需要重新评估。

### 7.3 授权目录以系统为准

- `manifest: disable_authorization_path=false`，用户在应用设置里授权目录，
  平台通过 `TRIM_DATA_ACCESSIBLE_PATHS`（冒号分隔）传给启动脚本。
- `resolveRoots` **只认当前授权路径**，不再信任历史保存的路径（撤销授权要生效）。
- `cfg.Root` 只是「用户选中的根目录」偏好：优先匹配授权列表，其次列表第一项；
  只有在完全没有授权路径时才回退到 `cfg.Root`（本地开发用）。
- 授权变更后**必须重启应用**，`cmd/config_callback` 是空的（与 zcode 一致，
  平台不会自动重启）。

### 7.4 封面策略

- zip/cbz：按名称自然排序取第一张图片；跳过 `__MACOSX/` 和隐藏文件。
- 图片目录：取第一张图。
- cbr/rar：不解析，使用内置占位图（灰卡 + 几何图形，无文字，避免字体依赖）。
- 解码支持 jpeg/png/gif/bmp/webp；heic/avif 会失败并回退到占位图。
- 缓存：`$TRIM_PKGVAR/covers/{bookID}.jpg`，mtime 不早于书文件 mtime 时复用。
- 缩放：最长边 320，CatmullRom，JPEG q=82。

### 7.5 图片目录的下载

`Format="dir"` 的书在 `/file` 上是**边读边打 zip**，没有 `Content-Length`，
不支持 Range。这是已知限制：这类库建议让用户把图片打成 cbz。

### 7.6 授权目录与自动重启

`TRIM_DATA_ACCESSIBLE_PATHS` 只在进程启动时读取，平台改授权目录不会通知运行中的
进程。`cmd/config_callback` 在应用设置保存后执行 `stop` →（原本运行时）`start`，
让新的授权路径和 TLS 路径立即生效。若平台没有触发该回调，手动停止/启动即可。

两个实现细节：

- 回调脚本若被 root 手动执行，会先用 `setpriv` 降到 `mangagate` 再操作，否则
  `saveConfig` 会把 `config.json` 写成 `root:root 600`，下次以 `mangagate`
  启动就会 `permission denied`，表现为「点启用没反应」。
- `cmd/main` 的 `status/stop` 在 PID 文件缺失时会用 `pgrep -x <二进制名>` 兜底，
  避免旧进程变成孤儿后被重复启动（端口占用、日志里出现两次 starting）。

### 7.7 公网访问与 TLS

- fnConnect 的 `*.fnos.net` 域名由中继转发，只开放飞牛门户端口；第三方应用的
  5080 端口不可达（实测 5667/5005/5006/5080 均超时），不要宣称该域名可用。
- 公网需自有域名 + 端口放行。应用支持同一端口 HTTP/HTTPS 双协议（`tls.go` 按
  首字节 `0x16` 识别 TLS），避免破坏局域网现有 `http://` 客户端。
- 证书不自动申请：80/443 被 fnOS nginx 占用，无法做 HTTP-01/TLS-ALPN-01。
  证书路径通过应用设置 `tls_cert` / `tls_key` 传入，留空即纯 HTTP。

## 8. fnOS 平台坑（重点）

### 8.1 `install-fpk` 不会升级

对已安装应用，`appcenter-cli install-fpk` 只输出
`Application [x] is installed.`，即使 fpk 版本号更高、应用已 stop，
也不会替换文件。正确流程：

```bash
sudo appcenter-cli uninstall mangagate
sudo appcenter-cli install-fpk mangagate.fpk -e /tmp/mg.env
sudo appcenter-cli start mangagate
```

卸载会保留 `@appconf / @apphome / @appdata / @appmeta`，配置、账号、home 里的
数据都在，**但 `appcenter` 数据库里的授权目录记录会随卸载删除**。重装后需要
重新在「应用设置 → 授权目录」里授权一次（文件系统层面的权限可能仍然保留，
但这不该被依赖）。验证是否真的换了：

```bash
grep '^version' /var/apps/mangagate/manifest
ls -l --time-style=long-iso /vol1/@appcenter/mangagate/server/
```

**每次发版必须递增 manifest.version**，否则分发出去的包无法被识别为更新。

### 8.2 向导变量

`wizard/install` 的字段名直接变成生命周期脚本的环境变量：
`admin_user`、`admin_pass`、`port`。`cmd/main` 用 `${port:-...}` 取端口。

`ensureUsers` 的规则：只有在用户不存在时用向导值创建；已有账号**不会**被
启动时的旧向导值覆盖。所以改密码请走管理页，不要在 wizard/config 里加密码字段
（会被每次提交覆盖，而且和用户预期冲突）。

### 8.3 运行身份与路径

- `run-as: package`，用户/组均为 `mangagate`，无特权。
- 二进制在 `target/server/`，socket 在 `target/` 下（`gatewaySocket` 只写文件名）。
- 生命周期脚本在 `/var/apps/{appname}/cmd/`，**不在** `TRIM_APPDEST/cmd`；
  脚本里要用 `$(dirname "$0")` 定位同目录的 `main`。
- `healthz` 必须免认证，否则 `cmd/main` 无法在不持有密码的情况下判断启动成功。

### 8.4 文件权限

在本工作区新建的文件会继承 `000` 权限（NAS 的 ACL/umask 行为），
`Write`/`Edit` 后会读不了。构建前统一：

```bash
chmod -R u+rwX,go+rX .
```

`build.sh` 里已对产物做 755/644 修正；注意 `chmod -R` 会清掉 `build.sh`
自身的可执行位，需要单独 `chmod +x build.sh`。

### 8.5 网络

- `proxy.golang.org` 不通，`goproxy.cn` 可用；`build.sh` 默认
  `GOPROXY=https://goproxy.cn,direct`、`GOSUMDB=off`。
- Go 官方下载走 `dl.google.com`（可用），解压到 `tools/go`。
- 开发机为 aarch64 fnOS（LAN IP 以实际为准），fnOS 1.2.0701。

## 9. 测试与验收

### 9.1 自动化

```bash
python3 tools/smoke_test.py --base http://127.0.0.1:5080 \
    --user admin --pass test123456
```

覆盖：healthz 免认证、匿名 401、错误密码 401、catalog 结构、系列/书目链接、
Range 206、完整下载、封面 JPEG、`/dav/` 已移除返回 404、管理页、status API，
以及 `--mutate` 下 Basic Auth 调用管理 API 必须返回 403。
所有检查必须 PASS，退出码 0。

加 `--mutate` 会额外创建/删除一个临时用户，随后自动清理。

加 `--gateway-admin <socket 路径>`（需以 `--socket` 启动测试实例）会模拟
fnOS 网关管理员：设置/复位某用户的目录权限，验证受限用户在 catalog 里看不到
未授权系列、直接访问系列/下载链接返回 404；并检查 `/api/log`、`/api/export`。
再加 `--scrape` 会起一个本地 mock Bangumi（`--scrape-base http://127.0.0.1:<随机>`
启动测试实例），走完 搜索 → 匹配 → OPDS 展示 → 清除 全链路。

加 `--data <运行数据目录>` 会检查 `library.json` 缓存已生成且 root 匹配。

### 9.2 测试数据

```bash
./tools/go/bin/go run tools/gen_sample.go --out testdata/library
```

生成 8 个系列、10 本书、58 页：压缩包系列、单本、嵌套图片目录、根级散图
目录、嵌套分类目录（`混合集/悬疑`、`混合集/异世界`，用于目录树授权测试）、
EPUB 样本。覆盖了扫描器的所有分支。

### 9.3 手工检查清单（发版前）

- [ ] `smoke_test.py` 全 PASS
- [ ] 桌面入口能打开管理页（网关模式，靠 `X-Trim-Username`）
- [ ] 管理页显示正确的 LAN IP 与 OPDS 地址
- [ ] 普通 NAS 用户桌面看不到入口；管理员能打开并管理
- [ ] Basic Auth 应用账号调用 `/api/users`、`/api/root`、`/api/rescan` 返回 403
- [ ] 点「重新扫描」后右下角出现扫描完成提示
- [ ] 未授权目录时显示「未就绪」，不会崩溃
- [ ] 授权目录后重启，能扫描出真实库
- [ ] 在可达阅读器里能列出系列、封面、打开一本并翻页（**只能真机验证**）

## 10. 当前部署状态（本机）

| 项 | 值 |
|---|---|
| 已安装版本 | 0.3.0（热替换二进制；应用中心仍显示 0.1.2，下次正式重装后对齐） |
| 端口 | 5080 |
| 管理员 | `admin`（密码未记录；另有 NAS 侧管理员） |
| 漫画库 | 已授权目录中的示例测试库（5 系列 / 5 本 / 13 页） |
| config.root | 同上；授权记录在 appcenter 数据库 app_id 27 下 |
| 运行用户 | `mangagate` |
| 扫描缓存 | `/vol1/@appdata/mangagate/library.json` |
| 自检 | `tools/smoke_test.py` 全 PASS（含 --gateway-admin、--data 权限/缓存检查） |

**下一步要做的**：

1. 用户在可达阅读器真机验证 OPDS 兼容性（最大未验证项）。
2. 公网 HTTPS：用自有域名的证书填入应用设置。
3. 按需为家庭账号配置只读账号和可见系列。

## 11. 已知限制

- 无内置 HTTPS 证书申请；Basic Auth 明文 HTTP 仅适合局域网（可配置自有证书）。
- RAR/CBR 不解析，封面为占位图；页数未知。
- 图片目录动态 zip 不支持 Range。
- HEIC/AVIF 被当作图片扫描，但 Go 解码器不支持，纯 HEIC 目录封面为占位图。
- 系列权限按**目录路径**（相对根目录）绑定：`User.Dirs`，一条授权覆盖该目录
  子树（含以后新增的系列）。目录改名或换根目录后路径失效，保存权限时自动
  归并（父覆盖子）并清除失效路径。多授权目录时路径带书库前缀（`书库名/…`），
  单目录时保持无前缀（0.2.x 兼容）。漫画库顶层散图（系列 Rel 为 `/`）不属于
  任何可授权目录，受限用户不可见。
- `rescan` 会新建 `library` 对象，旧请求可能短暂看到旧快照（可接受）。
- 搜索是内存子串匹配，没有分页。

## 12. 修改指南

### 加一个配置项

1. `Config` 结构加字段 + JSON tag，`loadConfig` 给默认值。
2. 需要用户输入就从 `wizard/install` 加字段（或用 `TRIM_*` 环境变量）。
3. `saveConfig` 持久化；管理页需要的话加 API + 前端表单。

### 加一个 OPDS 端点

在 `handleOPDS` 的 switch 里加分支，复用 `newFeed` / `seriesEntry` / `bookEntry`。
所有绝对链接用 `baseURL(r)`，不要硬编码 host。

### 改扫描规则

只改 `scanDir`，保持「一个目录要么是 Series 要么产出一本 dir Book」的模型，
否则 `library.scan` 的统计与封面选择会不一致。改完跑 `gen_sample` 重新验证。

### 发版

1. `pkg/mangagate/manifest` 与 `src/main.go:appVersion` **同时**递增版本号。
2. 更新 manifest 的 `changelog`。
3. `./build.sh`。
4. 本机：uninstall → install → start → `smoke_test.py`。
5. 分发新的 `pkg/mangagate/mangagate.fpk`。

### 不要做

- 不要引入 CGO、不要依赖 NAS 上的 Python/Node。
- 不要改 `/usr/trim`、`/usr/trim/nginx` 或系统服务。
- 不要把密码写进日志、URL 或 manifest。
- 不要让管理 API 接受 Basic Auth 账号（管理只认网关的 NAS 管理员）。
- 不要移除 `/healthz` 的免认证（`cmd/main` 依赖它）。
