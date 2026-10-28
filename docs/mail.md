# Mail and passwordless sign-in

The `mail` battery sends the mail a web application owes its users — a
sign-in code, a password reset, an order confirmation — with one value
and the standard library only: SMTP through a relay (Resend, Postmark,
Amazon SES and the like all offer one, and sign DKIM for you), `.eml`
files for development, or the log. `identity` uses it to sign people in
with a mailed code instead of a password, and can keep their session in
an HttpOnly cookie instead of handing the page a token.

This page goes from a first mail to a passwordless, cookie-based
sign-in. The complete example app is [`examples/chatter`](../examples/chatter),
whose page signs in this way. Every key is in
[config.md](config.md#mailconfig--section-mail), every symbol in
[api.md](api.md#mail).

## A mailer, and identity's mails

A mailer is a value you construct; `app.Use` is optional, since it has
no lifecycle — it only shows up on `/healthz`. This app signs people in
with codes and sends them through templates named after identity's
kinds of mail:

```go
package main

import (
	"context"
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
	// Each mail identity asks for is the template named after its kind,
	// templates/login-code.html or templates/password-reset.html,
	// rendered with the identity.Mail.
	auth := identity.MustNew(cfg.Auth, d, identity.Sender(m.SendTemplate))
	app.Use(d, auth) // /auth/passwordless/start and /verify, /auth/logout, ...

	chok.GET(app.Group("/api", auth.Authn()), "/hello", func(ctx context.Context, _ *chok.None) (string, error) {
		p, _ := chok.PrincipalFrom(ctx)
		return "hello " + p.Email, nil
	})
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
  reset_url: https://shop.example/reset          # Mail.Link of a password reset
  passwordless:
    enabled: true
    magic_link_url: https://shop.example/sign-in # Mail.Link of a sign-in code
  cookie:
    enabled: true
mail:
  from: Shop <no-reply@shop.example>
  transport: dir              # development: each mail an .eml file in dir
  dir: mail-out
```

`identity.Sender` takes a function of the address, the template's name
and its data — `m.SendTemplate` is one, `mail.Message{To: []string{to},
Template: template, Data: data}` sent. identity names the template after
the mail's `Kind` (`login-code` or `password-reset`) and gives the
`identity.Mail` as the data: `{{.Code}}`, `{{.Token}}`, `{{.Link}}` (set
when `magic_link_url` or `reset_url` is) and `{{.ExpiresAt}}`. identity
calls it in the background — a response never waits on a mail, so it
takes as long whether or not the address has an account — logs a
failure as a warning, and waits for the mails in flight when it closes.

## Templates

`mail.Templates(fsys)` reads every `.html` and `.txt` file of a file
system, at any depth, and names a template after its file:
`templates/login-code.html` is `login-code`. An `embed.FS`
(`//go:embed templates`) builds them into the binary; `os.DirFS`, above,
reads them from disk.

```html
{{define "subject"}}Your sign-in code is {{.Code}}{{end}}
<p>Your code: <b>{{.Code}}</b>. It works once, until {{.ExpiresAt.Format "15:04 MST"}}.</p>
{{if .Link}}<p>Or <a href="{{.Link}}">sign in with this link</a>.</p>{{end}}
```

- A template is `{name}.html`, an `html/template` sent as `text/html`;
  `{name}.txt`, a `text/template` sent as `text/plain`; or both, which go
  out together as `multipart/alternative`.
- `{{define "subject"}}…{{end}}` is the subject (the `.txt` one when both
  define it); a non-empty `Message.Subject` replaces it.
- Files whose name starts with `_` are partials: every `.html` template
  can call the `_*.html` ones, every `.txt` the `_*.txt` ones — a footer, a
  layout with `{{template "content" .}}` blocks.
- Every template parses when the mailer is built: a broken one fails
  startup, not the first mail. Two files with one name fail it too, and
  so does a file system with partials alone.

## Sending one

```go fragment
err := m.Send(ctx, mail.Message{
	To:       []string{"Ada Lovelace <ada@example.com>"},
	Bcc:      []string{"orders@shop.example"}, // on the envelope, never in the mail
	Template: "order-confirmed",
	Data:     order,
	Headers:  map[string]string{"List-Unsubscribe": "<https://shop.example/unsubscribe?u=42>"},
	Attachments: []mail.Attachment{
		{Name: "logo.png", Content: logo, Inline: true}, // <img src="cid:logo.png">
		{Name: "invoice-42.pdf", Content: invoice},     // application/pdf, from the extension
	},
})
```

Instead of `Template`, a message may carry `Text`, `HTML` or both. The
mailer writes `From` (and `Reply-To`) from the configuration, encodes a
subject or a name outside ASCII, dates the mail, gives it a
`Message-ID` at the sender's domain and nests the parts as mail clients
expect: text and HTML in `multipart/alternative`, inside
`multipart/related` with inline images, inside `multipart/mixed` with the
attachments. `Headers` adds fields of your own; the mailer's — `From`,
`To`, `Cc`, `Bcc`, `Subject`, `Date`, `Message-ID`, `MIME-Version`,
`Content-*` — cannot be set there. A bad address, a missing template or a
template given with a body is an error before anything is sent.

## Three transports

| `mail.transport` | What a Send does | For |
|---|---|---|
| `smtp` (default) | Dials `smtp.host:port`, `EHLO`, upgrades with `STARTTLS` (`smtp.tls: starttls`, port 587) or speaks TLS from the start (`implicit`, port 465), `AUTH PLAIN` with `smtp.username` and `smtp.password`, one `RCPT` per recipient — Bcc included — `DATA`, `QUIT`; all within `mail.timeout` (15s) | production |
| `dir` | Writes `{unix nanoseconds}-{random}.eml` into `mail.dir`, readable by its owner only, and logs the path | development, examples, tests |
| `log` | Logs the recipients, the subject and the size at Info, the body at Debug | development without a disk |

Each Send dials anew: there is no pool to keep healthy, so `Health` is
always nil. `smtp.tls: none` sends in the clear and only a loopback host
may use it. A relay's password is a secret: set it through the
environment (`SHOP_MAIL_SMTP_PASSWORD`), never in the file.

```yaml
mail:
  from: Shop <no-reply@shop.example>
  smtp:
    host: smtp.resend.com
    port: 465
    tls: implicit
    username: resend
```

`mail.Sender(fn)` replaces the transport with a function that gets the
message as rendered and its RFC 5322 text: an HTTP API instead of SMTP,
or a test that keeps what was sent.

```go fragment
m := mail.MustNew(cfg.Mail, mail.Templates(templates), mail.Sender(func(ctx context.Context, msg *mail.Message, raw []byte) error {
	return provider.Send(ctx, msg.To, msg.Subject, msg.HTML, msg.Text) // your API client
}))
```

## Retries through a jobs worker

A Send is one attempt. When a mail must arrive — and a relay can be down
— enqueue it in the transaction that calls for it and send it from a
`jobs` worker, which retries until the send succeeds:

```go fragment
type outgoing struct {
	To       string
	Template string
	Order    Order
}

j := jobs.MustNew(cfg.Jobs, d, jobs.Worker("mail.send", func(ctx context.Context, msg jobs.Message) error {
	var o outgoing
	if err := json.Unmarshal(msg.Payload, &o); err != nil {
		return err
	}
	return m.Send(ctx, mail.Message{To: []string{o.To}, Template: o.Template, Data: o.Order})
}))

// In the handler: the order and its mail commit together.
err := d.RunInTx(ctx, func(tx context.Context) error {
	if err := orders.Create(tx, order); err != nil {
		return err
	}
	return j.Enqueue(tx, "mail.send", outgoing{To: order.Email, Template: "order-confirmed", Order: *order})
})
```

The worker delivers at least once: after a crash between the relay's
answer and the worker's bookkeeping, a mail can go out twice. While a
message fails, the ones behind it on its topic wait, so give mail a topic
of its own, and check the addresses before you enqueue (a request's
`validate:"email"` does): then the worker's errors are the relay's, which
a retry can fix. identity's mails can take the same road — the `Sender`
function enqueues instead of sending, in a transaction of its own
(`d.RunInTx(ctx, …)` around `Enqueue`), and the worker sends — but the
payload then keeps a sign-in code or a reset token in the jobs table, in
the clear, until `jobs.retention` removes it, where identity itself only
stores HMACs. For those mails one attempt is usually the better trade: a
user who gets no code asks for another.

## Passwordless sign-in

With `auth.passwordless.enabled` (and a `Sender`), two public routes sign
people in with a code mailed to them:

- `POST /auth/passwordless/start` `{"email"}` answers **202** without a
  body, whatever the address: the mail goes out in the background, so
  neither the answer nor its timing tells whether the address has an
  account. The mail carries a code of `code_length` digits (6) and a
  magic-link token, both good for `code_ttl` (10m). While a code is
  unused, another start within `resend_after` (60s) of it changes nothing
  and mails nothing; each client address gets `login_rate_limit` starts
  per `login_rate_window` (429 after that).
- `POST /auth/passwordless/verify` `{"email", "code"}` or `{"token"}`
  answers what login does: `{"token", "expires_at"}` (and the cookie, in
  cookie mode). A wrong code is `401 InvalidCode` with
  `metadata.attempts_left`; a code that expired, was used, was tried
  `max_attempts` times (5) or was never asked for is `401 CodeExpired`:
  ask for a new one. The first sign-in of an unknown address creates its
  account — email verified, no password — unless `auto_register: false`.

For an application whose accounts are their addresses, `only: true` (with
`enabled`) leaves the code the only way in: register, login,
forgot-password, reset-password and change-password are not mounted at
all, while refresh-token, me, logout and OAuth stay, and
`Users().Create` and `ResetPassword` still work for operators.

The codes are kept as HMACs, never as they are, in the table
`auth_login_codes`; an hourly sweep deletes the expired ones. In the
page, the flow is two small forms:

```js
let email = "";
async function post(path, body) {
  const res = await fetch(path, {method: "POST", headers: {"Content-Type": "application/json"}, body: JSON.stringify(body)});
  if (!res.ok) throw new Error((await res.json()).message);
}
// 1. The address: always 202, then show the code form.
async function askCode(address) {
  email = address;
  await post("/auth/passwordless/start", {email});
}
// 2. The code from the mail; in cookie mode the session cookie is set.
async function enterCode(code) {
  await post("/auth/passwordless/verify", {email, code});
}
// 3. The mail's link opens the sign-in page with ?token=: good once, so
//    drop it from the address bar, then trade it.
const token = new URLSearchParams(location.search).get("token");
if (token) {
  history.replaceState(null, "", location.pathname);
  post("/auth/passwordless/verify", {token}).then(showApp, showError);
}
```

Without `magic_link_url` the mail has no link and the code is the only
way in. `auth.reset_url` does for password resets what `magic_link_url`
does for codes: the reset page takes `?token=` and posts it with the new
password to `/auth/reset-password`.

## Cookie mode and CSRF

With `auth.cookie.enabled`, every route that issues a token —
`register`, `login`, `refresh-token`, `passwordless/verify` and the OAuth
`exchange` — also sets it in a cookie: `chok_token` by default,
`HttpOnly`, `Secure` (set `secure: false` only for plain http on
localhost), `SameSite=Lax`, for `token_ttl`. `identity.Authn` reads the
cookie from a request without an `Authorization` header (a header, when
there is one, decides), and `POST /auth/logout` clears it with a 204. A
logout does not revoke the token — identity keeps no sessions — so a
leaked token stays good until it expires; `Users().RevokeTokens` ends all
of a user's tokens at once. With `cookie.only`, the bodies leave the
token out (`{"expires_at"}` alone): the page's scripts never see it.

```yaml
auth:
  cookie:
    enabled: true
    only: true        # the token lives in the cookie alone
    # same_site: lax  # the default; strict, or none (which needs secure)
```

A browser sends the cookie with every request to your API, wherever the
request comes from, so a cookie session needs protection from cross-site
requests. `http.csrf` gives it and is on by default (Go's
`CrossOriginProtection`: a cross-origin `POST`, `PUT`, `PATCH` or
`DELETE` from a browser is a 403; curl and mobile apps send none of the
headers it reads and pass); switch it off while routes take the cookie
and the app logs a warning when it starts. Serve the page from the API's
origin. `SameSite=Lax` keeps the cookie off other sites' `POST`s as well.
A front end on another origin goes in `http.cors.allow_origins`, which
`http.csrf` trusts, with `http.cors.allow_credentials: true`, and fetches
with `credentials: "include"`; on another site altogether it also needs
`same_site: none`. The OAuth callbacks are exempt: Apple posts its
callback from its own site, and the `state` parameter defends it.

In the page, `fetch` sends the cookie on its own (same origin needs no
`credentials` option), and nothing is stored in `localStorage`:

```js
const me = await fetch("/auth/me");            // 200, or 401: show the sign-in form
await fetch("/auth/logout", {method: "POST"}); // the cookie is gone
```

`realtime`'s ticket endpoint sits behind the same `Authn`, so in cookie
mode the page gets its ticket with a plain
`fetch("/realtime/ticket", {method: "POST"})`. The OpenAPI document lists
the cookie as the security scheme `cookieAuth` next to `bearerAuth`.

## Reference

| Route (under `auth.prefix`) | When | Answers |
|---|---|---|
| `POST /passwordless/start` `{email}` | `passwordless.enabled` | 202; 429 over the client's limit |
| `POST /passwordless/verify` `{email, code}` or `{token}` | `passwordless.enabled` | 200 `{token, expires_at}`; 401 `InvalidCode` / `CodeExpired` / disabled; 429 |
| `POST /logout` | `cookie.enabled`; bearer or cookie | 204, the cookie cleared |
| `POST /forgot-password` `{email}` | a `Sender`, without `passwordless.only` | 204; `Mail{Kind: "password-reset"}` when the account exists |

The `mail`, `auth.passwordless` and `auth.cookie` keys, with their
defaults, are in [config.md](config.md).
