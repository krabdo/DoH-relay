# DoH-relay

仅使用 Go 标准库的轻量 DNS over HTTPS 反向代理。无第三方 Go 依赖、无 HTTP/3、无 DNS 缓存，不解析或改写 DNS 报文，包括 ECS / EDNS 信息。

部署结构：客户端 → 现有 HTTPS 反代（默认 TCP 443，HTTP/1.1 / HTTP/2）→ relay（内部 HTTP :8080）→ HTTPS DoH 上游（HTTP/2 或 HTTP/1.1）。证书由现有反代管理。

## 快速运行

```sh
docker run -d --name doh-relay --restart unless-stopped \
  --read-only --cap-drop ALL --security-opt no-new-privileges:true \
  -p 127.0.0.1:8080:8080 \
  -e UPSTREAM_URL=https://cloudflare-dns.com/dns-query \
  ghcr.io/krabdo/doh-relay:latest
```

或复制 `.env.example` 为 `.env`，修改上游后执行 `docker compose up -d`。镜像支持 `linux/amd64` 和 `linux/arm64`，以非 root 用户运行；运行文件系统仅含程序和 CA 证书，无 shell。

将现有反代接到 `http://127.0.0.1:8080`，示例见 [Nginx](examples/nginx.conf) 和 [Caddy](examples/Caddyfile)。替换域名及证书路径后，对外使用 `https://你的域名/dns-query`。外部端口可以在现有反代中更改。

如果反代也运行在容器内，将两个容器加入同一 Docker 网络并使用 `doh-relay:8080`；容器内的 `127.0.0.1` 指向容器自身。

## 配置

| 环境变量 | 默认值 | 说明 |
| --- | --- | --- |
| `UPSTREAM_URL` | 必填 | 完整 HTTPS DoH 地址，可含自定义端口、路径及固定查询参数；不接受 URL 用户信息或片段 |
| `LISTEN_ADDR` | `:8080` | 内部 HTTP 监听地址及端口，例如 `0.0.0.0:8053` |
| `DOH_PATH` | `/dns-query` | 下游入口路径，精确匹配 |
| `REQUEST_TIMEOUT` | `10s` | 单次请求总超时，必须为正 Go duration |

修改内部端口时，同步调整 Docker 端口映射和反代目标。上游 TLS 使用系统 CA 校验；不提供跳过校验选项，不读取 HTTP_PROXY / HTTPS_PROXY。配置变更后重启容器。

## 透传行为

- 接受 GET 和 POST；其他方法返回 405，其他路径返回 404。
- DNS 请求体和响应体逐字节转发，不增加、删除或改写 ECS。
- 入口路径替换为上游配置路径。上游原始查询参数在前，下游原始查询字符串在后，以 `&` 连接，保留重复参数、顺序及编码形式。
- 保留端到端 HTTP 头部，包括客户端已提供的 Forwarded / X-Forwarded-*；不自行添加客户端地址。Host 和 TLS SNI 指向上游，连接级头部按 HTTP 协议处理。
- 不自动跟随上游重定向，不自动压缩或解压；上游状态码（包括错误）原样返回。
- 连接失败返回通用 502，响应头发出前超时返回 504；已经开始传输响应时发生错误或超时，会终止响应，无法再替换状态码。
- 客户端取消会取消上游请求。连接和响应复制缓冲区复用，不在应用层重试请求。

透传指 DNS 数据及 HTTP 端到端语义。HTTP/1.1 与 HTTP/2 的编码、头部大小写、帧和连接字段不保证逐字节一致。服务按流转发，不把整个请求载入内存。

## 日志

程序不记录成功或失败请求的访问日志、查询内容、客户端 IP 或上游错误详情。只记录启动、退出及不含配置值的配置错误；未启用指标端点或追踪。

Nginx 示例关闭访问日志并丢弃该虚拟主机的错误日志，Caddy 示例不启用访问日志并丢弃运行日志。合并到现有配置时需检查继承的日志配置。Caddy 示例还移除它默认生成的客户端转发头，因此该入口不会保留客户端自带的这些转发头；relay 本身仍支持透传已有头部。不要在外层额外添加客户端 IP 或 DNS 查询日志。

## 开发与验证

需要 Go 1.26 或更新版本：

```sh
go test -race ./...
go vet ./...
go test -run '^$' -bench . -benchmem ./...
CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o relay .
```

测试覆盖 HTTP/1.1、HTTP/2、GET/POST、ECS 报文、重复参数、原始编码、头部过滤、错误状态、重定向、取消和超时。基准使用内存模拟上游，包含测试请求/响应对象分配，只用于衡量转发开销，不代表真实公网吞吐。

## 镜像发布

GitHub Actions 先运行竞态测试、静态检查及基准，再构建双架构镜像。PR 仅验证；`main` 发布 `latest` 和 `sha-<完整提交>`，`v1.2.3` 格式标签发布 `1.2.3` 和提交标签。

镜像地址：`ghcr.io/krabdo/doh-relay`。使用工作流自带的 `GITHUB_TOKEN`，无需单独保存 registry 密钥。首次发布的包可能默认私有；若需要匿名拉取，在 GitHub 包设置中将其可见性改为 Public。私有包需要先 `docker login ghcr.io`。

发布后工作流按摘要拉取镜像，检查架构清单、记录未压缩大小，并执行真实 DoH GET/POST 冒烟测试和日志检查。构建及基准结果见项目 Actions，镜像摘要记录在运行摘要中。
