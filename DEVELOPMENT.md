# 可达书柜（mangagate）开发记录（面向后续维护者 / AI）

最后更新：2026-10-27，对应版本 **0.5.1**（源码仓库 `github.com/Bilibilimoon/keda-shelf`）。
当前发布渠道：GitHub Releases（每个 tag 挂 `mangagate.fpk`）。

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
git clone https://github.com/Bilibilimoon/keda-shelf.git ~/keda-shelf
cd ~/keda-shelf

# 构建（会自动下载 Go 到 tools/go，不会装进系统）
./build.sh

# 产物
pkg/mangagate/mangagate.fpk

# 本机安装
sudo appcenter-cli install-fpk pkg/mangagate/mangagate.fpk -e /tmp/mg.env
# /tmp/mg.env 内容：admin_user / admin_pass / port

# 单元测试（纯逻辑：清洗/归并/解析/扫描器/分页）
cd src && ../tools/go/bin/go test .

# 协议自检（需要账号密码，本地测试实例的用法见 §9.1）
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

## 4. 代码地图（src/，约 4600 行，含测试）

| 文件 | 职责 | 关键函数 |
|---|---|---|
| `main.go` | 参数解析、目录准备、用户初始化、库扫描（含缓存加载/后台重扫）、两个 listener、优雅退出、路由注册 | `main`, `routes`, `adminOnly`, `stripPrefix`, `resolvePort`, `ensureUsers` |
| `config.go` | `config.json` / `users.json` 读写、授权路径解析 | `loadConfig`, `saveConfig`, `resolveRoots`, `loadUsers`, `UserStore.*` |
| `auth.go` | `server` 结构体、Basic Auth、网关信任、按 IP 限速 | `requireAuth`, `gatewayAuth`, `rateLimiter` |
| `library.go` | 多根目录扫描、`Series`/`Book` 模型、页数统计与缓存沿用、异常收集、自然排序、扫描缓存（v2） | `library.scan`, `scanDir`, `makeBook`, `prevBook`, `saveCache`, `loadLibraryCache`, `naturalLess` |
| `epub.go` | epub OPF / cbz ComicInfo.xml 内嵌元数据提取、封面声明解析（含 URL 转义） | `archiveMetadata`, `epubMetadata`, `comicInfoMetadata`, `opfCoverPath`, `plainText` |
| `scrape.go` | 刮削：AniList/Bangumi 双源客户端、搜索词清洗、置信度判断、元数据覆盖层（含建议/忽略） | `newScraper`, `Search`, `Subject`, `cleanSearchTerm`, `scrapeConfident`, `metaStore.*`, `dirPerm` 在 opds.go |
| `opds.go` | OPDS 1.2 feed（根/书库/系列/最近添加/搜索，全部分页）、目录权限过滤、下载、封面路由、dc 元数据 | `handleOPDS`, `serveRootFeed`, `serveLibraryFeed`, `serveRecent`, `pageEntries`, `reqPerm`, `bookEntry`, `serveBookFile` |
| `cover.go` | 封面链（刮削封面 > 声明封面 > 首图 > 占位）、按书并发锁 + 总并发 4、320px JPEG 缓存、受控解码（`decodeLimited`）、zip 与 RAR/CBR 首图 | `coverJPEG`, `scrapedCoverJPEG`(opds.go), `extractCoverImage`, `imageFromZipEntry`, `firstImageInZip`, `firstImageInRar`, `decodeLimited`, `encodeCover` |
| `encoding.go` | 中文编码兼容：GBK/GB18030 显示名转换（zip 条目、本地文件名）、GBK 内嵌元数据解码 | `displayName`, `zipEntryName`, `xmlDecode` |
| `search.go` | OPDS 搜索：多关键词 AND、可搜索文本（书名/系列/目录/内嵌与刮削元数据） | `searchTerms`, `bookSearchText`, `bookSearchWithScrape`, `matchesAllTerms` |
| `tls.go` | 同端口 HTTP/HTTPS 协议识别、证书加载 | `loadTLSConfig`, `sniffListener` |
| `admin.go` | 管理页与 JSON API：状态/扫描进度/异常、用户与权限、刮削（搜索/匹配/批量/建议/忽略）、日志、导入导出、输入校验（`validUsername` / `validScrapeID`）、手动与自动扫描共用的 `runScan` | `apiStatus`, `api*User*`, `apiUserPerm`, `apiScrape*`, `runAutoScrape`, `applySubject`, `startScan`, `runScan`, `autoScanLoop`, `apiImport`, `apiExport`, `validUsername`, `validScrapeID` |
| `admin.html` | 管理页前端（内嵌 `go:embed`，相对路径请求） | — |
| `util.go` | MIME、ID、LAN IP 探测、日志尾部读取 | `hashID`, `detectLANIP`, `readLogTail` |
| `*_test.go` | 单元测试（清洗/置信度/归并/epub 解析/扫描器/多根前缀/异常/缓存回路/分页/受控解码/输入校验） | `go test .` |

### 数据模型

```go
Book   { ID, Title, Rel, Path, Format, Size, Pages, ModTime, SeriesID, Meta *bookMeta }
Series { ID, Name, Rel, Books []*Book, Cover }
library{ roots []string, name, series []*Series, books map[string]*Book,
         scannedAt, stats, problems []scanProblem }
```

- `ID = sha1(prefix + rel path)[:16]`，`book:` / `series:` 前缀区分。路径不变则 ID 稳定
  （权限与刮削匹配都绑 ID/Rel，改路径规则等于让用户重配）。
- `Rel` 是相对授权根目录的路径；**多授权目录时带书库前缀**（`书库名/子路径`），
  单目录时无前缀（兼容 0.2.x，保住已有权限/刮削数据）。`Path` 绝对路径，会进
  扫描缓存但只在本机有意义。
- `Meta` 是压缩包内嵌元数据；nil=还没提取过，空结构=提取过但没有（这个区分让
  缓存沿用不会反复重开没有元数据的包）。
- 扫描在后台 goroutine 里跑（缓存命中时启动即加载、后台校验），书架始终可访问，
  扫完原子换入。

## 5. 扫描与识别规则

规则集中在 `library.go:scanDir`：

1. 跳过隐藏项（`.` 开头）、`@eaDir`、`#recycle`、`$RECYCLE.BIN`、
   `System Volume Information`、`lost+found`、`Thumbs.db`、`desktop.ini`。
2. 跳过符号链接（`os.ModeSymlink`），防止逃出授权目录。
3. 目录内有压缩包（`.cbz/.zip/.cbr/.rar/.cb7/.pdf/.epub`）→ 该目录是一个 **Series**，
   每个压缩包是一本 **Book**。
4. 目录内只有图片且没有压缩包 → 整个目录是一本 **Book**（`Format="dir"`），
   归到父级 Series。
5. 有子目录 → 递归（最大深度 8），子目录各自成 Series。
6. 排序用 `naturalLess`，保证「第2卷」在「第10卷」之前。
7. zip/cbz/cb7/epub 的页数在扫描时统计（`countZipPages`），同时提取内嵌元数据
   （`archiveMetadata`）；RAR/PDF 不解包。
8. **缓存沿用**：mtime + 大小与上次一致的压缩包，直接沿用旧页数和旧元数据，
   不再开 zip——扫描慢的大头从来不是遍历目录而是逐个开压缩包。
9. **异常收集**：读不了的目录、打不开的压缩包记入 `problems`（上限 500 条），
   管理页「扫描异常」列表展示，不静默缺书。
10. **多根**：`library.roots` 是全部授权目录。只有一个根时 rel 无前缀（0.2.x
    兼容）；多个根时 rel 以 `根目录 basename` 为前缀，OPDS 根 feed 给每个书库
    一个 `/opds/v1.2/lib/{书库名}` 入口，权限树第一层即书库。
11. 扫描结果持久化到 `${TRIM_PKGVAR}/library.json`（v2 存 `roots` 数组；能读
    v1 单根旧缓存）。启动时缓存命中且根一致 → 立即服务 + 后台重扫；不一致 → 全量扫。
12. **显示名编码**：`Rel`/`ID` 始终用文件系统原始字节（权限和刮削都绑路径）；
    只有 `Series.Name` / `Book.Title` 经 `displayName` 转换——非 UTF-8 的
    GBK 字节转成中文，合法 UTF-8 原样。zip 条目名按 `NonUTF8` 标志同样处理
    （仅影响显示与自然排序，打开文件仍用原始条目名）。
13. **自动增量扫描**（0.5.0）：`runScan(auto=true)` 每 `auto_scan_minutes`
    （默认 10，0 关闭）跑一次。增量体现在两处：`prevBook` 让未变化的压缩包
    沿用页数/元数据；扫描完用 `library.fingerprint()` 对比快照，完全相同就
    丢弃新结果、不替换服务中的库、不写缓存（日志一行 "no changes"）。
    指纹含书 ID/标题/路径/大小/页数/mtime/元数据与扫描异常，扫描时间本身
    不参与，避免无意义的 feed `updated` 抖动。

## 6. 协议契约

### 6.1 端点

| 路径 | 说明 |
|---|---|
| `/healthz` | 免认证，返回 `{status,ready,series,books,uptime}` |
| `/opds`、`/opds/` | 与 `/opds/v1.2/catalog` 相同的根 feed |
| `/opds/v1.2/catalog` | 根导航：全部系列 + 最近添加 + 书库入口（多根时）+ 系列（分页） |
| `/opds/v1.2/series` | 全部系列（navigation，分页 `?page=N`，每页 100） |
| `/opds/v1.2/series/{id}` | 某系列的书籍（acquisition） |
| `/opds/v1.2/series/{id}/thumbnail` | 系列封面（刮削封面优先） |
| `/opds/v1.2/lib/{书库名}` | 单个授权目录的系列（多根时） |
| `/opds/v1.2/recent` | 最新入库 50 本（acquisition） |
| `/opds/v1.2/books/{id}` | 单书 feed |
| `/opds/v1.2/books/{id}/file` | 下载；压缩包走 `http.ServeContent`（支持 Range），`dir` 动态打 zip |
| `/opds/v1.2/books/{id}/thumbnail` | 封面 JPEG（刮削 > 声明 > 首图 > 占位） |
| `/opds/v1.2/search?q=` | 书名/系列名/目录/作者/标签/简介子串搜索（空格分词，全部命中；分页）。刮削补全的标题、作者、标签也参与匹配 |
| `/opds/v1.2/opensearch.xml` | OpenSearch 描述；描述风格书架的搜索入口 |
| `/api/status` | 登录即可读；含扫描进度 `scan_progress`、异常数 `problems` |
| `/api/users`、`/api/users/{add,delete,password,perm}` | 仅 NAS 管理员；`perm` 为目录树权限 |
| `/api/series`、`/api/rescan`、`/api/scan/problems` | 仅 NAS 管理员 |
| `/api/scrape/{status,search,apply,remove,ignore,auto}` | 仅 NAS 管理员；刮削全流程 |
| `/api/log`、`/api/export`、`/api/import` | 仅 NAS 管理员；运维与备份 |
| `/` | 管理页（相对路径请求 API，兼容网关前缀） |

所有系列/书目的可见性都经 `reqPerm(r)`（目录权限过滤）：根 feed、系列列表、
书库 feed、搜索、系列详情、单书、封面、**下载**全部过滤，未授权一律 404。

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
- **全部授权目录同时生效**（0.3.0 起），没有「选根目录」这回事；`cfg.Root` 仅在
  完全没有授权路径时作为本地开发回退。
- 授权变更后**必须重启应用**，`cmd/config_callback` 负责在应用设置保存后
  `stop` →（原本运行时）`start`。

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

### 7.8 刮削（0.2.0 引入，0.3.0 批量化）

**数据源是硬约束**：开发机的家宽网络实测 `api.bgm.tv`（Bangumi）DNS 被污染且
连接阻断，而 `graphql.anilist.co`（AniList）完全可用。因此：

- 双源实现（`scraper` 接口）：默认 **AniList**（日文原名 + 英文简介，多数网络
  可达）；**Bangumi**（中文元数据最好）可通过应用设置 `scrape_source` 切换，
  并支持 `scrape_proxy` 走代理（`http.Transport.Proxy`）。
- AniList 对**中文关键词命中率很低**（synonyms 覆盖差），这是已知体验短板，
  UI 引导用户改用日文原名/罗马字搜索，不要试图用模糊匹配去"修"它。
- 覆盖层 `series-meta.json`（`metaStore`）：系列级 + 书级匹配、建议（Suggested）、
  忽略（Ignored）。**独立于扫描缓存**——缓存重扫会重建，人工确认的匹配不能丢。
- 匹配确认流程：`applySubject()` 拉详情（含作者/标签/封面 URL）→ 下载封面到
  `covers/scrape-{level}-{id}.jpg` → 存覆盖层。封面下载失败不阻断，只是没有封面。
- **批量尝试**（`runAutoScrape`）逐系列搜索，限速 700ms/个（AniList 限 90 req/min，
  每个系列 search+detail+cover 三个请求）。置信度 `scrapeConfident` 刻意保守：
  归一化后完全相等，或字符集重合 ≥0.9 且长度差 ≤2 才自动应用；中日简繁差异
  （进击的巨人 vs 進撃の巨人）一律降为「待确认建议」，宁多人工确认不错配。
- 搜索词清洗 `cleanSearchTerm`：去 `[汉化组]`、卷话尾缀（第N卷/vol.N/V2）、扩展名。
- epub 内嵌元数据（`epub.go`）优先级高于文件名，刮削结果又高于内嵌；
  封面链：刮削封面 > epub 声明封面（`cover-image` property，含 URL 转义处理
  和 EPUB2 meta cover 回退）> 压缩包首图 > 占位图。

### 7.9 多书库与路径前缀（0.3.0）

多授权目录时系列 rel 带书库前缀（`书库名/子路径`），单目录时无前缀——这个
**条件前缀**是刻意的兼容：单目录用户从 0.2.x 升级，系列 ID 与权限/刮削绑定
的路径全部不变。顶层散图系列在单根时保持 `Rel="/"`、`Name=库名`（0.2.x 形状），
多根时 `Rel=书库名`（整库可勾选授权）。代价：将来单根→多根的切换会让系列 ID
变化（新库等于新配），可接受。

### 7.10 导入导出

导出（`apiExport`）打包 config + users + **scraped 覆盖层** + 日志尾部；导入
（`apiImport`）只恢复用户（含权限）、书架名、刮削文本信息——**封面文件不在
导出里**（导入后显示原首图封面），端口与授权目录归平台管不导入。用户列表
同名覆盖、异名保留。

### 7.11 图标

`tools/icon.png` 是图标源（随仓库），`gen_icon.go` 存在它时缩放出
ICON.PNG / ICON_256.PNG / ui/images/icon_{64,256}.png 四个尺寸，否则绘制
默认书本图形。注意 `gen_icon.go` 依赖 `x/image`，必须在 `src/` 模块目录下
运行（`build.sh` 已处理）。桌面图标读 `ui/images/`，应用中心读 `ICON*.PNG`；
**换了图标必须重跑 build.sh 重新打包**，否则 release 里的 fpk 还是旧图。
另一台设备上应用中心图标不刷新是**前端浏览器缓存**，强刷/无痕即恢复。

### 7.12 安全模型与 0.4.0 加固

先说已经是对的架构（改动时不要破坏）：

- **无路径穿越**：所有书目访问走 `hashID`（sha1 前缀 16 hex）查内存索引，
  从不拿用户输入拼文件路径；`findBook`/`findSeries` 查不到就 404。
- **symlink 不跟随**（`scanDir` 跳过 `os.ModeSymlink`，深度上限 8）。
- **管理 API 只在 Unix socket 侧可达**（0660，fnOS 网关注入 `X-Trim-Isadmin`）；
  TCP 侧 Basic Auth 用户永远 `Admin=false`。`encoding/xml` 无 XXE 风险。
- 密码 bcrypt、`users.json` 0600 原子写、登录限速 3 次/分钟/IP。

0.4.0 修复的问题（都有对应单测，见 `cover_test.go`）：

1. **解压炸弹（中危）**：恶意 cbz 里塞一张头声明 8000×6000 的 PNG 能让
   `image.Decode` 申请上百 MB 内存。现在所有解码走 `decodeLimited`：
   先 `io.LimitReader` 限编码数据 32 MB，再 `image.DecodeConfig` 预检像素
   （上限 4000 万 ≈ 7900×5100，比 8K 大），过了才真正 `Decode`。
   zip entry 的流不可回卷，所以先 `ReadAll` 到内存再重复 `bytes.NewReader`。
2. **用户名注入（中危）**：管理页用 `escHtml` 转义用户名后拼进
   `onclick="f('...')"`，但 `&#39;` 在 HTML 属性的 JS 上下文里会被解码回 `'`，
   含引号的用户名可注入前端脚本。根治是 `validUsername`：仅字母数字与
   `._-`、≤32 位，`apiAddUser` / `apiImport` / `ensureUsers` 三处入口都校验。
3. **刮削 ID 穿越（低危）**：`apiScrapeApply/Remove/Ignore` 的 id 会拼进
   封面文件路径，加 `validScrapeID`（hex 字符集、≤64 位）+ `filepath.Base` 兜底
   （`opds.go:scrapedCoverJPEG`、`scrape.go:metaStore.remove` 同样处理）。
4. **导入绕过权限白名单（低危）**：`apiImport` 恢复用户权限时现在过
   `normalizePermDirs(iu.Dirs, s.grantedDirs())`，备份文件里的越权路径会被清掉。

### 7.13 自动增量扫描（0.5.0）

用户不想每次手动点「重新扫描」。设计取舍：

- **定时轮询而不是 fsnotify**：inotify watch 在几万目录的漫画库上会撞
  `max_user_watches`，还要自己维护递归 watch；定时全量 walk 的主要成本
  只是 `os.ReadDir` + `stat`（未变化压缩包的页数走缓存，不打开 zip），
  小库 0–2ms、十万文件也在秒级。默认 10 分钟，可配 0 关闭。
- **无变化不替换**：新扫描结果与当前快照做 `fingerprint()` 比较（排序后的
  ID/标题/路径/大小/页数/mtime/元数据/异常），相同就丢弃，不写
  `library.json`。否则每 10 分钟一次原子替换会让所有 OPDS feed 的
  `updated` 变化、客户端缓存失效。
- 与手动扫描共用 `runScan` + `scanning` 标志，天然不会叠跑。
- 手动扫描永远替换并写缓存（用户点按钮时期望立即生效，哪怕结果相同）。

### 7.14 RAR/CBR 封面（0.5.0）

引入 `github.com/nwaples/rardecode/v2`（纯 Go，支持 RAR4/RAR5，含 solid
压缩包），约束得以保持：**不新增 CGO 和外部 `unrar` 依赖**。

- 封面取第一个图片条目（自然顺序不保证，取归档顺序第一个可解码图片）。
- 顺序 `Next()` 读取是 solid 归档唯一可行方式；遇到加密/链接/目录条目跳过。
- 条目上限 2000，解码复用 `decodeLimited`（32 MB / 4000 万像素双重上限），
  恶意 RAR 不能拖垮进程。
- 页数**不统计**：逐页解压会让扫描变慢一个数量级，保持 `Pages=0`。
- 封面缓存 key 加 `-rar1` 后缀：升级前 RAR 只有占位图缓存，后缀让它们
  失效一次，其余格式的封面缓存不受影响。

### 7.15 中文 / GBK 兼容（0.5.0）

国内 Windows 工具产出的 zip 常在未设 UTF-8 标志（`0x800`）时写 GBK 字节；
Linux 把文件名当不透明字节保存，直接显示就是乱码。处理原则：

- **只在显示层转换**。`Path`/`Rel`/`ID` 必须保持原始字节：权限、刮削匹配、
  扫描缓存、`os.Open` 全部依赖它。`displayName()` 只在字符串不是合法
  UTF-8 时尝试 GBK → GB18030，失败原样返回。
- zip 条目名用 `FileHeader.NonUTF8` 判断；`NonUTF8=true` 也可能是 CP437，
  GBK 解码失败就保留原文（仅影响显示与排序）。
- 内嵌元数据（`ComicInfo.xml`、epub 的 OPF/container.xml）经 `xmlDecode`：
  先按 UTF-8 解析，失败则 GBK/GB18030 转换后把 XML 声明里的 encoding 改成
  UTF-8 再解析。声明为 gbk 但字节是 UTF-8 的怪文件也能处理。
- 不做半角/全角、简繁归一；搜索和排序仍按字节序，中文书名之间的自然排序
  依赖 `naturalLess` 的数字段比较。

### 7.16 git 与隐私（开源）

- 仓库：`github.com/Bilibilimoon/keda-shelf`，main 分支直接提交，发版打 tag
  （`v0.3.0` 格式）。`.gitignore` 排除 Go 工具链、构建产物、备份、testdata。
- **每次提交前必须过隐私扫描**（本机 zcode 的 `git-privacy-check` skill）：
  已知敏感词（个人域名/内网 IP/NAS 账号/私人路径/IPv6 前缀）+ 通用模式
  （密钥/令牌/邮箱/IP）。首次推送做了全仓扫描；日常提交扫 staged diff。
  高危命中必须先脱敏（占位符替换）再提交。
- 文档里的 fnOS 系统路径（`/vol1/@app*`、`/var/apps`）是所有 fnOS 机器相同的
  标准布局，不算隐私；**私人内容路径**（具体共享文件夹名）才算。

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

### 8.5 本机 `install-local` 的 644 竞争 bug

**仅本开发机**（fnOS 1.2.0701，另一台同版本 NAS 正常）：`install-local`
把 fpk 解包到 `/tmp/appcenter.pack.<ts>` 时根目录 mode 是 644（无 x 位），
导致 `fork/exec cmd/install_init: permission denied`，报错码 10237
（APP_INSTALL_FAILED_INSTALL_INIT_EXCEPTION）。真实报错只出现在
`/var/log/trim_app_center/error.log`，journalctl 里只有 sudo 记录。

绕过：后台跑一个无 sleep 的紧密 chmod 循环赢得竞争（`/tmp/chmodwatch.py`，
60 秒有效，重启后可能丢失可重建）：

```python
import glob, os, time
end = time.time() + 60
while time.time() < end:
    for d in glob.glob('/tmp/appcenter.pack.*'):
        try: os.chmod(d, 0o755)
        except OSError: pass
```

配套坑：卸载后 `appcenter` 数据库 `app_package` 表可能残留**孤儿记录**
（指向已消失的 /tmp 路径），会直接挡住重装，需手工
`DELETE FROM app_package WHERE app_name='mangagate'`；另外卸载可能残留
`mangagate` 系统用户/组（userdel/groupdel 清掉再装）。命令链里 groupdel
对不存在的组报错会 `&&` 短路后续命令，注意别用链式写法。

### 8.6 网络

- `proxy.golang.org` 不通，`goproxy.cn` 可用；`build.sh` 默认
  `GOPROXY=https://goproxy.cn,direct`、`GOSUMDB=off`。
- Go 官方下载走 `dl.google.com`（可用），解压到 `tools/go`。
- 开发机为 aarch64 fnOS（LAN IP 以实际为准），fnOS 1.2.0701。

## 9. 测试与验收

### 9.1 单元测试与冒烟测试

```bash
# 单元测试：清洗/置信度/权限归并/epub 解析/扫描器/多根前缀/异常/缓存回路/分页
cd src && ../tools/go/bin/go test .

# 冒烟测试（HTTP 黑盒，本地起一个测试实例）
(cd src && CGO_ENABLED=0 ../tools/go/bin/go build -o /tmp/mg-test .)
rm -rf /tmp/mgtest && mkdir -p /tmp/mgtest/etc /tmp/mgtest/var
/tmp/mg-test --config /tmp/mgtest/etc --data /tmp/mgtest/var --port 15080 \
    --socket /tmp/mgtest/mg.sock --init-user admin --init-pass test123456 \
    --roots testdata/library &
python3 tools/smoke_test.py --base http://127.0.0.1:15080 \
    --user admin --pass test123456 \
    --mutate --gateway-admin /tmp/mgtest/mg.sock \
    --data /tmp/mgtest/var --scrape
```

冒烟覆盖：healthz 免认证、匿名/错密 401、catalog 结构、书库入口（多根时）、
最近添加、OPDS 搜索（OpenSearch 描述、真实系列命中、空查询与无结果查询）、
系列/书目链接、Range 206、完整下载、封面 JPEG、epub 类型与内嵌
元数据、`/dav/` 404、管理页、status API、Basic Auth 对全部管理 API 403，
以及 `--gateway-admin`（模拟 fnOS 网关管理员）：目录权限设置/父目录覆盖/
复位、日志、导出；`--scrape`（配 `--scrape-source bangumi --scrape-base
http://127.0.0.1:15800` 启动实例）在本地 15800 起 mock 数据源，走完
搜索 → 匹配 → OPDS 展示 → 封面 → 清除 → **批量尝试 → 建议 → 忽略 →
导出/导入回路**。双根实例（`--roots a:b`）再跑一遍验证多书库与权限前缀。
所有检查必须 PASS，退出码 0。

### 9.2 测试数据

```bash
./tools/go/bin/go run tools/gen_sample.go --out testdata/library
```

生成 8 个系列、10 本书、58 页：压缩包系列、单本、嵌套图片目录、根级散图
目录、嵌套分类目录（`混合集/悬疑`、`混合集/异世界`，用于目录树授权测试）、
带完整 OPF 元数据与声明封面的 EPUB 样本（封面条目名带空格，顺便验证 URL
转义）。覆盖了扫描器的所有分支。

### 9.3 手工检查清单（发版前）

- [ ] `go test .` 与 `smoke_test.py`（单根 + 双根）全 PASS
- [ ] 搜索：OpenSearch 描述可访问；搜真实系列名有条目；空查询无结果
- [ ] 自动扫描：留库不动一轮日志 "no changes"；新增目录后下一轮自动出现
- [ ] RAR 封面：放一个 .cbr 到库里，封面不是占位图（纯灰卡）
- [ ] GBK：用 GBK 字节命名的目录/书名在 feed 里显示为中文
- [ ] 桌面入口能打开管理页（网关模式，靠 `X-Trim-Username`）
- [ ] 管理页显示正确的 LAN IP 与 OPDS 地址
- [ ] 普通 NAS 用户桌面看不到入口；管理员能打开并管理
- [ ] Basic Auth 应用账号调用任一管理 API 返回 403
- [ ] 点「重新扫描」显示进度、结束弹结果；坏压缩包出现在「扫描异常」
- [ ] 未授权目录时显示「未就绪」，不会崩溃
- [ ] 授权目录后重启，能扫描出真实库（第二次重启秒开，走缓存）
- [ ] 在可达阅读器里能列出系列、封面、打开一本并翻页（**只能真机验证**）

## 10. 当前部署状态（开发机）

| 项 | 值 |
|---|---|
| 已安装版本 | 0.5.1（重装升级；0.3.x 期间用热替换，见下） |
| 显示名 | 可达书柜（原名「漫画网关」，0.3.0 更名；appname 保持 `mangagate` 不变） |
| 端口 | 5080 |
| 漫画库 | 单个授权目录（示例测试库，5 系列 / 5 本） |
| 运行用户 | `mangagate` |
| 扫描缓存 / 刮削数据 | `/vol1/@appdata/mangagate/{library.json, series-meta.json}` |
| 图标 | `tools/icon.png`（随仓库），`gen_icon.go` 缩放四尺寸 |
| 源码仓库 | `github.com/Bilibilimoon/keda-shelf`（main，提交前过隐私扫描 skill） |
| 自检 | `go test` + `smoke_test.py`（单根/双根、权限、刮削 mock 全 PASS） |

**部署方式**：日常迭代用**热替换**（备份二进制 → `install` 到
`/var/apps/mangagate/target/server/` → `appcenter-cli stop/start`），不卸载、
不丢授权与数据；代价是应用中心信息要手工对齐（或下次正式重装时自然对齐）。
热替换后建议清一次浏览器缓存（admin.html 内嵌在二进制里）。

**下一步要做的**：

1. 用户在可达阅读器真机验证 OPDS 兼容性（最大未验证项）。
2. 公网 HTTPS：用自有域名的证书填入应用设置。
3. 按需为家庭账号配置只读账号和可见系列。

## 11. 已知限制

- 无内置 HTTPS 证书申请；Basic Auth 明文 HTTP 仅适合局域网（可配置自有证书）。
- RAR/CBR 用纯 Go 解出封面，但页数不统计（逐页解压太慢）；加密 RAR
  或首图格式不受支持时仍是占位图。
- 图片目录动态 zip 不支持 Range。
- HEIC/AVIF 被当作图片扫描，但 Go 解码器不支持，纯 HEIC 目录封面为占位图。
- PDF 无内嵌元数据（扫描版 Info 基本为空），只靠在线刮削。
- 系列权限按**目录路径**绑定：一条授权覆盖子树（含新增）；目录改名/换根后
  路径失效，保存时自动归并（父覆盖子）并清失效项。顶层散图（Rel `/`）在
  单根模式下不可授权（多根模式下归入书库层可授权）。
- 刮削：AniList 无中文元数据且中文关键词命中率低；Bangumi 在多数家宽不可达
  （需代理）。全自动匹配刻意不做（`scrapeConfident` 高门槛）。
- `rescan` 会新建 `library` 对象，旧请求可能短暂看到旧快照（可接受）。
- 批量刮削的限速（700ms/系列）意味着 500 个系列约跑 6 分钟，期间可随时停止。
- 自动扫描是全量目录遍历（未变化压缩包不重开，但每个文件仍会 stat）；
  超大库（数十万文件）可把间隔调大或设 0 改用手动扫描。
- GBK 转换只覆盖显示层，且不做简繁/全半角归一；GBK 以外的编码（Big5 等）
  仍会显示乱码。
- 搜索是内存子串匹配，没有分词和拼音/首字母搜索；词序必须与文本一致。

## 12. 修改指南

### 加一个配置项

1. `Config` 结构加字段 + JSON tag，`loadConfig` 给默认值。
2. 需要用户输入就从 `wizard/install` 或 `wizard/config` 加字段，并在
   `cmd/main` 的启动命令里透传成 `--flag`。
3. `saveConfig` 持久化；管理页需要的话加 API + 前端表单。

### 加一个 OPDS 端点

在 `handleOPDS` 的 switch 里加分支，复用 `newFeed` / `seriesEntry` / `bookEntry`
/ `pageEntries`。所有绝对链接用 `baseURL(r)`，不要硬编码 host；
入口类 feed 记得过 `reqPerm(r)` 权限过滤。

### 改扫描规则

只改 `scanDir`，保持「一个目录要么是 Series 要么产出一本 dir Book」的模型，
否则 `library.scan` 的统计与封面选择会不一致。**注意 Rel/ID 的稳定性**：
路径规则一变，用户已配置的权限和刮削匹配全部失效——加前缀类的改动要像
多根那样做兼容分支。改完跑 `gen_sample` + `go test` 重新验证。

### 发版

1. `pkg/mangagate/manifest` 与 `src/main.go:appVersion` **同时**递增版本号。
2. 更新 manifest 的 `changelog` 与本文档对应章节。
3. `./build.sh`。
4. 本机：`go test` + `smoke_test.py`（单根/双根）全 PASS；本机升级验证
   （注意 §8.5 的 644 竞争绕过）。
5. 提交 git（先过隐私扫描）并打 tag：`git tag vX.Y.Z && git push --tags`。
6. 用 GitHub API 建 Release 并上传 `pkg/mangagate/mangagate.fpk`
   （SSH key 不能调 API，需要细粒度 PAT，仅给本仓库 Contents 读写权限、
   短有效期，用完即删）。上传前对 fpk **解包跑一次隐私扫描**——构建产物
   不走 git，扫描 skill 看不到它。
7. release 说明列出主要变更；用户从 Releases 页下载 fpk 安装。

### 不要做

- 不要引入 CGO、不要依赖 NAS 上的 Python/Node。
- 不要改 `/usr/trim`、`/usr/trim/nginx` 或系统服务。
- 不要把密码写进日志、URL 或 manifest。
- 不要让管理 API 接受 Basic Auth 账号（管理只认网关的 NAS 管理员）。
- 不要移除 `/healthz` 的免认证（`cmd/main` 依赖它）。
- 不要在文档或代码里写入真实域名/IP/账号/私人路径（开源仓库，提交前有
  隐私扫描兜底，但别依赖兜底）。
- 不要改动系列 Rel/ID 的生成规则而不做兼容评估。
