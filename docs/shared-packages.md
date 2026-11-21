# 公共基础包

这些包与业务无关，位于独立的 `kit/` module（import 路径为 `github.com/xnqOS/go-starter-kit/kit/<包名>`，见 [公共 kit module](kit-module.md)）。模板默认**不接入路由**，需要时在模块或 `internal/app` 中引用。邮件和文件存储见 [邮件](email.md)、[文件存储](storage.md)。全部只依赖标准库、`apperror`、`httpx` 和已有依赖（argon2 来自 `golang.org/x/crypto`）。

| 包 | 用途 |
|---|---|
| `httpx`（辅助函数） | 列表参数、Bearer token、JSON 绑定、结果输出 |
| `database`（约束、savepoint） | 识别唯一键 / 外键 / CHECK 冲突；事务内局部回滚 |
| `credential` | argon2id 密码哈希、随机 token 及其哈希、邮箱规范化 |
| `admission` | 限制昂贵操作的并发数，排队有上限，超载返回 503 |
| `ratelimit` | 固定内存的按 IP 限流中间件，返回 429 |
| `secretbox` | AES-256-GCM 加密存入数据库的密钥，绑定行 ID |
| `humancheck` | Cloudflare Turnstile 人机校验 |

## httpx 辅助函数

```go
func (h *Handler) List(c *gin.Context) {
    query, ok := httpx.ParseListParams(c) // 非整数时已回复 400
    if !ok {
        return
    }
    result, err := h.service.List(ctx, ListParams{Limit: query.Limit, Offset: query.Offset})
    httpx.PageResult(c, result.Items, httpx.Pagination{...}, err)
}

func (h *Handler) Update(c *gin.Context) {
    httpx.BindJSON(c, func(in UpdateRequest) {
        item, err := h.service.Update(ctx, c.Param("id"), in.Name)
        httpx.Result(c, http.StatusOK, item, err)
    })
}

token := httpx.BearerToken(c) // 无效或缺失时为 ""
```

`ParseListParams` 只检查是否为整数，默认值和上下限仍由 Service 负责（`limit=0` 表示默认值）。`make module` 生成的 `List` 已使用它。

## database：约束冲突与 savepoint

```go
if err := tx.Create(&row).Error; database.Constraint(err) == database.ConstraintUnique {
    return apperror.New(apperror.ErrConflict, "email_taken", "email is already registered")
}
```

`Constraint` 只分类驱动错误（SQLite 和 PostgreSQL），公开文案由调用方决定，SQL 细节不会外泄。

`WithSavepoint(tx, fn)` 在已有事务中执行一段操作，失败（返回错误、panic 或 context 取消）时只回滚这一段，外层事务可以继续。PostgreSQL 中语句出错会让整个事务进入 aborted 状态，用 savepoint 包住"可能冲突、冲突后改走其他分支"的语句即可继续。它不会开启或提交根事务；传入非事务的 `*gorm.DB` 返回 `ErrInvalid`。回滚或释放本身失败时返回 `ErrSavepointRecovery`，此时不要提交外层事务。回调 panic 时会先回滚再继续 panic；如果这时清理也失败，panic 的值会换成同时包含 `ErrSavepointRecovery` 和原 panic 的 error，在外层 `recover()` 的代码要用 `errors.Is` 检查它。

## credential：密码与 token

```go
hash, err := credential.HashPassword(password) // 12–1024 字节，否则 ErrInvalid
ok := credential.VerifyPassword(hash, password)

raw := credential.NewToken()        // 发给客户端
stored := credential.HashToken(raw) // 只存哈希，按哈希查找

email, err := credential.NormalizeEmail(input) // 小写、去空格、只接受纯地址
```

argon2id 参数为 OWASP 最低推荐（19 MiB 内存、2 轮），每次约几十毫秒并占用 19 MiB，务必用 `admission.Gate` 限制并发。修改参数会使旧哈希无法验证，需要先支持旧参数再升级。

## admission：限制并发

```go
var passwordGate = admission.New(2, 32, 2*time.Second) // 2 个并发，最多 32 个排队，最多等 2s

err := passwordGate.Do(ctx, func() error {
    hash, err = credential.HashPassword(password)
    return err
})
```

排队已满或等待超时返回 `apperror.ErrUnavailable`（503 + `Retry-After`），请求取消返回 `ctx.Err()`（499）。不同流量（例如后台登录和用户注册）用不同的 Gate，避免一类请求占满另一类的容量。

## ratelimit：按 IP 限流

```go
login := ratelimit.New(20, time.Minute) // 每个客户端突发 20 次，每分钟恢复 20 次
api.POST("/login", ratelimit.Middleware(login, nil), handler.Login)

// 按租户分别计数；同一 IP 跨租户合计最多 2 倍额度
shop := ratelimit.New(60, time.Minute)
api.Use(ratelimit.Middleware(shop, func(c *gin.Context) string { return c.Param("tenant") }))
```

- 客户端按 IPv4 地址或 IPv6 /64 识别；同一 IPv4 /24 或 IPv6 /48 合计最多 16 倍额度，换地址无法绕过。
- 内存固定（默认约 1.3 MiB），不随客户端数量增长；哈希冲突时共享额度，宁可多限。
- 状态保存在进程内，多实例部署时每个实例单独计数。
- 客户端 IP 来自 `c.ClientIP()`。API 在反向代理后面时必须设置 `TRUSTED_PROXY_CIDRS`，否则所有请求共用代理的额度。
- 放在解析请求体和访问数据库之前。

## secretbox：加密存储密钥

```go
key, err := secretbox.ParseKey("APP_SECRETS_KEY", os.Getenv("APP_SECRETS_KEY")) // openssl rand -base64 32
box, err := secretbox.New(key)

sealed, err := box.Seal(row.ID, []byte(apiKey)) // 存入 TEXT 列，格式 "v1:<base64>"
plain, err := box.Open(row.ID, sealed)          // 换行、换密钥或被篡改时返回 ErrCannotOpen
```

行 ID 作为认证数据，密文复制到其他行后无法解密。密钥丢失后数据无法恢复；更换密钥需要先用旧密钥解密再用新密钥加密。

## humancheck：Cloudflare Turnstile

```go
check := humancheck.NewTurnstile(secretKey, "https://app.example.com", cfg.Development())

switch err := check.Verify(ctx, req.Token, c.ClientIP(), "signup"); {
case errors.Is(err, humancheck.ErrRejected):
    httpx.Fail(c, apperror.New(apperror.ErrInvalid, "human_check_failed", "human check failed"))
case err != nil:
    httpx.Fail(c, err) // Cloudflare 不可用或返回 internal-error 时为 503；请求取消时为 499
}
```

除 Cloudflare 自身的有效期和一次性校验外，还会检查 token 是否签发给本站 hostname 和指定 action，其他站点或其他表单的 token 无法通过。Cloudflare 的测试 key 只在开发环境（第三个参数为 true）接受。
