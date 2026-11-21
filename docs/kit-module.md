# 公共 kit module

和业务无关、改动少的代码放在 `kit/`。它是一个独立的 Go module，路径为 `github.com/xnqOS/go-starter-kit/kit`。新项目把它当依赖使用，不再复制源码。kit 里修了 bug，各项目升级依赖就能拿到。

## 哪些在 kit，哪些留在模板

| 位置 | 内容 | 新项目怎么拿到 |
|---|---|---|
| `kit/`（依赖库） | `apperror`、`httpx`、`logging`、`database`、`credential`、`admission`、`ratelimit`、`secretbox`、`humancheck`、`email/...`、`storage`、`deployconfig` | `go.mod` 里的 `require`，升级用 `go get` |
| 模板（复制） | `cmd/`、`internal/app`、`internal/config`、`internal/example`、`internal/scaffold`、`internal/projectinit`、`internal/deploycatalog`、`migrations/`、部署文件 | `make init` 导出后归项目自己所有 |

判断标准：项目大概率要改的（路由组装、配置项、示例模块、表结构）留在模板；所有项目都应该一样、出了 bug 要一起修的放进 kit。

## 在模板里开发

模板的 `go.mod` 用 `replace github.com/xnqOS/go-starter-kit/kit => ./kit` 指向本地目录，所以改 kit 和改模板可以放在同一个 PR 里，`make check` 会同时测两个 module。Dockerfile 用 `COPY ki[t] ./kit`：模板有 `kit/` 就复制，导出的项目没有这个目录，这一行什么也不做。

## 发布新版本

kit 在子目录里，所以 tag 要带目录前缀：

```sh
# PR 合并到 main 之后
git tag kit/v0.1.0
git push origin kit/v0.1.0
```

发布后，把模板 `go.mod` 里 `require github.com/xnqOS/go-starter-kit/kit` 的版本改成刚打的 tag。`make init` 导出的项目会用这个版本，所以它必须是已经推送的 tag。

目前是 v0：不承诺兼容，但不兼容的改动要写进 tag 说明。以下内容发布后**不能改**，要改只能新增：

- `outbox.CreateTableV1` 等带版本号的建表函数（项目的迁移文件会调用它们，迁移必须可重放）
- `database.Migrate` 使用的迁移记录表 `starter_go_migrations`

## 新项目怎么用

`make init` 会删掉指向 `./kit` 的 `replace`，导出时也不会带上 `kit/` 目录：

```sh
make init MODULE=github.com/acme/my-api DEST=../my-api
cd ../my-api
go mod tidy        # 下载 go.mod 里写的 kit 版本
make check
```

升级：

```sh
go get github.com/xnqOS/go-starter-kit/kit@v0.2.0
go mod tidy
make check
```

在项目里临时调试 kit：克隆本仓库，在项目里执行 `go work init . ../go-starter-kit/kit`，或临时加一条 `replace`。改好后回到本仓库提 PR、打 tag，项目里再升级。不要把 `go.work` 和本地 `replace` 提交到项目里。

## 公开仓库

本仓库是公开的，kit 通过公共代理 `proxy.golang.org` 下载，本机、CI 和 Docker 构建都不需要额外配置。

如果把模板复制到私有仓库里继续开发自己的 kit，新项目就要配置 `GOPRIVATE`，CI 和 Docker 构建也要提供 GitHub 凭证，否则 `go mod download` 会报 `could not read Username`。
