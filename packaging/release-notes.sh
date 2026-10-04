#!/usr/bin/env bash
# Writes the release notes that go on the release page.
#
# The download table is generated from the artifacts that were actually built
# rather than typed, so a platform that failed to build cannot leave a row
# pointing at a file that is not there.
set -Eeuo pipefail

VERSION="${1:?usage: release-notes.sh <version> <commit>}"
COMMIT="${2:-}"
DIST="${DIST:-dist}"
v="${VERSION#v}"

cat <<EOF
# ncm-studio ${VERSION}

网易云音乐 \`.ncm\` 解密工具：Go 单文件、无任何依赖、自带网页界面。解密的同时抓歌词写进
音频标签（FLAC 写 Vorbis comment，MP3 写 ID3v2），并可选生成同名 \`.lrc\`；封面可以逐个
换、也可以按文件名/标签自动补全。

**它只在本机跑**：默认只监听 \`127.0.0.1\`，没有账号、没有上传，除了抓歌词和封面之外
不发起任何网络请求。

## 下载

| 平台 | 文件 | 用法 |
| --- | --- | --- |
EOF

emit() { # emit <file> <platform> <how>
  [ -f "$DIST/$1" ] || return 0
  printf '| %s | `%s` | %s |\n' "$2" "$1" "$3"
}

emit "ncm-studio-${v}-windows-amd64.zip" "Windows x64" "解压后双击 start-ncm-studio.bat"
emit "ncm-studio-${v}-windows-arm64.zip" "Windows on ARM" "同上"
emit "ncm-studio_${v}_amd64.deb" "Debian / Ubuntu" "\`sudo apt install ./ncm-studio_${v}_amd64.deb\`"
emit "ncm-studio-${v}-x86_64.AppImage" "Linux（免安装）" "chmod +x 后直接运行，双击也行"
emit "ncm-studio-linux-amd64" "Linux x64 单文件" "\`./ncm-studio-linux-amd64\`"
emit "ncm-studio-linux-arm64" "Linux arm64 单文件" "同上"
emit "ncm-studio-linux-arm" "Linux armv7 单文件" "同上（树莓派等）"
emit "ncm-studio-darwin-amd64" "macOS Intel" "首次运行需右键 → 打开"
emit "ncm-studio-darwin-arm64" "macOS Apple Silicon" "同上"
emit "ncm-studio-freebsd-amd64" "FreeBSD x64" "\`./ncm-studio-freebsd-amd64\`"
emit "ncm-studio-freebsd-arm64" "FreeBSD arm64" "同上"
emit "ncm-studio-android-arm64" "Android / Termux" "在 Termux 里运行，浏览器开 127.0.0.1:8080"

cat <<EOF

所有单文件版本都**不带扩展名也可以直接跑**：它们没有任何动态依赖，拷到哪都能用。
\`checksums.txt\` 里是每个文件的 sha256。

## 运行

EOF

cat <<'EOF'
```bash
# 最简单的用法：扫主目录，输出到 ~/ncm-output，界面在 http://127.0.0.1:8080
./ncm-studio-linux-amd64

# 指定目录与并发
./ncm-studio-linux-amd64 -dir ~/Music -output ~/Music/out -workers 4

# 让同一局域网里的手机也能打开界面
./ncm-studio-linux-amd64 -host 0.0.0.0
```

| 参数 | 默认 | 说明 |
| --- | --- | --- |
| `-host` | `127.0.0.1` | 监听地址；`0.0.0.0` 才会对局域网开放 |
| `-port` | `8080` | 网页端口 |
| `-dir` | 主目录 | 扫描 `.ncm` 的目录 |
| `-output` | `<dir>/ncm-output` | 解密输出目录 |
| `-workers` | `3` | 同时解密几个文件（1–16） |
| `-lyrics` | `both` | `off` / `embed` / `both` / `file` |
| `-workspace` | 程序所在目录 | 封面库、歌词缓存的存放位置 |
| `-config` | 系统配置目录 | 设置文件位置 |
| `-version` | | 打印版本号 |

## 这一版里有什么

- **扫描大小过滤**（设置页）：数字 + KB/MB/GB 单位，默认跳过小于 500 KB 的文件；
  这类文件基本都是没下完或损坏的。被跳过的行仍然列出来并写明原因，不是悄悄消失。
- **封面按名称补全**：没有封面的文件用标签和文件名去搜索，匹配确定后写入官方封面；
  不确定的标成「待确认」不写。可以随时停止。
- **换封面不再重写整个文件**：元数据链里有 padding 时原地改写，实测 40 MB 的曲子
  只写 0.08 MB；塞不下才回退整文件重写。
- **扫描每个文件的开销降到 1/10**：元数据窗口按实际链长分配，只看「有没有封面」时
  不再把整张封面图复制进内存（每文件 1.1 MB → 106 KB 分配）。
- 改完封面后**列表里那一个文件的缩略图立即更新**，不用重扫页面。

## 校验

```bash
sha256sum -c checksums.txt
```

EOF

if [ -n "$COMMIT" ]; then
  printf '从提交 `%s` 构建。\n' "$COMMIT"
fi
