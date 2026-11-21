// Package deploycatalog lists this project's deployment settings for
// `make deploy-config` (see kit/deployconfig). The test fails when the code
// reads an environment variable that is not described here or in
// NotDeployed, or when an entry is missing from .env.deploy.example, so a
// feature that adds configuration has to document it.
package deploycatalog

import (
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/xnqOS/go-starter-kit/kit/deployconfig"

	"starter-kit/internal/config"
)

type (
	Values = deployconfig.Values
	Level  = deployconfig.Level
)

const (
	required    = deployconfig.Required
	recommended = deployconfig.Recommended
)

var when = deployconfig.When

func syslog(v Values) bool { return strings.Contains(v["LOG_OUTPUTS"], "syslog") }
func tls(v Values) bool    { return syslog(v) && v["LOG_SYSLOG_NETWORK"] == "tls" }

// New returns the catalog. Add an entry here whenever the code reads a new
// environment variable that belongs in .env.deploy.
func New() *deployconfig.Catalog {
	return &deployconfig.Catalog{
		Groups: []string{"基础", "数据库", "日志"},
		Vars:   vars(),
		NotDeployed: map[string]string{
			"TEST_DATABASE_URL":                 "只给测试用",
			"VICTORIALOGS_TEST_URL":             "只给测试用",
			"VICTORIALOGS_TEST_SYSLOG_ADDRESS":  "只给测试用",
			"VICTORIALOGS_TEST_SYSLOG_NETWORK":  "只给测试用",
			"VICTORIALOGS_TEST_TLS_CA_FILE":     "只给测试用",
			"VICTORIALOGS_TEST_TLS_SERVER_NAME": "只给测试用",
		},
		Startup: func(Values) error {
			_, err := config.Load()
			return err
		},
		Notes: []string{
			"API 只监听主机的 127.0.0.1。对外服务时在主机上用 Caddy、Nginx 或隧道提供域名和 HTTPS，再转发到 127.0.0.1:8080（用 API_PORT 改过端口的话换成那个端口），并把代理的地址写进 TRUSTED_PROXY_CIDRS。",
			"升级前先备份数据库，步骤见 docs/deployment.md「SQLite 数据与备份」。",
		},
		NextSteps: "在服务器上的项目根目录执行 make deploy，然后 curl --fail http://127.0.0.1:8080/ready。如果文件是在本机生成的，先把它复制到服务器的 .env.deploy（例如 scp），再在服务器上 chmod 600。",
	}
}

func vars() []deployconfig.Var {
	return []deployconfig.Var{
		// --- 基础 ---
		{
			Name: "HTTP_ADDR", Group: "基础", Title: "API 监听地址",
			Help:    "容器里固定用 :8080，和 compose.deploy.yaml 的端口映射、健康检查一致。主机端口用 make deploy API_PORT=… 改，不要改这里。",
			Default: ":8080", Suggested: ":8080", Advanced: true,
			Need: func(v Values) (Level, string) {
				return when(!v.Empty("HTTP_ADDR") && v["HTTP_ADDR"] != ":8080", recommended, "容器里应保持 :8080，否则健康检查和端口映射对不上")
			},
		},
		{
			Name: "GIN_MODE", Group: "基础", Title: "Gin 运行模式",
			Help:    "线上用 release，否则 Gin 会打印调试信息。由 Gin 自己读取。",
			Default: "debug", Suggested: "release", Options: []string{"release", "debug", "test"},
			Need: func(v Values) (Level, string) {
				return when(v["GIN_MODE"] != "release", recommended, "线上应设为 release")
			},
		},
		{
			Name: "TRUSTED_PROXY_CIDRS", Group: "基础", Title: "可信反向代理",
			Help:    "API 前面有反向代理（Caddy、Nginx、隧道）时，列出代理的 IP 或 CIDR，逗号分隔。只有来自这些地址的 X-Forwarded-For 会被采信，限流和日志才能看到真实客户端 IP。不设则不信任任何代理。",
			Example: "172.16.0.0/12",
		},

		// --- 数据库 ---
		{
			Name: "STORAGE", Group: "数据库", Title: "存储类型",
			Help:    "sqlite 把数据存在容器数据卷里的文件中；postgres 连接外部 PostgreSQL；memory 只用于演示，重启后数据丢失。",
			Default: "sqlite", Suggested: "sqlite", Options: []string{"sqlite", "postgres", "memory"},
			Need: func(v Values) (Level, string) {
				return when(v["STORAGE"] == "memory", recommended, "memory 重启后数据全部丢失，线上不要用")
			},
		},
		{
			Name: "DATABASE_URL", Group: "数据库", Title: "数据库地址",
			Help:    "STORAGE=sqlite 时是文件路径，容器里用 /app/data/app.db（在数据卷里）；STORAGE=postgres 时是 postgres:// 连接串，建议带 sslmode=verify-full。",
			Default: "./data/app.db（仅 sqlite）", Suggested: "/app/data/app.db", Example: "postgres://app:password@db.example.com:5432/app?sslmode=verify-full",
			Secret:  true,
			Applies: func(v Values) bool { return v["STORAGE"] != "memory" },
			Need: func(v Values) (Level, string) {
				switch v["STORAGE"] {
				case "postgres":
					if !strings.HasPrefix(v["DATABASE_URL"], "postgres://") && !strings.HasPrefix(v["DATABASE_URL"], "postgresql://") {
						return required, "STORAGE=postgres 时必须是 postgres:// 连接串"
					}
				case "", "sqlite":
					if !v.Empty("DATABASE_URL") && !strings.HasPrefix(v["DATABASE_URL"], "/app/data/") {
						return recommended, "容器里只有 /app/data 在数据卷里，放在别处的数据库换容器后会丢失"
					}
				}
				return "", ""
			},
		},

		// --- 日志 ---
		{
			Name: "LOG_SERVICE", Group: "日志", Title: "应用名",
			Help:    "写进每条日志，用来在日志服务器里区分应用。1–48 个可打印 ASCII 字符，不含空格。",
			Default: "starter-kit", Example: "order-api",
			Need: func(v Values) (Level, string) {
				return when(v.Empty("LOG_SERVICE") || v["LOG_SERVICE"] == "starter-kit", recommended, "改成这个项目自己的名字")
			},
		},
		{
			Name: "LOG_ENV", Group: "日志", Title: "运行环境",
			Help:    "写进每条日志。线上用 production；development 和 local 会被当成开发机。",
			Default: "development", Suggested: "production",
			Need: func(v Values) (Level, string) {
				return when(v["LOG_ENV"] == "" || v["LOG_ENV"] == "development" || v["LOG_ENV"] == "local", recommended, "线上应设为 production")
			},
		},
		{
			Name: "LOG_LEVEL", Group: "日志", Title: "日志级别",
			Help:    "最低记录级别。",
			Default: "info", Suggested: "info", Options: []string{"debug", "info", "warn", "error"},
		},
		{
			Name: "LOG_OUTPUTS", Group: "日志", Title: "日志输出",
			Help:    "stdout 由 Docker 保存（make deploy-logs 查看）；syslog 发到日志服务器（见 docs/logging.md）；可以写 stdout,syslog 同时输出。",
			Default: "stdout", Suggested: "stdout", Options: []string{"stdout", "syslog", "stdout,syslog"},
		},
		{
			Name: "LOG_INSTANCE", Group: "日志", Title: "实例名",
			Help:    "同一应用的不同实例用不同的名字。默认是主机名（容器里是容器 ID）。",
			Default: "主机名", Advanced: true,
		},
		{
			Name: "LOG_SYSLOG_ADDRESS", Group: "日志", Title: "Syslog 服务器",
			Help:    "host:port。API 在容器里时，127.0.0.1 指向容器自己；用日志服务器的地址或同一 Compose 网络里的服务名。",
			Default: "127.0.0.1:5140", Example: "logs.example.com:6514",
			Applies: syslog,
			Need: func(v Values) (Level, string) {
				if !syslog(v) {
					return "", ""
				}
				if v.Empty("LOG_SYSLOG_ADDRESS") {
					return recommended, "默认的 127.0.0.1 在容器里指向 API 自己，日志发不出去"
				}
				host, port, err := net.SplitHostPort(v["LOG_SYSLOG_ADDRESS"])
				n, _ := strconv.Atoi(port)
				return when(err != nil || host == "" || n < 1 || n > 65535, required, "必须是 host:port")
			},
		},
		{
			Name: "LOG_SYSLOG_NETWORK", Group: "日志", Title: "Syslog 传输方式",
			Help:    "跨机器发送时用 tls。",
			Default: "tcp", Options: []string{"tcp", "tls"}, Applies: syslog,
			Need: func(v Values) (Level, string) {
				return when(syslog(v) && v["LOG_SYSLOG_NETWORK"] != "tls", recommended, "跨机器发送日志建议用 tls")
			},
		},
		{
			Name: "LOG_SYSLOG_FACILITY", Group: "日志", Title: "Syslog facility",
			Help:    "user、daemon 或 local0–local7。",
			Default: "local0", Advanced: true, Applies: syslog,
		},
		{
			Name: "LOG_SYSLOG_TIMEOUT", Group: "日志", Title: "Syslog 发送超时",
			Help:    "包含排队、建连、握手和发送的时间预算，大于 0，最多 30s。",
			Default: "500ms", Advanced: true, Applies: syslog,
			Need: func(v Values) (Level, string) {
				if v.Empty("LOG_SYSLOG_TIMEOUT") {
					return "", ""
				}
				d, err := time.ParseDuration(v["LOG_SYSLOG_TIMEOUT"])
				return when(err != nil || d <= 0 || d > 30*time.Second, required, "必须是 0 到 30s 之间的时长，例如 500ms")
			},
		},
		{
			Name: "LOG_SYSLOG_TLS_CA_FILE", Group: "日志", Title: "Syslog 私有 CA",
			Help:     "日志服务器用私有 CA 签发的证书时，填容器里能读到的 CA PEM 文件路径。默认用系统信任库。只在 LOG_SYSLOG_NETWORK=tls 时有效。",
			Advanced: true, Applies: tls,
		},
		{
			Name: "LOG_SYSLOG_TLS_SERVER_NAME", Group: "日志", Title: "Syslog 证书名称",
			Help:     "覆盖校验证书时用的服务器名称，默认用地址里的主机名。只在 LOG_SYSLOG_NETWORK=tls 时有效。",
			Advanced: true, Applies: tls,
		},
	}
}
