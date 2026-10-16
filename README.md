# ysp-live-go

Go 服务，为央视频直播频道生成 M3U/HLS 播放列表，并提供部分春晚点播入口。直播播放列表使用央视频 CDN 分片地址；春晚点播请求解析播放地址后重定向至 CDN。

## 要求

- Go 1.22 或更高版本

## 构建

在项目根目录执行：

```bash
go build -o ysp-live-go ./cmd/ysp-live
```

交叉编译 Linux ARM64：

```bash
mkdir -p dist
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -o dist/ysp-live-linux-arm64 ./cmd/ysp-live
```

## 运行

默认监听 `127.0.0.1:8767`：

```bash
./ysp-live-go
```

播放器订阅地址：

```text
http://127.0.0.1:8767/all.m3u
```

在局域网提供服务时，显式绑定所有网络接口：

```bash
./ysp-live-go -bind 0.0.0.0 -port 8767
```

服务没有用户认证。绑定 `0.0.0.0` 时，请通过防火墙限制访问范围，不要直接暴露到公网。

## HTTP 路由

| 路径 | 用途 |
| --- | --- |
| `/` | 频道列表 |
| `/all.m3u` | 包含直播和春晚点播的分组 M3U 列表 |
| `/health` | 健康检查 |
| `/diag` | 频道状态 |
| `/<slug>.m3u8` | 单频道播放列表 |
| `/vod/spring-gala/<slug>.mp4` | 春晚点播入口 |
| `/vod/2023-spring-festival.mp4` | 兼容旧版 2023 春晚订阅 |

春晚列表按年份升序收录 2020、2022、2023、2024 和 2026 年的完整晚会。

## 可选 4K 后端

4K 后端默认禁用。启用自动发现：

```bash
./ysp-live-go -backend auto
```

或指定后端二进制的绝对路径：

```bash
./ysp-live-go -backend /path/to/ysptp-project-arm64
```

后端默认使用 `8766` 端口，可通过 `-backend-port` 修改。该后端是独立的外部二进制，不包含在本项目中；启用前请自行确认其来源和行为。

## 项目结构

```text
cmd/ysp-live/
├── main.go          # 参数、启动和退出处理
├── server.go        # HTTP 路由和 M3U 输出
├── channels.go      # 直播频道和播放列表
├── gala.go          # 春晚点播源
├── ckey.go          # cKey 计算
├── protocol_jce.go  # JCE 协议编解码
└── backend.go       # 可选 4K 后端管理
```
