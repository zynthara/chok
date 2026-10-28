<p align="center">
  中文 | <a href="README.md">English</a>
</p>

<p align="center">
  <img src="https://img.shields.io/badge/Go-1.27%2B-00ADD8?logo=go&logoColor=white" alt="Go 1.27+" />
  <a href="LICENSE"><img src="https://img.shields.io/badge/license-MIT-green" alt="MIT" /></a>
  <a href="https://pkg.go.dev/github.com/zynthara/chok/v3"><img src="https://pkg.go.dev/badge/github.com/zynthara/chok/v3.svg" alt="Go Reference" /></a>
  <a href="https://github.com/zynthara/chok/actions/workflows/ci.yml"><img src="https://github.com/zynthara/chok/actions/workflows/ci.yml/badge.svg" alt="CI" /></a>
</p>

<h1 align="center"><code>chok</code></h1>
<p align="center"><b>一套有主见的 Go 应用工具包，用普通 Go 代码装配。</b></p>

---

> [!NOTE]
> 这里是 **chok v3**（`github.com/zynthara/chok/v3`），以 `v3.0.0-alpha.N`
> 发布：v3.0.0 之前 API 仍可能变化。v2 封版于
> [`v2` 分支](https://github.com/zynthara/chok/tree/v2)（只收安全修复；
> `go get github.com/zynthara/chok/v2@v2.0.0-beta.6` 永久可用）。从 v2 迁移请看
> [迁移指南](docs/migration-v2-to-v3.md)。

---

chok 给一个 Go 服务带来：一份严格的类型化配置、自动发布 OpenAPI 文档的类型化
HTTP handler、把每条查询围在当前登录用户之内的 gorm 存储——一个带认证的 CRUD
资源只要四个 chok import。身份、任务、遥测、缓存、实时推送、邮件是电池：你构造
出来、交给 `app.Use` 的值。没有内核、没有注册表、没有生成文件；`main.go` 就是全部装配：

```go
package main

import (
	"context"

	"github.com/zynthara/chok/v3"
	"github.com/zynthara/chok/v3/db"
	"github.com/zynthara/chok/v3/identity"
	"github.com/zynthara/chok/v3/store"
)

type Config struct {
	chok.Base                    // the app, http and log sections
	DB   db.Config       `yaml:"db"`
	Auth identity.Config `yaml:"auth"`
}

// Note belongs to whoever creates it: the store stamps the owner and
// fences every query to the signed-in user.
type Note struct {
	db.OwnedModel
	Text string `json:"text" store:"query,update" gorm:"size:500;not null"`
}

type newNote struct {
	Text string `json:"text" validate:"required,max=500"`
}

func main() {
	cfg := chok.MustLoad[Config]("notes") // chok.yaml, then NOTES_* variables
	app := chok.New(cfg.Base)
	d := db.MustOpen(cfg.DB, db.Tables(db.Table(&Note{})))
	auth := identity.MustNew(cfg.Auth, d) // /auth/register, /auth/login, ...
	app.Use(d, auth)

	notes := store.New[Note](d)
	api := app.Group("/api", auth.Authn())
	chok.POST(api, "/notes", func(ctx context.Context, req *newNote) (*Note, error) {
		n := &Note{Text: req.Text}
		return n, notes.Create(ctx, n)
	}, chok.Created())
	chok.List(api, "/notes", notes) // ?page=&size=&order=text:desc&text=
	app.Execute()
}
```

```yaml
# chok.yaml —— 拼错的键会让启动失败并给出候选
db:
  driver: sqlite
  sqlite:
    path: notes.db
auth:
  token_ttl: 2h # signing_key 来自环境变量 NOTES_AUTH_SIGNING_KEY
```

跑起来就有：带 request id 与访问日志的 JSON 服务、`/healthz` `/readyz`
`/livez`、`/openapi.json` 的 OpenAPI 3.1 文档与 `/docs` 的 UI、带限速的注册与
登录、401 / 404 / 409 / 400 错误信封、Ctrl-C 优雅停机。SQLite、MySQL、Postgres
同等支持；表在启动时自动建，或由版本化 SQL 迁移建。

## 30 秒 hello

```sh
go run github.com/zynthara/chok/v3/cmd/chok@latest init hello
cd hello && go mod tidy && go run .
```

另开一个终端：

```sh
curl -s localhost:8080/healthz
# {"status":"up","components":[{"component":"db","status":"up","duration_ms":0}]}
curl -s -X POST localhost:8080/api/v1/notes -H 'Content-Type: application/json' -d '{"title":"hello"}'
# {"id":"note_…","version":1,"created_at":"…","updated_at":"…","title":"hello","body":""}
curl -s localhost:8080/api/v1/notes
# {"items":[…],"total":1,"page":1,"size":20,"has_more":false}
```

`chok init` 写出 76 行的 `main.go`（一个资源、四个路由）、`chok.yaml`、
`Makefile`、`migrations/`，以及 require chok 并在 `tool` 指令里登记命令的
`go.mod`——于是 `go tool chok` 跑的就是项目所用的那个版本。打开
<http://localhost:8080/docs> 查看 API。

## 5 分钟 blog

[`examples/blog`](examples/blog) 是一个不到一百行的 JWT 博客：注册、登录、写
只有自己看得见的文章、在乐观锁下更新、删除、看 `/metrics`。

```sh
git clone https://github.com/zynthara/chok && cd chok/examples/blog
export BLOG_AUTH_SIGNING_KEY=$(openssl rand -hex 32)
go run .
```

```sh
curl -s -X POST localhost:8080/auth/register -H 'Content-Type: application/json' \
  -d '{"email":"alice@example.com","password":"password123","name":"Alice"}'
TOKEN=$(curl -s -X POST localhost:8080/auth/login -H 'Content-Type: application/json' \
  -d '{"email":"alice@example.com","password":"password123"}' |
  python3 -c 'import json,sys; print(json.load(sys.stdin)["token"])')
curl -s -X POST localhost:8080/api/v1/posts -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' -d '{"title":"Hello chok","content":"my first post"}'
# {"id":"pst_…","version":1,…,"title":"Hello chok","content":"my first post","status":"draft"}
curl -s "localhost:8080/api/v1/posts?status=draft" -H "Authorization: Bearer $TOKEN"
```

它的 [README](examples/blog/README.md) 走完全部九步——别的用户看到 404、过期
版本的更新 409、删除、OpenAPI 文档与指标——验收测试在 CI 里走同一条路径。想从
空目录一步步搭起来，读 [quickstart](docs/quickstart.md)。

## 实时推送

`realtime` 电池把事件即时推到浏览器——订单变了、聊天来了新消息、后台任务跑完了
——经 WebSocket 或 server-sent events，两者共用同一套主题。页面先用自己的 token
换一张一次性 ticket，长期 token 不进任何 URL；`Authorize` 决定谁能订什么；断线
重连的页面从断点续上；有 Redis 时所有副本共享事件。

```go
package main

import (
	"context"

	"github.com/zynthara/chok/v3"
	"github.com/zynthara/chok/v3/realtime"
)

type Config struct {
	chok.Base
	Realtime realtime.Config `yaml:"realtime"`
}

type story struct {
	Title string `json:"title" validate:"required,max=200"`
}

func main() {
	cfg := chok.MustLoad[Config]("news")
	app := chok.New(cfg.Base)
	// Anonymous tickets (no realtime.Authn), and anyone may follow "news".
	hub := realtime.MustNew(cfg.Realtime, realtime.Authorize(func(_ context.Context, topic string) error {
		if topic == "news" {
			return nil
		}
		return chok.ErrForbidden.Msgf("no topic %s", topic)
	}))
	app.Use(hub) // POST /realtime/ticket, GET /realtime/ws, GET /realtime/sse

	chok.POST(app.Router(), "/stories", func(ctx context.Context, s *story) (chok.None, error) {
		return chok.None{}, hub.Publish(ctx, "news", "story.posted", s)
	})
	app.Execute()
}
```

```js
// In the page: a one-time ticket, then the stream.
const {ticket} = await (await fetch("/realtime/ticket", {method: "POST"})).json();
const news = new EventSource(`/realtime/sse?topics=news&ticket=${ticket}`);
news.addEventListener("story.posted", (e) => console.log(JSON.parse(e.data).data.title));
```

重连与续传、WebSocket 协议、多副本、实时的后台任务面板：
[docs/realtime.md](docs/realtime.md)。[`examples/chatter`](examples/chatter)
是在它上面做的带机器人与任务面板的聊天室。

## 邮件与无密码登录

`mail` 电池只用标准库发事务邮件——经 SMTP 中继、开发时写成 `.eml` 文件、或者只记
日志——模板是 `html/template` 与 `text/template` 文件。有了发信的办法，`identity`
就能用验证码代替密码登录：`POST /auth/passwordless/start {"email"}` 一律回 202，
并寄出验证码（配了 `magic_link_url` 时还有一键登录链接）；
`POST /auth/passwordless/verify` 交上验证码（或链接里的 token）即登录，第一次登录时
建号。cookie 模式下会话是页面脚本碰不到的 HttpOnly
cookie，由默认开着的 `http.csrf` 把守。

```go
package main

import (
	"os"

	"github.com/zynthara/chok/v3"
	"github.com/zynthara/chok/v3/db"
	"github.com/zynthara/chok/v3/identity"
	"github.com/zynthara/chok/v3/mail"
)

type Config struct {
	chok.Base
	DB   db.Config       `yaml:"db"`
	Auth identity.Config `yaml:"auth"`
	Mail mail.Config     `yaml:"mail"`
}

func main() {
	cfg := chok.MustLoad[Config]("shop")
	app := chok.New(cfg.Base)
	d := db.MustOpen(cfg.DB)
	m := mail.MustNew(cfg.Mail, mail.Templates(os.DirFS("templates")))
	// A mail is the template named after its kind:
	// templates/login-code.html, templates/password-reset.html.
	auth := identity.MustNew(cfg.Auth, d, identity.Sender(m.SendTemplate))
	app.Use(d, auth) // + POST /auth/passwordless/start, /auth/passwordless/verify, /auth/logout
	app.Execute()
}
```

```yaml
# chok.yaml; the signing key comes from SHOP_AUTH_SIGNING_KEY
db:
  driver: sqlite
  sqlite:
    path: shop.db
auth:
  passwordless:
    enabled: true
  cookie:
    enabled: true
mail:
  from: Shop <no-reply@shop.example>
  smtp:
    host: smtp.example.com   # the password comes from SHOP_MAIL_SMTP_PASSWORD
    username: apikey
```

模板、附件、三种传输、经 jobs worker 重试、页面一侧的登录流程与 cookie：
[docs/mail.md](docs/mail.md)。[`examples/chatter`](examples/chatter) 就是这样登录的。

## 包

| 包 | 是什么 |
|---|---|
| [`chok`](https://pkg.go.dev/github.com/zynthara/chok/v3) | 配置装载、App 与生命周期、类型化 handler 与路由、绑定与校验、错误信封、默认中间件、OpenAPI、健康探针、slog |
| [`db`](https://pkg.go.dev/github.com/zynthara/chok/v3/db) | SQLite / MySQL / Postgres 句柄、事务、模型基座、RID、软唯一索引、单账本上的 auto / versioned / off 迁移 |
| [`store`](https://pkg.go.dev/github.com/zynthara/chok/v3/store) | 泛型 `Store[T]`：CRUD、失败关闭的 owner 隔离、带乐观锁的 Patch、游标、聚合 |
| [`store/where`](https://pkg.go.dev/github.com/zynthara/chok/v3/store/where) | 查询 DSL：`Eq`、`In`、`Like`、`Order`、`Page`、`Cursor`、`FromQuery` … |
| [`identity`](https://pkg.go.dev/github.com/zynthara/chok/v3/identity) | 账户、JWT、登录限速、OAuth、邮件验证码的无密码登录、cookie 会话、审计日志、`Authn()` |
| [`identity/rbac`](https://pkg.go.dev/github.com/zynthara/chok/v3/identity/rbac) | casbin 的基于角色的访问控制：`Require`、策略、Redis watcher |
| [`identity/google`](https://pkg.go.dev/github.com/zynthara/chok/v3/identity/google)、[`github`](https://pkg.go.dev/github.com/zynthara/chok/v3/identity/github)、[`apple`](https://pkg.go.dev/github.com/zynthara/chok/v3/identity/apple)、[`facebook`](https://pkg.go.dev/github.com/zynthara/chok/v3/identity/facebook) | OAuth provider |
| [`jobs`](https://pkg.go.dev/github.com/zynthara/chok/v3/jobs) | cron 调度的周期任务与事务性 outbox worker（至少一次） |
| [`telemetry`](https://pkg.go.dev/github.com/zynthara/chok/v3/telemetry) | Prometheus 指标、OpenTelemetry 链路、slog 到 OpenTelemetry Logs、SQL 埋点 |
| [`cache`](https://pkg.go.dev/github.com/zynthara/chok/v3/cache) | 一个 `Config` 后面的内存层与 Redis 层，带熔断器 |
| [`realtime`](https://pkg.go.dev/github.com/zynthara/chok/v3/realtime) | 经 WebSocket 与 server-sent events 推给浏览器的事件：一次性 ticket、授权的主题、断线续传、Redis backplane |
| [`mail`](https://pkg.go.dev/github.com/zynthara/chok/v3/mail) | 经 SMTP 中继、`.eml` 文件或日志发出的事务邮件：模板、附件、内嵌图片；identity 的登录验证码 |
| [`choktest`](https://pkg.go.dev/github.com/zynthara/chok/v3/choktest) | 测试辅助：按 lane 的方言给每个测试一个一次性数据库、进程内的 App 客户端 |

每个导出符号一行：[docs/api.md](docs/api.md)。

## 配置

一个 struct，由 `chok.Load[T]` 装入：默认值 → `chok.yaml` → 环境变量
（`http.addr` 是 `<NAME>_HTTP_ADDR`）→ flag。未知键让启动失败并给出候选；
secret 打码；`--check-config` 校验配置并打印、不启动任何东西；只有 `log.level`
经 SIGHUP 热更。每个键的类型、默认值与规则：[docs/config.md](docs/config.md)。

## 命令行

```sh
go install github.com/zynthara/chok/v3/cmd/chok@latest   # 或在项目里 go tool chok
```

| 命令 | 做什么 |
|---|---|
| `chok init <name>` | 一个 `go run .` 即起的项目 |
| `chok migrate create <name>` | 下一个版本化迁移的空 SQL 文件，每个方言一个 |
| `chok migrate up` / `status` / `mark-applied <version>` | 按应用自己的 `db` 配置，在 `chok_migrations` 账本上应用、列出、登记迁移 |
| `chok openapi export <url> [--out file]` | 把运行中应用的 OpenAPI 文档存成 JSON 或 YAML |
| `chok version` | 本命令构建自的 chok 版本、修订与工具链 |

## 设计三句话

配置是严格的：一个类型化 struct，未知键启动即失败，只有日志级别能热更。错误描述
自己：实现 `chok.Coder` 的错误就是 HTTP 信封，没有映射注册表。装配就是代码：依赖
是传给构造函数的值，`app.Use` 只登记生命周期。

详见 [docs/design.md](docs/design.md)。

## 文档

| 主题 | 位置 |
|---|---|
| Quickstart：从空目录到带认证的资源 | [docs/quickstart.md](docs/quickstart.md) |
| 实时推送：五分钟在浏览器里看到事件 | [docs/realtime.md](docs/realtime.md) |
| 邮件、无密码登录与 cookie 会话 | [docs/mail.md](docs/mail.md) |
| 配置参考（生成） | [docs/config.md](docs/config.md) |
| API 地图（生成） | [docs/api.md](docs/api.md) |
| 设计文档 | [docs/design.md](docs/design.md) |
| 从 v2 迁移 | [docs/migration-v2-to-v3.md](docs/migration-v2-to-v3.md) |
| 各版本的变更 | [CHANGELOG.md](CHANGELOG.md) |
| GoDoc | <https://pkg.go.dev/github.com/zynthara/chok/v3> |
| 给编码代理的约定 | [CLAUDE.md](CLAUDE.md)、[AGENTS.md](AGENTS.md) |

## 参与开发

`make verify` 按 CI 的顺序跑完所有闸门：tidy、gofmt、vet、golangci-lint、
`go fix`、SQLite / Postgres / MySQL 三条 lane 的测试、API 预算、`examples/blog`
的冒烟测试。数据库 lane 读 `CHOK_TEST_PG_DSN` 与 `CHOK_TEST_MYSQL_DSN`；约定见
[CLAUDE.md](CLAUDE.md)。安全问题请按 [SECURITY.md](SECURITY.md) 报告。

## 许可

[MIT License](LICENSE)。`jobs/internal/cron` 的 cron 解析器改编自 robfig/cron
（MIT），见 [NOTICE](NOTICE)。
