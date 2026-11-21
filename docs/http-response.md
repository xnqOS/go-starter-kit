# HTTP 响应约定

业务 API 统一使用 `code / message / data / request_id`。分页列表额外提供顶层 `pagination`。所有响应通过 `kit/httpx` 输出，业务 Service 不依赖 Gin 或 HTTP 状态码。

## 常见写法与取舍

| 常见写法 | Starter 的选择 |
| --- | --- |
| 集中处理结果和错误；包含整数 `code`、`msg`、`success`；列表用 `data.items/total` | 集中输出和错误映射；业务码采用稳定字符串；保留实际 HTTP 错误状态 |
| 成功响应为 `message/data`，列表使用顶层 `pagination`；错误为字符串 `error` | 使用完整名称 `message` 和顶层分页，成功与失败共用信封 |

`code` 用于程序分支，`message` 用于展示；不要通过比较文案判断错误。成功统一为 `code: "ok"`，业务失败使用如 `invalid_input`、`order_already_paid` 的稳定标识。HTTP 状态表达请求结果，业务码进一步说明原因。无需再增加 `success`、`ok` 或同时提供 `msg` 和 `message`。

## 成功、列表与失败

创建成功：HTTP **201**，同时返回 `Location` 响应头。

```json
{
  "code": "ok",
  "message": "success",
  "data": {"id": "example-id", "name": "hello"},
  "request_id": "server-generated-id"
}
```

列表成功：HTTP **200**。`data` 始终是数组，空列表为 `[]`。分页字段仅在分页响应中出现。

```json
{
  "code": "ok",
  "message": "success",
  "data": [],
  "request_id": "server-generated-id",
  "pagination": {"limit": 20, "offset": 100, "total": 42}
}
```

`total` 是符合全部查询条件的记录总数，不是当前页长度。超过末页时仍返回真实总数。默认 `limit=20`，最大 100，`offset` 从 0 开始；显式 `limit=0` 使用默认值。不同时接受或输出 `page/per_page/total_page`，避免同一个请求有两套分页含义。

SQL Repo 在 `database.ReadTx` 中先查询当前页；不足一页且非空时，使用 `offset + 当前页长度` 得到总数，空的第一页总数为 0。满页或 offset 大于 0 的空页仍会执行 COUNT，以返回真实总数。分页与计数使用同一快照，后续增加搜索、用户范围等过滤时，只在共享的 `base` 查询上添加条件。生成的模块遵循相同规则。传入已有事务时会复用该事务，其隔离级别由调用方负责。

失败示例：HTTP **400**。`data` 固定为 `null`，不再嵌套 `error` 对象。

```json
{
  "code": "invalid_input",
  "message": "name must contain 1 to 200 characters",
  "data": null,
  "request_id": "server-generated-id"
}
```

未知错误返回 HTTP **500**，固定 `code: "internal_error"` 与通用文案。错误详情进入日志，不把数据库、第三方 SDK、堆栈等内部诊断直接发给客户端。

## HTTP 状态与公共错误

| HTTP 状态 | 默认业务码 | 来源或用途 |
| --- | --- | --- |
| 200 | `ok` | 查询、更新或返回结果的操作 |
| 201 | `ok` | 创建资源 |
| 204 | 无响应体 | 删除成功 |
| 400 | `invalid_input` | `apperror.ErrInvalid` |
| 400 | `invalid_json` | JSON 格式、未知字段或多段 JSON |
| 401 | `unauthorized` | `apperror.ErrUnauthorized` |
| 403 | `forbidden` | `apperror.ErrForbidden` |
| 404 | `not_found` | `apperror.ErrNotFound` 或未知路由 |
| 405 | `method_not_allowed` | 不支持的 HTTP 方法 |
| 409 | `conflict` | `apperror.ErrConflict` |
| 413 | `body_too_large` | JSON 请求体超过 1 MiB |
| 415 | `unsupported_media_type` | 请求 Content-Type 不受支持 |
| 429 | `rate_limited` | `apperror.ErrRateLimited` |
| 499 | `request_canceled` | 客户端断开，`context.Canceled`（访问日志记为 Info） |
| 500 | `internal_error` | 未识别错误或 panic |
| 503 | `unavailable` | `apperror.ErrUnavailable`，或请求 deadline 超时 `context.DeadlineExceeded`（后者不记 Error 日志） |
| 503 | `not_ready` | 就绪检查的数据库连接不可用 |

这些错误分类提供映射能力；认证、权限、限流策略仍需具体项目实现。

`httpx.Fail` 对 `ErrUnavailable` 和请求 deadline 超时统一添加 `Retry-After: 1`，详情、写操作和分页错误一致。

需要让客户端识别具体业务原因时，使用明确的公开错误：

```go
return apperror.New(
    apperror.ErrConflict,
    "order_already_paid",
    "order is already paid",
)
```

Handler 统一调用 `httpx.Fail(c, err)`，上例返回 HTTP 409。`apperror.New` 的文案必须适合公开展示，类别不携带 HTTP 依赖。`errors.Is`、`errors.As` 支持外层包装；外层附带的内部诊断不会进入响应。没有显式公开文案的分类错误使用默认提示，未知分类仍按 500 处理。

## Handler 写法

```go
httpx.Data(c, http.StatusCreated, item)
httpx.Page(c, result.Items, httpx.Pagination{
    Limit: result.Limit, Offset: result.Offset, Total: result.Total,
})
httpx.NoContent(c)
httpx.Fail(c, err)
// 中间件或路由层已明确知道状态和公共信息时：
httpx.Reject(c, http.StatusForbidden, "forbidden", "access denied")

// 同时处理结果和错误；列表参数、JSON 绑定和 Bearer token 也有公共辅助函数。
httpx.Result(c, http.StatusOK, item, err)
httpx.PageResult(c, result.Items, pagination, err)
httpx.BindJSON(c, func(in CreateRequest) { /* 调用业务 Service */ })
query, ok := httpx.ParseListParams(c)
token := httpx.BearerToken(c)
```

一个请求只选一个输出分支；失败后立即返回。分页相关的 `ListResult` 留在业务模块，HTTP 信封与分页序列化交给 `httpx`。`make module` 生成的模块使用同一套约定。

## 请求 ID 与协议例外

`request_id` 由服务端生成，与 `X-Request-ID` 响应头、访问日志和业务日志一致；不会直接信任客户端传入的同名请求头。前端可以在报错反馈中附上这个 ID。

删除成功的 **204 没有 JSON 响应体**，客户端不能无条件调用 `response.json()`；请求 ID 仍在响应头中。这符合 [RFC 9110 的 204 定义](https://www.rfc-editor.org/rfc/rfc9110.html#name-204-no-content)。

`/health`、`/ready` 成功时分别保持简洁的 `{"status":"ok"}` 和 `{"status":"ready"}`，失败使用统一错误格式。文件下载、流式响应以及第三方回调按自身协议处理。

## 从原 Starter 响应迁移

原来的成功 `data` 字段保持原位，新增 `code/message/request_id`。前端读取错误需从 `error.code/error.message` 改为顶层 `code/message`。列表新增 `pagination`；内部 Repo 的 `List` 同时返回记录和真实总数，Service 返回包含有效 limit/offset 的 `ListResult`。

这是响应契约更新，未保留两套并行字段。已有项目应同步更新客户端与手写模块；生成器只影响以后新增的模块。
