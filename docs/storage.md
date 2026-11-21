# 文件存储

`kit/storage` 用一个小接口保存上传文件、生成的文件等二进制对象。项目可以先存在本地磁盘，以后换成对象存储，业务代码不用改。模板默认没有接入。

```go
type Storage interface {
    Put(ctx context.Context, key string, data []byte) error           // key 已存在时返回 ErrExists
    Open(ctx context.Context, key string) (io.ReadSeekCloser, error)  // 不存在时返回 ErrNotFound
    Delete(ctx context.Context, key string) error                     // 不存在不算错误
}
```

## key 的规则

key 由服务端生成，**不要用用户上传的文件名**。格式是若干层文件夹加文件名，每段只能有字母、数字、`_`、`-`（1–128 个字符），最多 7 层文件夹，最后是小写扩展名，例如 `T1/S1/IMG01.png`。`..`、空段、开头的 `/` 都不合法。

```go
key, err := storage.Key("png", tenantID, storeID, imageID) // "tenantID/storeID/imageID.png"
storage.ValidKey(key)                                      // 校验已有的 key
```

ID 里有不合法字符时 `Key` 返回 `ErrInvalidKey`，调用方可以退回不带文件夹的 key。按归属分文件夹的好处是以后可以按前缀统计、列出或删除一批对象。

对象不会被覆盖：同一个 key 第二次 `Put` 返回 `ErrExists`。要替换内容就换一个新 key，再删掉旧的。

`ErrNotFound` 同时满足 `errors.Is(err, os.ErrNotExist)`，`ErrExists` 同时满足 `errors.Is(err, os.ErrExist)`。

## Local：本地磁盘

```go
files, err := storage.NewLocal(os.Getenv("UPLOAD_DIR")) // 目录为空时报错，不会悄悄写进当前目录
```

- 所有读写都经过 `os.Root`，key 和符号链接都不能访问目录以外的文件。
- 文件权限 0600，目录 0700。删除对象后会顺带删掉变空的上层文件夹。
- 容器部署时目录要放在数据卷里（例如 `/app/data/uploads`），否则换容器后文件会丢失。

## Worker：通过 HTTP 存到对象存储

`Worker` 是一个 HTTP 客户端，对接一个只给 API 调用的小服务，通常是 Cloudflare Worker 加 R2 bucket。协议：

| 请求 | 结果 |
| --- | --- |
| `PUT /objects/<key>` | 201；key 已存在返回 409 |
| `GET /objects/<key>` | 200 带内容；不存在返回 404 |
| `DELETE /objects/<key>` | 204，已经不存在也返回 204（404 也接受） |

每个请求都带 `Authorization: Bearer <secret>`，`PUT` 的 `Content-Type` 按扩展名推断（未知的用 `application/octet-stream`）。

```go
files, err := storage.NewWorker(os.Getenv("STORAGE_WORKER_URL"), os.Getenv("STORAGE_WORKER_SECRET"), storage.WorkerOptions{
    MaxSize: 32 << 20, // 默认 32 MiB；不能超过 Worker 自己的上限
})
```

- URL 必须是 https，只有 localhost 允许 http（本地开发）；secret 至少 32 个字符（`openssl rand -hex 32`）。
- `Open` 会把对象整个读进内存（最多 `MaxSize`），这样结果可以 seek，`http.ServeContent` 需要这一点。大文件不适合这种方式。
- Worker 不做权限判断，API 在每次读取前仍要自己检查权限。

**本仓库还没有附带 Worker 实现。** 需要自己按上面的协议写一个 Cloudflare Worker（绑定 R2，校验 Bearer 密钥，不覆盖已存在的 key），这是后续工作。

## 接入示例

```go
var files storage.Storage
switch cfg.FileStorage {
case "worker":
    files, err = storage.NewWorker(cfg.WorkerURL, cfg.WorkerSecret, storage.WorkerOptions{})
default:
    files, err = storage.NewLocal(cfg.UploadDir)
}

key, err := storage.Key("pdf", userID, reportID)
if err := files.Put(ctx, key, data); err != nil { ... }
// 把 key 存进数据库；读取时先检查权限，再 files.Open(ctx, key)
```

新增的环境变量要同时加进 `internal/deploycatalog` 和 `.env.deploy.example`（见 [部署配置页](deployment.md#部署配置页)）。
