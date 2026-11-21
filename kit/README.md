# kit

`github.com/xnqOS/go-starter-kit/kit`：从 Go Starter Kit 拆出的公共基础包，新项目作为依赖引用。

```sh
go get github.com/xnqOS/go-starter-kit/kit@latest
```

tag 格式为 `kit/vX.Y.Z`。发布和升级见 [../docs/kit-module.md](../docs/kit-module.md)，各包用法见 [../docs/shared-packages.md](../docs/shared-packages.md)、[../docs/email.md](../docs/email.md)、[../docs/storage.md](../docs/storage.md)。

本 module 只能依赖标准库和第三方库，不能 import 模板里的 `internal/` 包。
