# chatter — chok's realtime example

Chat rooms that update without a reload, a bot that answers through a
background worker, and a live panel of the background jobs, behind a
sign-in by mailed code: identity, mail, store, jobs and the realtime hub
in one app of about 240 lines of Go, two mail templates and one HTML
page with plain JavaScript (no build step). `main.go` assembles it;
`chat.go` is the rooms and messages; `bot.go` the worker, a periodic job
and the bridge from the jobs to the hub.

How the pieces meet:

- Signing in takes an address and the code mailed to it: identity's
  passwordless routes ask the app's `MailSender` — a few lines in
  `main.go` — for a mail, which the mail battery renders from
  `templates/login-code.html` and `.txt`. Here `chok.yaml` sets the mail
  transport to `dir`: every mail becomes an `.eml` file in
  `/tmp/chatter-mail` instead of leaving the machine. A first sign-in
  creates the account, and `auth.passwordless.only` leaves the code the
  only way in: there is no password to register, log in with or reset.
- The session is an HttpOnly cookie (`auth.cookie`, with `only`): the page
  never holds a token, its `fetch` calls carry the cookie by themselves,
  and `http.csrf` refuses cross-origin writes.
- A message is written in a transaction; `d.AfterCommit` publishes it to
  the topic `room:<id>`, so nobody sees a message that rolled back.
- A message that mentions `@bot` also `Enqueue`s the bot's turn in the
  same transaction; the `bot.reply` worker writes the answer and
  publishes it in turn.
- `jobs.Observe` hands every run of the jobs to the hub, on the topic
  `jobs`: the batteries do not import each other, the app's closures join
  them.
- `realtime.Authorize` lets a room's members follow its topic, and
  everyone signed in follow `jobs`; the page's chat uses a WebSocket, its
  jobs panel an EventSource.

## The five-minute path

Requires Go 1.27. From the repository root:

```sh
cd examples/chatter
export CHATTER_AUTH_SIGNING_KEY=$(openssl rand -hex 32)   # ≥ 32 bytes, never in chok.yaml
go run .
```

### In the browser

1. Open <http://localhost:8080>, enter `alice@example.com` and press
   **Mail me a code**. The mail is a file now — the server logs
   `mail: written path=/tmp/chatter-mail/….eml` — and its subject holds
   the code:

   ```sh
   grep -h '^Subject:' /tmp/chatter-mail/*.eml | tail -1
   # Subject: Your chatter code is 482913
   ```

   Type it in and you are signed in, as alice: the first sign-in made the
   account. (Opening the `.eml` in a mail app shows the HTML version, whose
   link signs in too.)
2. Create the room `lobby`. The jobs panel on the right starts listening,
   and every 30 seconds the `stats` job reports in.
3. In a private window (another cookie jar), sign in as
   `bob@example.com` the same way and open `lobby` from the room list:
   opening a room joins it.
4. Write in either window: the other shows the message at once.
5. Write `hello @bot`: the bot's answer arrives a moment later, written by
   the `bot.reply` worker, and the jobs panel shows the worker's run
   (`worker bot.reply finished …, 1 message(s)`).
6. Stop the server (Ctrl-C) and start it again: both pages reconnect by
   themselves — with a fresh ticket — and catch up: the chat resumes after
   the last message it saw, or reloads the room when the server no longer
   has that far back (a new process starts with an empty replay buffer).
   The sessions survive the restart: they are signed cookies, not server
   state. **log out** clears the cookie.

### With curl

The same, from a terminal, with a cookie jar per person. A browser's
WebSocket and EventSource cannot send a header, so a client first trades
its session for a one-time ticket:

```sh
# 1. alice and bob each ask for a code, read it from their mail and sign
#    in; the session cookie lands in their jar.
signin() {
  curl -s -X POST localhost:8080/auth/passwordless/start -H 'Content-Type: application/json' \
    -d "{\"email\":\"$1@example.com\"}"
  sleep 1   # the mail goes out in the background
  CODE=$(grep -l "^To: <$1@example.com>" /tmp/chatter-mail/*.eml | tail -1 |
    xargs sed -n 's/^Subject: Your chatter code is //p' | tr -d '\r')
  curl -s -c $1.jar -X POST localhost:8080/auth/passwordless/verify -H 'Content-Type: application/json' \
    -d "{\"email\":\"$1@example.com\",\"code\":\"$CODE\"}"
}
signin alice   # {"expires_at":"…"}: with auth.cookie.only the token is in the cookie alone
signin bob

# 2. alice opens a room, bob joins it.
ROOM=$(curl -s -b alice.jar -X POST localhost:8080/api/rooms \
  -H 'Content-Type: application/json' -d '{"name":"lobby"}' |
  python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')
curl -s -b bob.jar -X POST localhost:8080/api/rooms/$ROOM/join

# 3. bob follows the room: a ticket, then the stream (it stays open).
TICKET=$(curl -s -b bob.jar -X POST localhost:8080/realtime/ticket |
  python3 -c 'import json,sys; print(json.load(sys.stdin)["ticket"])')
curl -sN "localhost:8080/realtime/sse?topics=room:$ROOM&ticket=$TICKET"
# retry: 3000
```

In a second terminal, with the same variables:

```sh
# 4. alice calls the bot.
curl -s -b alice.jar -X POST localhost:8080/api/rooms/$ROOM/messages \
  -H 'Content-Type: application/json' -d '{"text":"hello @bot, how are you"}'
```

and bob's stream shows both messages, alice's at once and the bot's
answer after it (ids and times differ):

```text
id: 4f1c2a9be0d3-1
event: message.created
data: {"id":"4f1c2a9be0d3-1","topic":"room:room_…","type":"message.created","data":{"id":"msg_…",…,"author":"alice","text":"hello @bot, how are you","reply_to":""},"at":"…"}

id: 4f1c2a9be0d3-2
event: message.created
data: {"id":"4f1c2a9be0d3-2","topic":"room:room_…","type":"message.created","data":{"id":"msg_…",…,"author":"bot","text":"alice, that was 5 words. Noted.","reply_to":"msg_…"},"at":"…"}
```

The same ticket does not open a second stream (`401`), and a user who has
not joined the room is refused it: with only that topic, the stream
answers `403` (`join the room first`); among other topics, it is a
`refused` event and the rest stream on. `GET /realtime/sse?topics=jobs&…`
is the jobs panel; `/openapi.json` lists every route, the three realtime
ones included, with the session cookie as `cookieAuth`.
`curl -s -b alice.jar -c alice.jar -X POST localhost:8080/auth/logout`
clears alice's cookie.

The acceptance test, `chatter_test.go`, walks this path — sign-in by
mailed code and by the mail's link, HTTP, a WebSocket and an event stream
against the real server — on SQLite, Postgres and MySQL.

## Real mail

`transport: dir` is for development. To send, point the mail section at
a relay — the password through `CHATTER_MAIL_SMTP_PASSWORD` — and serve
the page over https, which lets `auth.cookie.secure` take its default:

```yaml
mail:
  from: chatter <no-reply@chat.example.com>
  transport: smtp
  smtp: { host: smtp.example.com, port: 587, username: apikey }
```

[docs/mail.md](../../docs/mail.md) has the rest: templates, retries
through a jobs worker, the cookie and CSRF.

## Several replicas

This app keeps tickets and the replay buffer in its process: run one
replica, or pin each browser to one. With Redis, every replica shares
both: pass the client to the hub — `realtime.Redis(client)`, for instance
`cache.Redis()` — and see [docs/realtime.md](../../docs/realtime.md).
