# Go Starter Kit

按业务模块组织的 Go 后端：**Gin + GORM + SQLite / PostgreSQL**。默认使用 SQLite 文件快速开发，部署时切换 PostgreSQL，两个数据库共用一份仓储和版本化 Go 迁移。

要求 Go 1.25+。SQLite 使用官方 GORM 驱动，需要 CGO 和 C 编译器（macOS 安装 Command Line Tools，Debian/Ubuntu 安装 `build-essential`）。PostgreSQL 模式使用 pgx，可通过 Docker Compose 启动本地实例。

集中日志使用 **slog → Syslog → VictoriaLogs**，默认只写控制台。支持多个应用按名称区分，见 [日志与集中日志服务器](docs/logging.md)。

## 快速开始

```sh
cp .env.example .env
go mod download
make migrate
make run
```

`make` 会读取并导出 `.env`；直接执行 `go run ./cmd/api` 时读取进程环境变量。默认地址为 `:8080`，数据保存在 `./data/app.db`，重启后仍保留。数据库目录自动创建，迁移需显式执行。

```sh
curl http://localhost:8080/health

curl -X POST http://localhost:8080/api/v1/example \
  -H 'Content-Type: application/json' \
  -d '{"name":"第一个 example"}'

curl 'http://localhost:8080/api/v1/example?limit=20&offset=0'
```

业务响应统一使用 `code/message/data/request_id`，成功示例：

```json
{"code":"ok","message":"success","data":{"id":"...","name":"第一个 example","created_at":"...","updated_at":"..."},"request_id":"..."}
```

错误格式：

```json
{"code":"invalid_input","message":"name must contain 1 to 200 characters","data":null,"request_id":"..."}
```

列表的 `data` 为数组，额外返回 `pagination: {"limit":20,"offset":0,"total":42}`。`code` 是稳定的字符串业务码，HTTP 状态仍表达成功或失败；`request_id` 与响应头 `X-Request-ID`、请求日志一致。删除成功返回无响应体的 204。

模板取舍、完整状态码、业务错误扩展和前端使用方式见 [HTTP 响应约定](docs/http-response.md)。

## 目录与依赖

```text
cmd/
  api/                     API 入口与信号处理
  migrate/                 数据库迁移命令
  modulegen/               模块生成器命令
  projectinit/             重命名项目或导出干净副本
  deploy-config/           部署前填写 .env.deploy 的本地网页
internal/
  app/                     依赖组装、路由、中间件、优雅退出
    modules.go             所有业务模块的显式注册
  config/                  环境变量配置
  deploycatalog/           本项目的部署配置项目录（供 deploy-config 使用）
  example/                 可运行的 CRUD 示例
    routes.go              路由声明
    handler.go             HTTP 参数与响应
    dto.go                 请求结构与查询参数
    service.go             业务规则；只依赖 Repo 接口
    repo.go                模块需要的持久化能力
    repo_memory.go         线程安全的内存实现
    repo_sql.go            共用的 GORM 实现（SQLite / PostgreSQL）
    model.go               业务数据结构
    *_test.go              业务、HTTP 和并发测试
  scaffold/templates/      可定制的模块生成模板
  projectinit/             初始化与导出；仅改 module 和 Go import
kit/                       独立的公共 module（github.com/xnqOS/go-starter-kit/kit），新项目作为依赖引用
  apperror/                与 HTTP 无关的公共错误
  httpx/                   JSON 请求、统一响应信封、分页和 HTTP 错误映射
  logging/                 slog、Syslog TCP/TLS 输出与请求日志上下文
  database/                SQLite / PostgreSQL 连接、Go 迁移、读事务、约束识别与 savepoint
  credential/              argon2id 密码哈希、token 哈希、邮箱规范化
  admission/               昂贵操作的并发上限与有界排队
  ratelimit/               固定内存的按 IP 限流中间件
  secretbox/               AES-GCM 加密存储密钥
  humancheck/              Cloudflare Turnstile 人机校验
  email/                   邮件 provider 接口与 SMTP / Resend / SendGrid 适配器
    outbox/                事务内排队、重试、加密存储的邮件 outbox
  storage/                 文件存储接口：本地目录与 Worker（R2）
  deployconfig/            .env.deploy 解析、渲染与配置页
migrations/                Go 迁移与显式注册；源码嵌入用于校验
  legacy/                  原 SQL 迁移归档，仅用于兼容旧数据库
scripts/add-module.sh      快速添加模块
deploy/log-server/         独立日志服务器的 TLS 部署配置
Dockerfile                 同时构建 API 与迁移程序，支持 SQLite CGO
compose.deploy.yaml        单机部署；迁移和 API 共用镜像与数据卷
```

`kit/` 是单独的 Go module，模板通过 `go.mod` 里的 `replace` 使用本地副本，`make init` 出来的新项目改为依赖已发布的版本。发布和升级见 [公共 kit module](docs/kit-module.md)。`credential`、`admission`、`ratelimit`、`secretbox`、`humancheck`、`email`、`storage` 默认不接入路由，用法见 [公共基础包](docs/shared-packages.md)、[邮件](docs/email.md) 和 [文件存储](docs/storage.md)。

请求调用链为 `Handler → Service → Repo`。`internal/app/modules.go` 创建仓储、Service 和 Handler，业务构造函数不创建数据库或其他模块的仓储。

## 一条命令添加业务模块

```sh
make module NAME=order
```

会生成 `internal/order/`、一份冻结表结构的 Go 迁移，并修改 `internal/app/modules.go` 和 `migrations/migrations.go`，自动注册 CRUD 路由和迁移。SQLite、PostgreSQL 共用 `NewSQLRepo(db)`；内存模式使用 `NewMemoryRepo()`。

也可以直接使用脚本、自定义路由，或预览变更：

```sh
./scripts/add-module.sh coffee_fund -path /coffee-funds
./scripts/add-module.sh order -dry-run
make module NAME=product ROUTE=/products
```

模块名使用小写字母、数字和下划线，如 `order`、`coffee_fund`；默认路由把下划线转为连字符，不自动猜测单复数。路由参数是 `/api/v1` 之后的单个路径段。

生成内容是带 `name` 字段的完整 CRUD 骨架。修改字段时更新 DTO、Model、Service、SQL Repo 中的存储映射和内存 Repo。首次迁移前可调整生成的表结构；已经应用的迁移保持原样，后续变更另加递增编号的 `.go` 迁移。

生成器会读取当前 `go.mod` 的 module 路径，检查非法名称、目录和路由冲突，并拒绝覆盖已有文件。写入失败会清理新建文件、还原已修改的注册文件；生成操作使用锁文件串行执行。请保留 `modules.go` 中的两个 `scaffold:` 标记和 `migrations.go` 中的 `scaffold:migrations` 标记。

新增模块后运行 `make test`、`make migrate`，再重启 API。内存模式无需迁移；生成器不会替运行中的进程自动热加载路由。

## Example API

| 方法 | 路径 | 行为 |
| --- | --- | --- |
| POST | `/api/v1/example` | 创建；JSON 为 `{"name":"..."}`，返回 201 |
| GET | `/api/v1/example` | 列表；支持 `limit`、`offset` |
| GET | `/api/v1/example/:id` | 详情 |
| PUT | `/api/v1/example/:id` | 更新 name |
| DELETE | `/api/v1/example/:id` | 删除，返回 204 |
| GET | `/health` | 进程存活检查 |
| GET | `/ready` | 就绪检查；SQLite / PostgreSQL 模式会检测连接 |

`name` 去除两端空白后需为 1–200 个字符。列表默认 `limit=20`，最大 100；`offset` 从 0 开始，响应中带有效分页参数和真实总数 `total`。空列表的 `data` 为 `[]`，翻到末页之外仍保留总数。缺失资源返回 404，数据库等内部错误返回统一的 500，不向客户端暴露错误细节。

## 切换 PostgreSQL

```sh
make db-up
make migrate STORAGE=postgres DATABASE_URL='postgres://starter:starter@localhost:5432/starter?sslmode=disable'
make run STORAGE=postgres DATABASE_URL='postgres://starter:starter@localhost:5432/starter?sslmode=disable'
```

`make` 命令行参数优先于 `.env`。也可以在 `.env` 中同时修改 `STORAGE=postgres` 和 `DATABASE_URL` 后执行迁移与启动命令。本地 Compose 使用 `starter` 数据库、用户及密码，只监听本机 5432 端口。

日常 CRUD 和表结构变更通常只写一份：GORM 处理查询参数与数据库方言，Go 迁移使用 Migrator 执行建表、加列等操作。迁移保存独立的历史结构，不引用当前业务 Model，也不在 API 启动时运行 `AutoMigrate`。

执行器在一个事务内应用待执行迁移，串行处理并发执行，校验已应用源码的 SHA-256，拒绝未知版本和向历史中间插入新迁移。默认只向前迁移；回退通过备份恢复或新增修复迁移处理。事务外操作（如 `CREATE INDEX CONCURRENTLY`）需要独立部署步骤。`make db-down` 停止数据库并保留数据卷。

**切换配置只会连接另一个数据库，不会把 SQLite 中的数据搬到 PostgreSQL。** 数据导入、旧 PostgreSQL 兼容、增加字段示例及跨库限制见 [数据库与迁移](docs/database.md)。临时演示可用 `make run STORAGE=memory`，数据在退出后清空。

## 多模块组合查询

同库关联查询可以直接在所属模块的 Repo 中写 JOIN，返回查询专用 DTO。仪表盘、报表可以单独建 `internal/dashboard/query.go`，没有业务编排时无需额外 Service。读取其他模块的表会产生表结构依赖，需一起维护相关查询和数据范围过滤。

涉及多个模块的写入和业务规则时，提取明确的 Service / UseCase；需要原子提交的操作应通过 GORM Transaction 给相关 Repo 传入同一个事务。当前 CRUD 示例暂不引入通用 Unit of Work 抽象。

## 验证

```sh
make test
make check
make build
```

普通测试自动使用临时 SQLite 文件，不需要外部数据库或日志服务器。PostgreSQL 集成测试需显式提供独立的测试数据库，测试会在其中创建并清理自己的 schema：

```sh
# 先创建 starter_test 数据库，再执行：
TEST_DATABASE_URL='postgres://starter:starter@localhost:5432/starter_test?sslmode=disable' make test
```

SQLite 和 PostgreSQL 运行同一套仓储、HTTP、持久化与迁移测试，覆盖分页、更新结果、约束、重复执行、内容校验、失败回滚、并发迁移和旧版本数据保留。未设置 `TEST_DATABASE_URL` 时跳过 PostgreSQL；GitHub Actions 提供 PostgreSQL 服务并执行两种数据库测试，同时生成一个模块验证模板。

## 集中日志

`slog` 是 Go 写日志的标准接口，Syslog 是应用与日志服务器之间的协议。当前业务调用方式保持为 `logger.InfoContext(ctx, "message", "key", value)`。

启动本地 VictoriaLogs 并启用发送：

```sh
make logs-up
make run LOG_OUTPUTS=stdout,syslog LOG_SERVICE=order-api
```

打开 <http://localhost:9428/select/vmui/>，输入 `_time:15m app_name:="order-api"` 查询。不同应用设置不同的 `LOG_SERVICE`；服务器会从标准 `APP-NAME` 字段区分它们。实例默认用主机名区分，也可以设置 `LOG_INSTANCE`。

`make logs-test` 会发送两个应用的日志并验证实际查询；`make logs-down` 停止服务器并保留数据。服务器默认保留 14 天日志。本地演示监听本机地址，独立服务器部署使用 [TLS Compose 配置](deploy/log-server/compose.yaml)。

请求日志自动携带 `request_id`；请求处理代码可通过 `logging.FromContext(ctx)` 继续记录同一请求的日志。发送失败会报告到 stderr，其他输出继续工作；当前没有断网持久化补发队列。

完整配置、slog/Syslog 的区别、多应用查询及 TLS 部署步骤见 [日志文档](docs/logging.md)。

## 用作新项目

通过 GitHub 模板创建仓库，或下载、克隆项目后，在自己的副本里执行：

```sh
make init MODULE=github.com/your-name/my-api
cp .env.example .env
go mod tidy
make check
make migrate
make run
```

初始化会读取当前 `go.mod`，更新 module 声明和源码中的对应 import，并删掉指向本地 `./kit` 的 `replace`，改为依赖已发布的 kit 版本（见 [公共 kit module](docs/kit-module.md)），支持重复执行和再次重命名。只改 import，不替换注释、业务字符串或配置；未改动的历史迁移保留原始字节。生成器会自动使用新 module 路径。按需修改 `.env` 中的 `LOG_SERVICE`。

从当前工作目录另建项目，可导出干净副本：

```sh
make init MODULE=github.com/your-name/my-api DEST=../my-api
cd ../my-api
cp .env.example .env
go mod tidy
make check
```

`DEST` 的父目录必须存在，目标目录必须尚不存在且位于源项目之外。导出只包含列明的项目源码、配置样例、文档和许可文件，排除 `kit/`、Git 历史、本地环境文件、数据库、构建产物与证书私钥。自定义顶层文件需自行复制；源码目录中的符号链接会被拒绝。原项目保持不变。原地初始化遇到写入错误会尝试还原已修改文件；初始化不执行迁移。

当前提供业务骨架和存储适配；登录、用户隔离与权限策略应按具体产品需求加入路由中间件及查询条件。

## 部署

需要 Docker Engine 和 Docker Compose v2。首次部署：

```sh
cp .env.deploy.example .env.deploy
# 按项目修改 .env.deploy 中的 LOG_SERVICE 和数据库设置，或用 make deploy-config 在本地网页里填写和检查
make deploy
curl http://localhost:8080/ready
```

`make deploy` 依次构建镜像、运行一次性迁移、等待 API 就绪；任何步骤失败都会停止后续步骤。默认 SQLite 数据位于命名卷 `app_data`，API 只监听服务器的 `127.0.0.1:8080`。部署环境文件与本地开发的 `.env` 分开；切换 PostgreSQL 时同时修改 `.env.deploy` 的 `STORAGE` 和 `DATABASE_URL`。

```sh
make deploy-logs      # 查看 API 日志
make deploy-down      # 停止 API，保留数据卷
make deploy           # 应用代码或部署配置更新
```

镜像使用非 root 用户，包含 `/app/api` 和 `/app/migrate`，构建时启用 CGO。`make build` 也会输出 `bin/api` 与 `bin/migrate`，便于直接部署二进制。镜像构建、反向代理、PostgreSQL、备份与更新步骤见 [部署文档](docs/deployment.md)。

## 发布到 GitHub

GitHub 不是运行此项目的前提；供别人下载和复用时，推荐创建公开仓库，上传后在 **Settings → General → Template repository** 勾选模板仓库。别人就能使用 **Use this template** 创建自己的仓库，再运行上面的初始化命令。[GitHub 模板仓库说明](https://docs.github.com/en/repositories/creating-and-managing-repositories/creating-a-template-repository)

在 GitHub 建好一个空仓库后，在项目根目录执行（把 URL 换成自己的）：

```sh
git init -b main
git add .
git status --short
git commit -m "Initial starter kit"
git remote add origin https://github.com/YOUR_NAME/go-starter-kit.git
git push -u origin main
```

提交前核对暂存文件。本地环境文件、数据和证书目录已有忽略规则。公开复用前请补充你选择的 `LICENSE`；初始化导出会保留常见名称的许可文件。

参考：[GORM 迁移](https://gorm.io/docs/migration.html)、[GORM 连接数据库](https://gorm.io/docs/connecting_to_the_database.html)、[Gin 测试文档](https://gin-gonic.com/en/docs/testing/)、[Go 服务项目目录建议](https://go.dev/doc/modules/layout#server-project)。
