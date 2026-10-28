package jobs

import (
	"embed"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"io/fs"
	"time"

	"github.com/zynthara/chok/v3/db"
	"github.com/zynthara/chok/v3/internal/dbx"
)

// Message is an enqueued message as a worker receives it, its payload
// decoded into T.
type Message[T any] struct {
	// ID numbers the message in chok_jobs_messages. A redelivery carries
	// the same ID, so a worker may dedupe on it.
	ID uint64
	// Topic is the topic it was enqueued under: the worker's.
	Topic string
	// Payload is the value given to Enqueue, decoded from its JSON with
	// encoding/json/v2.
	Payload T
	// CreatedAt is when Enqueue ran, in UTC.
	CreatedAt time.Time
	// Attempt is which delivery of the message this is, from 1: the
	// failed deliveries before it, plus one. A redelivery after a crash
	// repeats the number of the delivery it repeats.
	Attempt int
}

// rawMessage is a message as the relay hands it on, its payload the JSON
// as stored: what Worker[T] decodes.
type rawMessage = Message[jsontext.Value]

// DeadMessage is a message parked in the dead letters, chok_jobs_dead:
// its worker failed on it MaxAttempts times, or could not decode its
// payload. Jobs.Requeue enqueues it again, Jobs.Discard drops it.
type DeadMessage struct {
	// ID is the parked message's ID.
	ID    uint64 `json:"id"`
	Topic string `json:"topic"`
	// Payload is the message's JSON as Enqueue stored it.
	Payload jsontext.Value `json:"payload"`
	// CreatedAt is when the message was enqueued, in UTC.
	CreatedAt time.Time `json:"created_at"`
	// ParkedAt is when it was parked, in UTC.
	ParkedAt time.Time `json:"parked_at"`
	// Attempts counts its failed deliveries.
	Attempts int `json:"attempts"`
	// LastError is the error of the last one; "decode: ..." when the
	// payload did not decode into the worker's type.
	LastError string `json:"last_error"`
}

// maxTopicLen bounds a topic: the topic columns are varchar(200).
const maxTopicLen = 200

// errBadTopic is an empty topic or one longer than maxTopicLen.
var errBadTopic = errors.New("a topic must be 1 to 200 bytes")

func checkTopic(topic string) error {
	if topic == "" || len(topic) > maxTopicLen {
		return fmt.Errorf("%w, got %d", errBadTopic, len(topic))
	}
	return nil
}

// message is a row of chok_jobs_messages: written by Enqueue in the
// caller's transaction, read by the workers, deleted only by the retention
// cleanup. (created_at, id) is the delivery order. One index serves a
// worker's scan of its topic, the other the scans across topics: the
// settled-frontier probe and the cleanup. The worker writes the last
// three columns, and only when a delivery fails: head-of-line delivery
// means only the head of a topic can be failing, and the count travels
// with the message, so a restart picks it up.
type message struct {
	ID        uint64    `gorm:"primaryKey;index:idx_chok_jobs_messages_scan,priority:2;index:idx_chok_jobs_messages_topic,priority:3"`
	CreatedAt time.Time `gorm:"not null;index:idx_chok_jobs_messages_scan,priority:1;index:idx_chok_jobs_messages_topic,priority:2"`
	Topic     string    `gorm:"size:200;not null;index:idx_chok_jobs_messages_topic,priority:1"`
	Payload   []byte
	// Attempts counts the failed deliveries; RetryAt is when the next
	// may start (NULL: at once); LastError is the last failure's error.
	Attempts  int `gorm:"not null;default:0"`
	RetryAt   *time.Time
	LastError string
}

func (message) TableName() string { return "chok_jobs_messages" }

func (m *message) toMessage() rawMessage {
	return rawMessage{
		ID: m.ID, Topic: m.Topic, Payload: jsontext.Value(m.Payload),
		CreatedAt: m.CreatedAt.UTC(), Attempt: m.Attempts + 1,
	}
}

// watermarkRow is a worker's persisted position in chok_jobs_watermarks,
// keyed by its topic: (watermark_at, watermark_id) is the (created_at, id)
// of the last settled message whose whole prefix, in delivery order, is
// delivered. A row appears with the first advance, so watermark_at is
// never the zero time, which MySQL would refuse.
type watermarkRow struct {
	Topic       string    `gorm:"primaryKey;size:200"`
	WatermarkAt time.Time `gorm:"not null"`
	WatermarkID uint64    `gorm:"not null"`
	UpdatedAt   time.Time `gorm:"not null"`
}

func (watermarkRow) TableName() string { return "chok_jobs_watermarks" }

// deadRow is a row of chok_jobs_dead: a parked message under its own ID,
// written when its worker parks it, deleted only by Requeue and Discard.
type deadRow struct {
	ID        uint64 `gorm:"primaryKey;autoIncrement:false;index:idx_chok_jobs_dead_parked,priority:2;index:idx_chok_jobs_dead_topic,priority:3"`
	Topic     string `gorm:"size:200;not null;index:idx_chok_jobs_dead_topic,priority:1"`
	Payload   []byte
	CreatedAt time.Time `gorm:"not null"`
	ParkedAt  time.Time `gorm:"not null;index:idx_chok_jobs_dead_parked,priority:1;index:idx_chok_jobs_dead_topic,priority:2"`
	Attempts  int       `gorm:"not null"`
	LastError string
}

func (deadRow) TableName() string { return "chok_jobs_dead" }

func (r *deadRow) toDead() DeadMessage {
	return DeadMessage{
		ID: r.ID, Topic: r.Topic, Payload: jsontext.Value(r.Payload), CreatedAt: r.CreatedAt.UTC(),
		ParkedAt: r.ParkedAt.UTC(), Attempts: r.Attempts, LastError: r.LastError,
	}
}

// deliveredRow is a row of chok_jobs_delivered: a message a worker
// delivered (or parked) that its watermark had not passed yet when the
// jobs stopped. Close writes them; the worker's first sweep after the
// restart reads its topic's rows back into its memory and deletes them,
// so a graceful restart does not deliver them again.
type deliveredRow struct {
	Topic     string    `gorm:"primaryKey;size:200"`
	MessageID uint64    `gorm:"primaryKey;autoIncrement:false"`
	CreatedAt time.Time `gorm:"not null"`
}

func (deliveredRow) TableName() string { return "chok_jobs_delivered" }

// tables are the jobs schema: plain tables, without a model base.
func tables() []db.TableSpec {
	return []db.TableSpec{
		dbx.PlainTable(&message{}).(db.TableSpec),
		dbx.PlainTable(&watermarkRow{}).(db.TableSpec),
		dbx.PlainTable(&deadRow{}).(db.TableSpec),
		dbx.PlainTable(&deliveredRow{}).(db.TableSpec),
	}
}

// migrationFiles are the jobs' SQL migrations for migrate: versioned,
// under migrations/{sqlite,mysql,postgres}: the four tables as migrate:
// auto creates them.
//
//go:embed migrations
var migrationFiles embed.FS

// migrations are the SQL files of migrate: versioned, one directory per
// dialect.
func migrations() fs.FS {
	sub, err := fs.Sub(migrationFiles, "migrations")
	if err != nil {
		panic(err) // the embedded tree is fixed at build time
	}
	return sub
}

// The chok command reports and applies the jobs' migrations through dbx,
// for an application that runs with migrate: off.
func init() { dbx.Migrations["jobs"] = migrations() }
