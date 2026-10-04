# 开发

主力平台是手机（Android arm64）和桌面，代码是纯 Go、**没有任何第三方依赖**
（`go.mod` 只有一行 module、一行 go 版本），前端是 `internal/web/static/` 下的
一个 `index.html` + 一个 `i18n.json`，用 `go:embed` 打进二进制。

## 构建与测试

```bash
go build ./...                       # 编译所有包
go test ./...                        # 全部测试
go vet ./...                         # 静态检查

go build -ldflags "-X main.buildVersion=1.0.0" -o ncmstudio .
GOOS=android GOARCH=arm64 CGO_ENABLED=0 \
  go build -ldflags "-X main.buildVersion=1.0.0" -o ncmstudio .
```

## 不装 Go 也能跑的两个静态检查

`internal/web` 里有两个测试会读前端的 `index.html` 和 `i18n.json`
（`TestEveryKeyTheUIRefersToExists`、`TestTranslationsCoverEveryKey`）。
在没有 Go 工具链的机器上改完前端之后，`tools/check-static.cjs` 是同样两件事的
Node 版本，几秒钟就能跑完：

```bash
node tools/check-static.cjs
```

它检查的是：

- `index.html` 里每一个 `t("…")` / `data-i18n` 的 key，五种语言（简中、繁中、英、日、韩）
  的表里都有；
- 五种语言的 key 集合完全一致（少一个 key 的语言会在界面上显示成 key 本身）；
- 这一轮新加的 key 确实在表里，而不是被总数掩盖过去。

改动前端之后先用它跑一遍，再跑 `go test ./...`。

## 目录

| 路径 | 作用 |
| --- | --- |
| `main.go` | 命令行参数、各页面用的 job pool 的组装 |
| `internal/ncm` | `.ncm` 容器解析与解密 |
| `internal/pipeline` | 一次解密的完整流程：解密 → 抓歌词 → 写标签 |
| `internal/tag` | FLAC / MP3 的标签读写（封面、歌词、平台 ID） |
| `internal/lyric` | 网易云接口客户端：搜索、歌词、缓存、限速 |
| `internal/backfill` | 已经解密好的文件的批量补歌词 / .lrc / AI 翻译 |
| `internal/cover` | 封面库（按音频指纹索引）与官方封面下载 |
| `internal/job` | 带进度、可取消的并发池 |
| `internal/store` | 设置、解密记录、指纹缓存 |
| `internal/web` | HTTP 路由、JSON API、内嵌的前端 |
| `internal/web/static` | 单页界面与五语言文案 |

## 约定

- 设置里存的是**值**，界面上的单位只是显示方式（大小过滤存字节数，
  顶上写着 KB/MB/GB 只影响怎么显示和怎么读回来）。
- 读设置文件里的字段要经过 `configMember()`：文件是个信封
  （`{"config":{…},"records":{…}}`），直接问信封「有没有 minSize」永远是没有，
  而那正是「用默认值」的意思——这里错过一次，保存过的过滤设置每次启动都会被重置。
- 目录扫描有两类结果：**过滤掉**（不列出）和**标记跳过**（列出但不能选）。
  音频列表用后者——一个文件无声消失比一个写着原因的灰行更难排查。
- 写文件都是先写 `.part` 再改名，中途断电不会毁掉原文件。`tag.SetLyrics` 和
  `tag.SetCover` 都会先取**按路径的锁**（`lockRewrite`）：两者都以 `<path>.part`
  为中转，同路径并发写会互相覆盖，最后两个 rename 有一个必然失败。
- 测试里凡是读 `/api/files` 或 `/api/audio` 的地方，如果造的是几 KB 的假音频，
  要在 setup 里把 `store.MinSize` 关掉（默认 500 KB），否则会被大小过滤挡掉。
