# NCM Studio（Windows 一键启动包）

解压后双击 **`start-ncm-studio.bat`** 就能用：

1. 弹出的黑窗口就是程序本体，关掉它（或按 Ctrl-C）就是退出。
2. 浏览器会自动打开 <http://127.0.0.1:8080>。
3. 第一次打开会先问三个目录：`.ncm` 放哪、解密后的音乐放哪、程序自己的数据（封面库、
   歌词缓存）放哪。之后随时能在设置里改。

没有安装步骤，没有运行库，没有账号，也不需要联网（只有抓歌词和封面时会连网易云）。
整个程序就是一个 `ncm-studio.exe`，把它单独拷走一样能用。

## 常用命令行参数

```
start-ncm-studio.bat -dir D:\music -output D:\music\out -workers 4
```

| 参数 | 默认 | 说明 |
| --- | --- | --- |
| `-dir` | 用户主目录 | 扫描 `.ncm` 的目录 |
| `-output` | `<dir>\ncm-output` | 解密输出目录 |
| `-workers` | 3 | 同时解密几个文件（1–16） |
| `-lyrics` | both | `off` / `embed` / `both` / `file` |
| `-port` | 8080 | 网页端口，被占用了就换一个 |
| `-host` | 127.0.0.1 | 改成 `0.0.0.0` 才允许局域网里的手机/平板访问 |
| `-workspace` | 程序所在目录 | 封面库、歌词缓存的存放位置 |
| `-version` | | 打印版本号后退出 |

## 手机（Android / Termux）

同一份代码有 `android/arm64` 的构建，在手机的 Termux 里直接跑
`ncm-studio-android-arm64` 即可，界面用手机浏览器打开 `http://127.0.0.1:8080`。

## 其它平台

发布页上还有 Linux 的 `.deb` 和 `.AppImage`、macOS 的 `darwin-amd64/arm64`、
FreeBSD 的 `freebsd-amd64/arm64`、Linux 的 `armv7`，都是单文件、无依赖。
