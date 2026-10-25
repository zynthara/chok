# chatter — chok's realtime example

Chat rooms that update without a reload, a bot that answers through a
background worker, and a live panel of the background jobs: identity,
store, jobs and the realtime hub in one app of about 230 lines of Go and
one HTML page with plain JavaScript (no build step). `main.go` assembles
it; `chat.go` is the rooms and messages; `bot.go` the worker, a periodic
job and the bridge from the jobs to the hub.

How the pieces meet:

- A message is written in a transaction; `d.AfterCommit` publishes it to
  the topic `room:<id>`, so nobody sees a message that rolled back.
- A message that mentions `@bot` also `Enqueue`s the bot's turn in the
  same transaction; the `bot.reply` worker writes the answer and
  publishes it in turn.
- `jobs.Observe` hands every run of the jobs to the hub, on the topic
  `jobs`: the batteries do not import each other, the app's closure joins
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

1. Open <http://localhost:8080> and register **alice**. Create the room
   `lobby`. The jobs panel on the right starts listening, and every 30
   seconds the `stats` job reports in.
2. In a private window (another session), open the same address, register
   **bob**, and open `lobby` from the room list: opening a room joins it.
3. Write in either window: the other shows the message at once.
4. Write `hello @bot`: the bot's answer arrives a moment later, written by
   the `bot.reply` worker, and the jobs panel shows the worker's run
   (`worker bot.reply finished …, 1 message(s)`).
5. Stop the server (Ctrl-C) and start it again: both pages reconnect by
   themselves — with a fresh ticket — and catch up: the chat resumes after
   the last message it saw, or reloads the room when the server no longer
   has that far back (a new process starts with an empty replay buffer).

### With curl

The same, from a terminal. A browser's WebSocket and EventSource cannot
send an `Authorization` header, so a client first trades its token for a
one-time ticket:

```sh
# 1. Two users; keep their tokens.
ALICE=$(curl -s -X POST localhost:8080/auth/register -H 'Content-Type: application/json' \
  -d '{"email":"alice@example.com","password":"password123","name":"alice"}' |
  python3 -c 'import json,sys; print(json.load(sys.stdin)["token"])')
BOB=$(curl -s -X POST localhost:8080/auth/register -H 'Content-Type: application/json' \
  -d '{"email":"bob@example.com","password":"password123","name":"bob"}' |
  python3 -c 'import json,sys; print(json.load(sys.stdin)["token"])')

# 2. alice opens a room, bob joins it.
ROOM=$(curl -s -X POST localhost:8080/api/rooms -H "Authorization: Bearer $ALICE" \
  -H 'Content-Type: application/json' -d '{"name":"lobby"}' |
  python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')
curl -s -X POST localhost:8080/api/rooms/$ROOM/join -H "Authorization: Bearer $BOB"

# 3. bob follows the room: a ticket, then the stream (it stays open).
TICKET=$(curl -s -X POST localhost:8080/realtime/ticket -H "Authorization: Bearer $BOB" |
  python3 -c 'import json,sys; print(json.load(sys.stdin)["ticket"])')
curl -sN "localhost:8080/realtime/sse?topics=room:$ROOM&ticket=$TICKET"
# retry: 3000
```

In a second terminal, with the same variables:

```sh
# 4. alice calls the bot.
curl -s -X POST localhost:8080/api/rooms/$ROOM/messages -H "Authorization: Bearer $ALICE" \
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
ones included.

The acceptance test, `chatter_test.go`, walks this path — HTTP, a
WebSocket and an event stream against the real server — on SQLite,
Postgres and MySQL.

## Several replicas

This app keeps tickets and the replay buffer in its process: run one
replica, or pin each browser to one. With Redis, every replica shares
both: pass the client to the hub — `realtime.Redis(client)`, for instance
`cache.Redis()` — and see [docs/realtime.md](../../docs/realtime.md).
