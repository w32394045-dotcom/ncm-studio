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

## 性能取向：先量，再改

这台设备就是目标设备（Android arm64、FUSE 外置存储），所以性能问题一律先在手机上量。
测出来的基线（142.7 MB 的曲子，并发 3）：整轨解密约 2 秒、峰值 RSS 12.3 MB、
RC4 变换 460 MB/s、SHA-256 520 MB/s、目录遍历 400 个文件 0.5 秒。

已经改掉的两处，都是量出来才确认的：

1. **扫描每个文件的开销**。`tag.Inspect` 原来固定按 1 MB 窗口读元数据，读完再重试，
   而 `parseFLACBlocks` 会把 PICTURE 块的图像字节也复制一份。结果每读一个文件分配
   1.1 MB、耗时约 3 ms。现在：先用 4 字节块头把链的长度算出来，一次分配正好那么大的
   窗口（不再倍增重试）；只看「有没有封面」的调用把 PICTURE 的负载跳过不复制。
   实测**每文件 106 KB、1.0 ms**（分配降到 1/10，时间降到 1/3），整库扫描少几百 MB 垃圾。
2. **换封面要重写整个文件**。FLAC 链尾通常有 PADDING。现在新图塞得进旧图 + padding 的
   空间时就原地改写（只 `pwrite` 元数据那一段），塞不下才回退整文件重写。见
   `fitInPadding`，实测 40.3 MB → 写 0.08 MB。

量性能的工具是一次性的，测完就删了，没有留在仓库里：它们的路径写死了这台手机的存储
目录，放进仓库只会成为没人维护的负担。需要重测时照下面的形状再写一个就行——
`go run` 一个 main 包，对真实文件调用 `tag.Inspect` / `ncm.Open` / `tag.SetCover`，
用 `time.Since` 计时、`runtime.ReadMemStats` 看分配、`/proc/self/io` 看真实读写字节。
第二个数字是关键：只看时钟会被页缓存骗过去，只看结果会被「重写了但恰好一样大」骗过去。

## 发版

发版全在 GitHub 云端做，本机不需要工具链：

```bash
git tag v1.0.1 && git push origin v1.0.1
```

推 tag 会触发 `.github/workflows/build.yml`，依次跑测试 → 交叉编译十个平台 →
打 Windows 一键包 → 打 .deb 和 AppImage → 检查包内容 → 建 Release 并把产物挂上去。

也可以手动跑（Actions → build → Run workflow）：`publish` 不勾就只构建、不发版，
用来验证打包改动；勾上才会建 Release。手动跑不填版本就用 `日期-短commit`。

产物（15 个）：

| 文件 | 说明 |
| --- | --- |
| `ncm-studio-<v>-windows-amd64.zip` / `-arm64.zip` | 解压后双击 `start-ncm-studio.bat`，自动开浏览器 |
| `ncm-studio_<v>_amd64.deb` | `apt install ./…deb`，装 `/usr/bin/ncm-studio`，带菜单项和图标 |
| `ncm-studio-<v>-x86_64.AppImage` | 免安装，双击即跑（AppRun 里会 `xdg-open` 打开界面） |
| `ncm-studio-linux-{amd64,arm64,arm}` | 单文件，`arm` 是 armv7（树莓派） |
| `ncm-studio-{darwin-amd64,darwin-arm64}` | macOS Intel / Apple Silicon |
| `ncm-studio-{freebsd-amd64,freebsd-arm64}` | FreeBSD |
| `ncm-studio-android-arm64` | Termux 里直接跑 |
| `ncm-studio-windows-{amd64,arm64}.exe` | 裸 exe，zip 里的那个 |
| `checksums.txt` | 上面每个文件的 sha256 |

打包脚本在 `packaging/`，都是普通 shell，可以在任何 Linux 上单独跑：

```bash
VERSION=1.0.1 bash packaging/build-linux.sh        # .deb + AppImage
VERSION=1.0.1 bash packaging/build-windows.sh      # 一键 zip（需要 zip）
VERSION=1.0.1 bash packaging/check-linux-packages.sh  # 检查包内容
bash packaging/release-notes.sh 1.0.1 abc1234      # 发布说明
```

几个坑，都是踩过的：

- **`.deb` 的版本号不能以字母开头**：tag 是 `v1.0.0`，进 deb 之前必须去掉 `v`。
  版本在 workflow 的 `version` job 里统一归一化，脚本里也各自 `VERSION="${VERSION#v}"` 兜一层。
- **通过 API 上传的文件没有可执行位**：所以 workflow 里一律 `bash packaging/xxx.sh`，
  不要写 `./packaging/xxx.sh`。
- **`FOO=bar cmd` 这种前缀只在字面量时才算赋值**：`$extra` 展开出的 `GOARM=7`
  会被 shell 当成命令去找，写成 `env GOARM="$goarm" go build …` 才对。
- **`ldd` 把「不是动态可执行文件」写在 stderr**：只抓 stdout 会把好二进制判成坏的。


- 改文件用 PowerShell 的 `-replace` + `Set-Content` 会把非 ASCII 字符写成 mojibake，
  Go 源文件里全是中文注释和 `t("覆盖")` 这类字符串，**别用 PowerShell 改源码**。
  用编辑工具，或者从仓库里取回正确的那份再改。
- Windows 的 `os.Rename` 覆盖已存在文件会失败（`Access is denied`），
  所以 `internal/tag` 和 `internal/web` 里所有「重写后改名」的测试在 Windows 上会红。
  它们要在手机上跑，那里的文件系统语义才是真的。
- 测试夹具里的 FLAC 必须让音频帧**跟在元数据链后面**。先写文件再 append 音频会把
  音频塞到 PADDING 块后面，链就走不通了——这会让人以为是代码坏了。
