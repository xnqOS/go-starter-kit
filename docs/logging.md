# slog、Syslog 与集中日志服务器

本项目使用 **Go slog → Syslog → VictoriaLogs**。默认只写本地控制台；需要集中日志时，通过配置增加 Syslog 输出。

## slog 和 Syslog 有什么区别

| 名称 | 作用 | 本项目中的位置 |
| --- | --- | --- |
| `log/slog` | Go 标准库的日志 API，提供级别、消息和键值字段 | 业务代码调用的接口 |
| Syslog | 独立于语言的日志消息格式和传输协议 | 应用发送到服务器的协议 |
| VictoriaLogs | 接收、保存和查询日志的服务 | 可以自行部署的日志服务器 |

它们可以一起使用：

```text
应用调用 slog.InfoContext(...)
        ├── JSON → 本地 stdout
        └── RFC 5424 Syslog → TCP / TLS → VictoriaLogs
```

`slog` 本身不会自动发送远程日志，需要配置 Handler。本项目在 `kit/logging` 中提供适配，无需业务代码拼接报文，也没有引入新的日志 SDK。

## 多个应用如何区分

每个应用配置自己的 `LOG_SERVICE`，同一应用的不同实例使用不同的 `LOG_INSTANCE`。

| 应用配置 / 信息 | Syslog 标准字段 | VictoriaLogs 中的字段 | 用途 |
| --- | --- | --- | --- |
| `LOG_SERVICE` | `APP-NAME` | `app_name` | 区分应用，例如 order-api、billing-api |
| `LOG_INSTANCE`，默认主机名 | `HOSTNAME` | `hostname` | 区分主机或容器实例 |
| 进程 PID | `PROCID` | `proc_id` | 区分同一主机上的进程 |
| 日志时间、级别 | `TIMESTAMP`、`PRI` | 服务端解析对应元信息 | 定位时间和严重程度 |
| `LOG_ENV`、模块、请求 ID 等 | JSON 消息正文 | 使用 `unpack_json` 展开 | 进一步筛选业务信息 |

JSON 正文也会带上 `service`、`env`、`instance`，方便在本地和服务器使用相同字段。`APP-NAME` 不依赖正文解析，因此服务器接收后就能按应用筛选。

例如：

```text
order-api   / order-worker-1   / production
order-api   / order-worker-2   / production
billing-api / billing-worker-1 / production
```

同一个服务器可以接收全部应用。应用名和实例字段用于标识、筛选，不构成访问权限或租户隔离。

`LOG_SERVICE` 需为 1–48 个非空格可打印 ASCII 字符，`LOG_INSTANCE` 上限为 255；不能使用单独的 `-`。推荐使用字母、数字、短横线和下划线，例如 `order-api`。

## 本地启动日志服务器

要求 Docker Compose；API 仍在宿主机运行。

```sh
make logs-up
make migrate
make run LOG_OUTPUTS=stdout,syslog LOG_SERVICE=order-api
```

日志服务器使用独立数据卷，默认保留 14 天。Syslog 接收端口为 `127.0.0.1:5140`，网页地址为：

<http://localhost:9428/select/vmui/>

访问 API 或创建 example，就可以看到请求和业务日志：

```sh
curl -X POST http://localhost:8080/api/v1/example \
  -H 'Content-Type: application/json' \
  -d '{"name":"日志示例"}'
```

在另一个终端启动第二个应用实例：

```sh
make run LOG_OUTPUTS=stdout,syslog LOG_SERVICE=billing-api HTTP_ADDR=:8081
curl http://localhost:8081/health
```

如果 API 也运行在容器中，`127.0.0.1` 指向 API 容器自身；同一 Compose 网络下应设置 `LOG_SYSLOG_ADDRESS=victorialogs:5140`。

常用命令：

```sh
make logs-tail    # 查看日志服务器自身的运行日志
make logs-test    # 发送两个不同应用的测试日志，并验证服务端查询
make logs-down    # 停止日志服务器，保留数据卷
```

## 在服务器中查询

在网页查询框中，查看最近 15 分钟的指定应用：

```text
_time:15m app_name:="order-api"
```

查看指定实例：

```text
_time:15m app_name:="order-api" hostname:="order-worker-1"
```

Syslog 消息正文是 JSON，初始保存在 `_msg` 中。展开后可以按日志级别、环境或请求 ID 筛选：

```text
_time:15m app_name:="order-api" | unpack_json | filter level:="ERROR"
```

```text
_time:15m app_name:="order-api" | unpack_json | filter env:="production"
```

```text
_time:15m app_name:="order-api" | unpack_json | filter request_id:="你的请求ID"
```

也可以通过 HTTP API 查询：

```sh
curl -sS http://localhost:9428/select/logsql/query \
  --data-urlencode 'query=_time:15m app_name:="order-api"'
```

## 应用如何写日志

```go
logger.InfoContext(ctx, "order created", "order_id", order.ID)
logger.ErrorContext(ctx, "payment failed", "order_id", order.ID, "error", err)
```

在请求处理代码中，使用带请求 ID 的 Logger：

```go
logger := logging.FromContext(ctx)
logger.InfoContext(ctx, "order created", "module", "order", "order_id", order.ID)
```

HTTP 中间件生成 `request_id`，通过响应头 `X-Request-ID` 和业务 JSON 的 `request_id` 返回，并把请求 Logger 放入标准 `Request.Context()`。将该 context 传到下一层即可继续取到同一 Logger；`httpx.Fail` 也使用它。204 响应通过响应头提供请求 ID。

需要固定模块字段时可以使用 `logger.With("module", "order")`。业务 Service 也可以通过构造函数接收 `*slog.Logger`。`InfoContext` 会传递 context，但不会自动寻找里面的请求字段，因此请求代码应使用 `logging.FromContext(ctx)` 获得已绑定字段的 Logger。

Example 和生成器生成的模块在成功创建记录时会输出一条带 `module`、`id` 的业务日志。HTTP 访问日志按状态选择级别：2xx/3xx 为 INFO、4xx 为 WARN、5xx 为 ERROR。

## 配置参考

`make` 会读取 `.env`，命令行参数优先，例如 `make run LOG_SERVICE=order-api`。直接运行二进制或 `go run` 时读取进程环境变量。

| 环境变量 | 默认值 | 说明 |
| --- | --- | --- |
| `LOG_OUTPUTS` | `stdout` | `stdout`、`syslog` 或 `stdout,syslog` |
| `LOG_LEVEL` | `info` | 最低级别，常用 debug / info / warn / error |
| `LOG_SERVICE` | `starter-kit` | 应用名，写入 APP-NAME |
| `LOG_ENV` | `development` | 环境，写入 JSON 正文 |
| `LOG_INSTANCE` | 主机名 | 实例名，写入 HOSTNAME |
| `LOG_SYSLOG_ADDRESS` | `127.0.0.1:5140` | 接收端 host:port |
| `LOG_SYSLOG_NETWORK` | `tcp` | `tcp` 或 `tls` |
| `LOG_SYSLOG_FACILITY` | `local0` | user / daemon / local0–local7 |
| `LOG_SYSLOG_TIMEOUT` | `500ms` | 包含排队、建连、握手及发送的时间预算，上限 30s |
| `LOG_SYSLOG_TLS_CA_FILE` | 空 | 可选的私有 CA PEM 文件；默认使用系统信任库 |
| `LOG_SYSLOG_TLS_SERVER_NAME` | 地址中的主机名 | 可选，覆盖证书校验所用的服务端名称 |

常用日志级别映射：DEBUG → Syslog DEBUG，INFO → INFO，WARN → WARNING，ERROR → ERR。JSON 正文保留 slog 的 `level` 字段。

## 部署到独立的日志服务器

本地演示的两个端口只绑定回环地址。跨机器部署可使用 [独立 TLS 部署配置](../deploy/log-server/compose.yaml)：

1. 在日志服务器的 `deploy/log-server/certs/` 放入 `server.crt`、`server.key`。证书的 SAN 应包含客户端连接的域名，例如 `logs.example.com`。
2. 指定日志服务器用于接收应用连接的内网 IP：

```sh
LOG_SERVER_BIND_IP=10.0.0.5 docker compose -f deploy/log-server/compose.yaml up -d --wait
```

3. 在各应用中配置：

```dotenv
LOG_OUTPUTS=stdout,syslog
LOG_SERVICE=order-api
LOG_ENV=production
LOG_SYSLOG_NETWORK=tls
LOG_SYSLOG_ADDRESS=logs.example.com:6514
# 私有 CA 才需要提供：
# LOG_SYSLOG_TLS_CA_FILE=/etc/myapp/log-ca.pem
```

服务端开启 Syslog TLS，客户端验证证书链和主机名。查询网页仍绑定服务器本机，可通过 SSH 转发访问：

```sh
ssh -L 9428:127.0.0.1:9428 user@logs.example.com
```

独立部署配置只有日志服务器，不包含 PostgreSQL。它使用自己的 Compose 项目与数据卷，与根目录的本地演示分开管理。应用名称用于日志筛选，查询接口的访问控制按实际部署环境设置。

## 协议与故障行为

应用输出标准 RFC 5424 报文，TCP 使用 RFC 6587 的 octet-counting 分帧，TLS 使用 RFC 5425 相同的长度分帧。正文使用单条 JSON，中文、换行以及多条连续日志都按字节长度区分。此实现不使用 Go `log/syslog` 的旧式报文格式。

初次写远程日志时才建立连接，复用连接发送；发送失败后关闭连接，下一条日志尝试重连。配置或证书文件无效会阻止启动；服务器暂时不可用会报告到独立 stderr，并继续尝试其他日志输出。建议保留 `stdout,syslog`，以便远程故障期间仍有本地副本。

远程写入采用同步、有超时的发送方式；没有持久化队列，不会自动补发断网期间的日志，也不会在部分写入后自动重试同一条记录。TCP 写入成功也不代表服务端已持久化。需要断网补发时，可后续接入 rsyslog / syslog-ng 等带磁盘队列的采集器。

退出时，API 先停止处理请求，再关闭数据库及日志连接。普通测试通过内存连接验证报文、并发、超时、重连及 TLS 证书校验，无需启动日志服务器；`make logs-test` 用于额外验证实际 VictoriaLogs 接收和多应用查询。

参考：[Go slog](https://pkg.go.dev/log/slog)、[RFC 5424](https://www.rfc-editor.org/rfc/rfc5424)、[RFC 6587](https://www.rfc-editor.org/rfc/rfc6587)、[RFC 5425](https://www.rfc-editor.org/rfc/rfc5425)、[VictoriaLogs Syslog 接入](https://docs.victoriametrics.com/victorialogs/data-ingestion/syslog/)、[LogsQL](https://docs.victoriametrics.com/victorialogs/logsql/)。
