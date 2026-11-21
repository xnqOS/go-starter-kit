# 数据库与迁移

默认用 SQLite 快速开发，部署时通过配置切换 PostgreSQL。业务 Service 仍依赖 Repo 接口；SQLite 和 PostgreSQL 使用同一个 GORM SQLRepo。常规 CRUD 和迁移只维护一份，数据库特有功能才按方言处理。

## 本地 SQLite

```sh
cp .env.example .env
go mod download
make migrate
make run
```

默认配置：

```dotenv
STORAGE=sqlite
DATABASE_URL=./data/app.db
```

路径相对于命令执行目录，父目录自动创建。支持普通文件路径和 `:memory:`；不接受 PostgreSQL URL 或带参数的 SQLite URI。连接参数集中在 `kit/database/sqlite.go` 管理，避免不同连接遗漏配置。

SQLite 使用官方 `gorm.io/driver/sqlite`，底层为 `mattn/go-sqlite3`，需要 C 编译器和 `CGO_ENABLED=1`。macOS 可安装 Command Line Tools；Debian/Ubuntu 可安装 `build-essential`。常规本机 Go 安装默认启用 CGO；如果曾禁用，可执行 `make migrate CGO_ENABLED=1` 和 `make run CGO_ENABLED=1`。PostgreSQL 的 pgx 驱动本身不需要 CGO。

文件数据库启用 WAL、外键检查、5 秒 busy timeout，连接池限制为一个连接。事务采用 `BEGIN IMMEDIATE`，防止多个迁移进程同时读取旧历史后争抢写锁。配置通过 DSN 应用到每个新连接。WAL 改善读写共存，SQLite 仍适合本机开发和轻量单实例应用；多个实例共享写入时应使用 PostgreSQL。

`data/`、`*.db` 及 WAL 辅助文件已加入忽略规则。数据库有 WAL 时，运行中只复制主文件可能遗漏数据；使用 SQLite backup 能力，或先停止写入、正常关闭连接后备份。

已有 `.env` 不会被自动覆盖。若仍为旧版 `STORAGE=memory`，手动修改上面两个设置才会启用 SQLite；内存中的临时数据不会自动持久化。

## PostgreSQL 部署

同时设置存储类型和连接地址：

```dotenv
STORAGE=postgres
DATABASE_URL=postgres://app:password@db:5432/app?sslmode=require
```

先执行 `make migrate`，成功后启动或更新 API。API 启动和就绪检查不会执行迁移。没有复制 `.env` 时，直接运行 Go 命令需要在进程环境中提供这些值；Go 程序本身不加载 dotenv 文件。

本地 Compose 示例：

```sh
make db-up
make migrate STORAGE=postgres DATABASE_URL='postgres://starter:starter@localhost:5432/starter?sslmode=disable'
make run STORAGE=postgres DATABASE_URL='postgres://starter:starter@localhost:5432/starter?sslmode=disable'
```

`make` 命令行参数优先于 `.env`。只写 `STORAGE=postgres` 会继续读取原来的 SQLite 文件路径，因此两个设置应一起修改。生产连接的 TLS 参数根据部署环境配置。

## 一份 SQLRepo

`internal/example/repo_sql.go` 和生成的模块都接收 `*gorm.DB`，使用相同的方法：

```go
repo := example.NewSQLRepo(db)
item, err := repo.Get(ctx, id)
```

查询用 `Where("id = ?", id)` 绑定参数，GORM 处理数据库占位符。显式指定表名，避免复数命名猜测；更新使用 map，能够写入零值。更新结果使用两种数据库都支持的 `RETURNING`；使用的是驱动内置的 SQLite 版本。

业务 Model 不带 GORM 依赖。Repo 内有独立的行结构负责存储映射，历史迁移另存当时的表结构。新增字段时同步修改业务 Model、DTO、校验、SQL 行映射和内存仓储，但不修改已应用的迁移。

需要跨模块写入事务时，将同一个事务传给各个 Repo：

```go
err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
    // example.NewSQLRepo(tx)、other.NewSQLRepo(tx) 共用此事务。
    // 在这里完成需要一起提交的业务操作，并返回错误触发回滚。
    return nil
})
```

事务内不要调用使用外部 db 连接的 Repo。SQLite 只配置一个连接，混用外部连接还会等待连接池，直到请求超时。

## 新模块与后续字段变更

```sh
make module NAME=order
make test
make migrate
make run
```

生成器创建 `repo_sql.go`、内存实现和一个 `migrations/000002_create_order.go`，并显式注册路由和迁移。序号取当前 Go 迁移最大值加一。无需各写一套 PostgreSQL/SQLite SQL。

Go 迁移使用 GORM Migrator；首次建表直接 `CreateTable`，不对当前业务 Model 运行 `AutoMigrate`。每个迁移的结构体加版本后缀，保存在该迁移文件中，不从业务包引用结构体。

例如只有初始 example 迁移时，要增加一个可空 note 字段，新增 `migrations/000002_add_example_note.go`：

```go
package migrations

import "gorm.io/gorm"

type exampleNoteV000002 struct {
    Note *string `gorm:"type:text"`
}

func (exampleNoteV000002) TableName() string { return "m_example" }

func addExampleNoteV000002(tx *gorm.DB) error {
    return tx.Migrator().AddColumn(&exampleNoteV000002{}, "Note")
}
```

在 `migrations/migrations.go` 的 definitions 列表中，放在标记前：

```go
{"000002_add_example_note", addExampleNoteV000002},
// scaffold:migrations
```

然后更新应用字段和映射，在两种数据库上测试，执行 `make migrate`。若已有其他迁移，使用下一个未占用的六位序号。上述代码只展示表结构迁移，应用层是否暴露新字段由业务决定。

## 迁移执行规则

迁移源码随二进制嵌入，SHA-256 和执行时间记录在 `starter_go_migrations`。执行器校验完整历史，按版本排序，在一个事务中执行本批新增迁移：

- 已执行的迁移不会再执行；源码变动会拒绝继续，即使只改了注释。
- 新版本只能追加，不能删除历史迁移或向已执行版本之前插入迁移。
- PostgreSQL 使用事务级 advisory lock；SQLite 使用 immediate 事务串行处理，等待超过 busy timeout 时应重试迁移命令。
- 某个迁移失败，整个批次的数据库变更和历史记录一起回滚。回调只操作传入的 tx，不做文件、网络等无法回滚的副作用。
- 默认只有向前迁移。上线回退使用新增修复迁移或恢复备份，不提供自动删除表和数据的 down 命令。

只改 `migrations/migrations.go` 的注册列表不会改变历史源码校验；执行器仍会验证版本序列与已有记录。新增文件必须在列表中注册，每个文件仅注册一次；生成器会完成注册。

事务中无法执行的操作（例如 PostgreSQL `CREATE INDEX CONCURRENTLY`）需要单独部署步骤。大型表的数据回填建议分批完成，避免长事务；加非空字段时通常先加可空字段，再回填，最后补约束。

## 已有 PostgreSQL 数据库如何升级

原来的 `000001_create_example.up.sql` 保留在 `migrations/legacy/`，仅用于校验旧记录。它不再作为活动迁移执行。

执行新命令时，如果存在旧表 `starter_schema_migrations`，执行器检查旧迁移名称、原始 checksum 及对应业务表；匹配后给 Go 迁移建立已执行记录，保留原表、索引、数据和旧历史表，不重新建表。接着执行新的 Go 迁移。

该兼容已配置到原始 example 迁移。如果旧项目另有生成的 SQL 模块或自定义 SQL 迁移，会停止并明确列出未识别版本。需要先为这些版本建立冻结的 Go 定义，并在注册处补充 `database.LegacyMigration` 的名称、原始 checksum 和表清单，再在数据库副本上验证升级。不要通过删除历史表或业务表绕过检查。

## SQLite 的已有数据搬到 PostgreSQL

迁移命令只管理表结构。改变环境变量不会复制 SQLite 数据，也不能直接把 SQLite 的 SQL dump 当成 PostgreSQL 迁移执行。

如果要保留开发数据：

1. 在目标 PostgreSQL 执行同一份 Go 迁移，建立空表。
2. 停止源数据库写入并备份。
3. 编写一次性的 Go 导入命令或使用合适的导入工具，按外键依赖顺序读取和写入，保留主键、UTC 时间及业务值。可复用行映射和 GORM；导入脚本需按实际业务表设计，本项目未提供通用数据搬迁命令。
4. 核对每张表的数量、关键关联、时间和金额等值，并在目标库运行关键业务检查。
5. 修改应用的两个数据库配置后启动，确认通过后再处理旧文件。

## 跨库边界与验证

文本、整数、UTC 时间、可空字段及常规索引通常可以共享；JSONB、数组、全文搜索、数据库函数、排序规则、特定索引和复杂 ALTER TABLE 可能需要方言分支。GORM 不保证任意 SQL 都能跨库使用。金额优先用最小货币单位的整数，避免依赖 SQLite 和 PostgreSQL 不同的浮点/数值行为。

```sh
make check

# 独立测试数据库；测试创建并清理自己的 schema
make check TEST_DATABASE_URL='postgres://starter:starter@localhost:5432/starter_test?sslmode=disable'
```

默认跑真实的临时 SQLite 文件测试；设置 `TEST_DATABASE_URL` 后，同一组仓储、HTTP、持久化和迁移测试也会运行 PostgreSQL。CI 配置提供 PostgreSQL 服务并设置此变量，避免只测 SQLite。

参考：[GORM Migrator](https://gorm.io/docs/migration.html)、[GORM 数据库连接](https://gorm.io/docs/connecting_to_the_database.html)、[SQLite 驱动配置](https://github.com/mattn/go-sqlite3#connection-string)、[SQLite WAL](https://www.sqlite.org/wal.html)。
