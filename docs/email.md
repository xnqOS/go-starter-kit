# 邮件：provider 适配层和 outbox

邮件代码在 kit module 里（`github.com/xnqOS/go-starter-kit/kit/email/...`），从一个多租户电商项目中抽出来。模板默认**不接入**邮件，也不读任何 `MAIL_*` 环境变量；需要发邮件的项目按下面的步骤自己接上。

| 包 | 用途 |
|---|---|
| `email` | `Provider` 接口、`Outbound` 消息、失败分类（`Retryable` / `Ambiguous` / `Permanent`）、`Disabled` 和 `Log` 两个内置 provider、webhook 事件类型、发信域名接口 |
| `email/smtp` | SMTP（STARTTLS 或隐式 TLS） |
| `email/resend` | Resend API，支持 `Idempotency-Key`、发信域名管理、Svix 签名的 webhook |
| `email/sendgrid` | SendGrid v3 API，支持发信域名管理、Signed Event Webhook |
| `email/emailtest` | adapter 的合约测试：新增 provider 时用它验证行为一致 |
| `email/outbox` | 持久化发信队列：在业务事务里排队，worker 发送、重试，正文加密存储 |

## provider：直接发一封

```go
p := smtp.New(smtp.Config{Address: "smtp.example.com:587", Username: user, Password: pass})
_, err := p.Send(ctx, email.Outbound{
    From:    mail.Address{Name: "My App", Address: "no-reply@example.com"},
    To:      mail.Address{Address: "user@example.com"},
    Subject: "验证你的邮箱",
    Text:    "验证码：123456",
})
switch email.Classify(err) {
case email.Retryable: // 对方肯定没收到，可以重试
case email.Ambiguous: // 可能已经收到（写完请求后超时、5xx），重试可能重复
case email.Permanent: // 地址无效、域名未验证、密钥错误，重试没用
}
```

adapter 只负责投递，不改内容：`Outbound` 里的主题、正文都由项目渲染好。`email.Code(err)` 返回 `http_429`、`smtp_550` 这类可以写进日志的短代码，不含正文和密钥。

直接 `Send` 只适合不怕丢的邮件。验证码、重置密码这类和业务数据一起产生的邮件，用 outbox。

## 在项目里按配置选 provider

模板没有邮件配置项，下面是项目里 `internal/config` 和组装代码的参考写法（变量名自定）：

```go
func newProvider(c config.Mail) (email.Provider, error) {
    switch c.Provider {
    case "", "disabled":
        return email.Disabled{}, nil
    case "log": // 本地开发：把邮件（含 token）打到日志
        return email.Log{}, nil
    case "smtp":
        return smtp.New(smtp.Config{Address: c.SMTPAddress, Username: c.SMTPUsername, Password: c.SMTPPassword, TLSMode: c.SMTPTLSMode}), nil
    case "resend":
        return resend.New(resend.Config{APIKey: c.ResendAPIKey, WebhookSecret: c.ResendWebhookSecret}), nil
    case "sendgrid":
        return sendgrid.New(sendgrid.Config{APIKey: c.SendGridAPIKey, WebhookPublicKey: c.SendGridWebhookKey})
    }
    return nil, fmt.Errorf("unknown MAIL_PROVIDER %q", c.Provider)
}
```

记得把新变量加进部署配置目录（`make deploy-config` 的配置项）和 `.env.deploy.example`。

## outbox：在事务里排队

### 1. 建表

outbox 的表结构由 kit 提供，但迁移文件属于项目。在项目里新增一个迁移，调用冻结的建表函数：

```go
// migrations/000002_create_email_outbox.go
package migrations

import (
    "gorm.io/gorm"

    "github.com/xnqOS/go-starter-kit/kit/email/outbox"
)

func createEmailOutboxV000002(tx *gorm.DB) error {
    return outbox.CreateTableV1(tx)
}
```

然后在 `migrations/migrations.go` 的列表里登记 `{"000002_create_email_outbox", createEmailOutboxV000002}`。

`CreateTableV1` 的 SQL 永远不改（有测试锁住它的校验和）。以后 kit 需要改表，会新增 `MigrateV2` 之类的函数，项目再加一个新迁移去调用。升级 kit 版本时看一下 kit 的变更说明里有没有新的迁移函数。

### 2. 创建并启动

```go
key, err := secretbox.ParseKey("MAIL_OUTBOX_KEY", cfg.OutboxKey) // openssl rand -base64 32
box, err := outbox.New(db, provider, key, logger, outbox.Options{
    // 发生"结果不确定"的失败后，哪些类型可以再发一次。
    // 验证码、链接可以；订单通知这类重复了会让人困惑的不行。
    ResendSafe: func(kind string) bool { return kind == "email_verification" || kind == "password_reset" },
})
box.Start()
defer box.Close(shutdownCtx) // 等正在发的邮件发完；超时后取消，未完成的行租约到期后由其他 worker 接手
```

所有实例必须用同一个 `MAIL_OUTBOX_KEY`；换 key 后，用旧 key 加密、还没发出的邮件会被标记为 `dead`（`undecryptable`）。

### 3. 在业务事务里排队

```go
err := db.Transaction(func(tx *gorm.DB) error {
    if err := tx.Create(&resetToken).Error; err != nil {
        return err
    }
    return box.Deliver(ctx, tx, outbox.Message{
        Kind:      "password_reset",
        Outbound:  email.Outbound{From: from, To: mail.Address{Address: user.Email}, Subject: subject, Text: body},
        ExpiresAt: resetToken.ExpiresAt,                                      // 过期后不再发送，正文被清掉
        Receipt:   &outbox.Receipt{Table: "password_reset_tokens", ID: resetToken.ID}, // 可选
    })
})
```

- 事务回滚，邮件也不存在；事务提交，邮件才进入队列。`Deliver` 在 savepoint 里执行，返回错误时 `tx` 仍可继续使用。`tx` 传 `nil` 时 outbox 自己开事务。
- `Outbound.IdempotencyKey` 相同的邮件只排队一次，重复调用返回成功。不填时用行 ID。支持去重的 provider（Resend）也会收到这个 key。
- 队列满（默认最多 1000 封待发，同一地址最多 5 封）返回 `ErrQueueFull`，可以回 503。等待重试的行不算，所以 provider 故障时新邮件还能进来。
- 收件人为空、头部有换行（注入）、`Receipt` 表名不合法，直接返回 `ErrInvalidMessage`。
- `Receipt` 指向的表需要两个可空时间列 `sent_at`、`delivery_failed_at`。最终结果和 outbox 状态在同一个事务里写入。

### 4. 状态和重试

```
queued  ─领取→ sending ─成功→ sent
failed  ─领取→ sending ─可重试→ failed（按 Backoff 稍后再试）/ dead（次数用完）
                       ─永久失败→ dead
                       ─结果不确定且不能重发→ unknown ─webhook→ sent
                                                      ─24h→ dead
```

- worker 用带租约的条件 UPDATE 领取（PostgreSQL 加 `FOR UPDATE SKIP LOCKED`），多个进程共用一个数据库也不会重复发。
- worker 崩溃后租约到期，行会被重新领取；这次尝试的结果未知，按"结果不确定"处理。
- "结果不确定"之后能不能重发：`Options.ResendSafe(kind)` 返回 true 的邮件类型可以随时重发；否则只有支持去重的 provider（实现了 `IdempotencyWindow()`，Resend 是 24 小时）才能重发，而且要在去重窗口之内。窗口从最早一次可能已被接收的尝试开始算（记在 `maybe_accepted_at`）。超出窗口时，行转为 `unknown`（`resend_unsafe`，回收租约时为 `lease_expired`），等 webhook 结算，不会重复投递。
- janitor 每分钟处理过期邮件、超时的 unknown 行，并删除超过保留期（默认 30 天）的已完成行。
- 可选 `Options.Suppressions`：每次发送前检查地址是否已退订或硬退信，命中则标记 `dead`（`suppressed`）。
- `Providers().Add(p)` 可以注册多个 provider；`Message.Provider` 指定用哪个，不填时记下当前默认 provider 的名字，所以之后切换默认 provider 不会影响已经排队的邮件。

## webhook：确认投递

provider 的 webhook 能证明邮件已被接收，用来结算 `unknown` 行，也能在超时重试之前发现邮件其实已经送达。每次发送都会带上 `outbox_id` 标签，webhook 优先按它找行，找不到再按 provider 的消息 ID 找。

```go
const maxWebhookBody = 2 << 20

func registerEmailWebhooks(api *gin.RouterGroup, box *outbox.Outbox, handlers ...email.WebhookHandler) {
    byName := map[string]email.WebhookHandler{}
    for _, h := range handlers {
        byName[h.Name()] = h
    }
    api.POST("/email/webhooks/:provider", func(c *gin.Context) {
        h, ok := byName[c.Param("provider")]
        if !ok {
            httpx.Reject(c, http.StatusNotFound, "not_found", "route not found")
            return
        }
        body, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, maxWebhookBody))
        if err != nil {
            httpx.Reject(c, http.StatusBadRequest, "invalid_request", "could not read webhook body")
            return
        }
        events, err := h.Parse(c.Request, body) // 校验签名；时间戳超过 5 分钟拒绝
        if errors.Is(err, email.ErrBadSignature) {
            httpx.Reject(c, http.StatusUnauthorized, "unauthorized", "invalid webhook signature")
            return
        }
        if err != nil {
            httpx.Reject(c, http.StatusBadRequest, "invalid_request", "invalid webhook payload")
            return
        }
        for _, event := range events {
            if err := box.Resolve(c.Request.Context(), event); err != nil {
                // 5xx 让 provider 稍后重发；Resolve 可以重复执行
                httpx.Reject(c, http.StatusInternalServerError, "internal_error", "could not store webhook")
                return
            }
        }
        httpx.NoContent(c)
    })
}
```

`resend.Provider` 和 `sendgrid.Provider` 实现了 `email.WebhookHandler`（需要在 Config 里填 webhook 密钥）。如果还要把事件记下来做去重、维护退订名单，在自己的事务里调用 `outbox.ResolveEvent(tx, event)`，和这些写入一起提交。

SendGrid 的自定义参数在事件的顶层，和它自己的字段混在一起，所以只读回 `sendgrid.Config.WebhookTags` 列出的标签，默认是 `kind` 和 `outbox_id`。

## 发信域名

`resend` 和 `sendgrid` 实现了 `email.DomainManager`（`CreateDomain` / `VerifyDomain` / `DeleteDomain`），返回需要添加的 DNS 记录。`email.Log` 也实现了它，所有域名立即通过验证，方便本地试流程。kit 只提供 provider 这一层；域名存在哪、谁能管理，由项目决定。

## 没有放进 kit 的部分

这些和电商业务绑得太紧，没有抽：

- **多租户作用域**：`Scope{TenantID, StoreID}`、平台邮件和商家邮件分 worker、按租户和店铺分别限额、每个租户同一时间只发一封。
- **营销邮件**：`marketing` stream、活动的独立额度、取消（`canceled_at`）、退订链接和 consent。
- **模板**：`templates` 包（邮件类型、渲染、哪些类型可以重发）。kit 的 outbox 只接收渲染好的 `Outbound`，用 `ResendSafe` 代替模板里的重发规则。
- **发信身份和域名绑定**：商家自有域名的验证、绑定、在 provider 之间迁移（`domains.go`、`sender_id`、`binding_id`）。
- **退订和抑制名单的存储**：`Store`、webhook 事件去重表。kit 只定义了 `Suppressions` 接口。

需要这些能力的项目自己在外层实现；provider 层可以直接用 kit 的。
