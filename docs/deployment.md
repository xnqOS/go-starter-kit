# 单机部署

同一镜像包含 `/app/api` 与 `/app/migrate`。迁移是显式部署步骤：成功后才启动或替换 API。默认使用持久化 SQLite，也可以连接外部 PostgreSQL。

## Docker Compose

在部署机器上准备项目源码，安装 Docker Engine 和 Docker Compose v2，然后在项目根目录执行：

```sh
cp .env.deploy.example .env.deploy
# 修改 LOG_SERVICE；按需配置数据库和日志输出
make deploy
curl --fail http://127.0.0.1:8080/ready
```

也可以用 `make deploy-config` 代替手动复制和修改，见下文「部署配置页」。

`.env.deploy` 供容器读取，不会被打进镜像或提交到 Git。它与 `make run` 使用的 `.env` 分开。容器内 HTTP 端口固定为 `8080`，以匹配端口映射和就绪检查。

`make deploy` 等价于按顺序执行：

```sh
docker compose -f compose.deploy.yaml build api
docker compose -f compose.deploy.yaml run --rm --no-deps migrate
docker compose -f compose.deploy.yaml up -d --no-deps --wait api
```

迁移失败时不会执行后面的 API 启动步骤；正在运行的旧 API 会继续运行。API 首次启动失败时，查看 `make deploy-logs`。就绪检查验证进程和数据库连接，不验证业务表结构，所以应始终先运行迁移。

镜像名默认 `starter-kit:local`，主机端口默认 `8080`，可以在命令行覆盖：

```sh
make deploy IMAGE_NAME=my-api:2026-10-21 API_PORT=8081
make deploy-logs
make deploy-down
```

非默认端口在以后每次启动或更新时继续传入。默认 Compose 项目名来自项目目录；保持目录和项目名一致才能复用已有数据卷。需要自定义时，每次使用相同的 `COMPOSE_PROJECT_NAME`。

API 只绑定主机 `127.0.0.1`。对外服务时，在主机上用 Caddy、Nginx 等反向代理提供域名与 HTTPS，再转发到 `127.0.0.1:8080`。默认不信任代理转发的客户端 IP；需要真实客户端 IP（限流、日志）时，在 `.env.deploy` 中用 `TRUSTED_PROXY_CIDRS` 列出实际代理的 IP 或 CIDR（逗号分隔），只有来自这些地址的 `X-Forwarded-For` 会被采信。业务路由的认证和访问范围按产品需求配置。

## 部署配置页

`make deploy-config` 在本机 `127.0.0.1:8099` 打开一个网页，用来编写 `.env.deploy`：

- 逐项说明每个设置的作用、默认值和取值，列出哪些必填、哪些建议填；
- 顶部的「API 启动检查」直接调用 `config.Load`，结果和容器启动时一致；
- 文件还不存在时，从 `.env.deploy.example` 和线上建议值开始；
- 保存前先把旧文件备份成 `.env.deploy.bak-时间`（备份里也有密钥，检查完删掉）；现有文件里有 Compose 会做变量替换的 `$`、没闭合的引号等无法准确读取的行时，拒绝保存。

地址里的 `?t=…` 是一次性口令，页面只能监听回环地址。在服务器上用时先建 SSH 隧道：`ssh -L 8099:127.0.0.1:8099 <server>`，再在本机打开终端里打印的地址。

设置列表在 `internal/deploycatalog`。代码新读取一个环境变量时，要在那里加一项（或者写进 `NotDeployed` 并说明原因），同时加进 `.env.deploy.example`，否则 `make test` 会失败。页面和 env 文件读写来自 `kit/deployconfig`。

## SQLite 数据与备份

API 和迁移程序共用 `app_data:/app/data`，数据库路径为 `/app/data/app.db`。镜像中该目录属于 UID/GID `10001:10001`，新命名卷会继承目录权限。若改用主机目录挂载，先给这个用户配置写入权限。

```sh
make deploy-down
```

停止 API 会保留卷，重新部署后数据仍在。`docker compose down` 默认也保留命名卷；`down -v` 会删除卷和数据，仅在明确清理测试环境时使用。

SQLite 启用了 WAL，运行中只复制 `app.db` 可能遗漏数据。可使用 SQLite backup API；这个最小方案也支持先停止 API，再归档整个数据目录：

```sh
mkdir -p backups
make deploy-down
docker compose -f compose.deploy.yaml run --rm --no-deps -T migrate \
  tar -C /app/data -czf - . > backups/sqlite-data.tar.gz
docker compose -f compose.deploy.yaml up -d --no-deps --wait api
```

备份文件保存在主机 `backups/`。恢复时先停止 API，再将归档恢复到同一个卷，并确认目录归属 `10001:10001`；在数据库副本上验证恢复流程。备份操作期间不要同时执行其他部署或写入命令。

SQLite 方案适合单实例。多实例服务使用 PostgreSQL。

## 外部 PostgreSQL

先在数据库服务器创建应用数据库和用户，再修改 `.env.deploy` 的两个设置：

```dotenv
STORAGE=postgres
DATABASE_URL=postgres://app:password@db.example.com:5432/app?sslmode=verify-full
```

容器中的 `localhost` 指容器自身；应填写容器可以访问的数据库 DNS 名称或地址。本地开发的 `compose.yaml` 不会被部署命令自动启动。密码包含 URL 特殊字符时先进行百分号编码。TLS 的 CA 和连接参数按数据库服务商要求配置；若使用私有 CA，将证书以只读挂载方式提供给容器，并在连接参数中指定路径。

再次执行 `make deploy`，迁移与 API 会连接同一个 PostgreSQL。SQLite 卷可以保留；修改连接配置不会复制其中的数据。数据搬迁参见 [数据库与迁移](database.md)。数据库备份使用 PostgreSQL 自身的备份工具或托管服务能力。

## 构建镜像与直接部署二进制

```sh
make image IMAGE_NAME=my-api:local
make build
```

Docker 使用两阶段构建，构建阶段默认 Go 1.25，运行阶段为匹配的 Debian Bookworm。构建时 `CGO_ENABLED=1`，运行镜像保留所需 C 运行库和 CA 证书；SQLite 使用的驱动需要 CGO。升级 Go 要求时同步调整 `go.mod` 和 Dockerfile 的 `GO_VERSION`，也可以通过 `docker build --build-arg GO_VERSION=...` 指定版本。

`make build` 输出 `bin/api` 与 `bin/migrate`。直接部署时在目标操作系统和架构上构建，通过进程管理器注入环境变量，先运行迁移再启动 API。例如在 Linux 部署主机上：

```sh
go mod download
make build
export HTTP_ADDR=127.0.0.1:8080 GIN_MODE=release
export STORAGE=sqlite DATABASE_URL=/srv/my-api/data/app.db
export LOG_SERVICE=my-api LOG_ENV=production LOG_OUTPUTS=stdout
./bin/migrate
# 上一步成功后执行；用 systemd 等进程管理器维持运行
./bin/api
```

运行用户需能写入数据库父目录。Go 程序不会自行加载 `.env` 或 `.env.deploy`。API 收到 SIGTERM 后最多等待 10 秒完成 HTTP 请求，Compose 的停止宽限期为 15 秒。

## 更新

更新源码或 `.env.deploy` 后执行 `make deploy`。此方案不提供自动回退或零停机发布；先备份数据库，并保证新迁移兼容尚在运行的旧 API。需要破坏性表结构变更时，安排维护窗口，先停止 API。

回退应用需重新部署旧镜像；数据库变更通过新增修复迁移或恢复备份处理。不要修改已经执行的历史迁移。

参考：[Docker 多阶段构建](https://docs.docker.com/build/building/multi-stage/)、[Compose 服务配置](https://docs.docker.com/reference/compose-file/services/)、[SQLite 驱动构建要求](https://github.com/mattn/go-sqlite3#installation)。
