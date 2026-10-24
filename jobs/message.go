package jobs

import (
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"time"

	"github.com/zynthara/chok/v3/db"
	"github.com/zynthara/chok/v3/internal/dbx"
)

// Message is an enqueued message as a worker receives it.
type Message struct {
	// ID numbers the message in chok_jobs_messages. A redelivery carries
	// the same ID, so a worker may dedupe on it.
	ID uint64
	// Topic is the topic it was enqueued under: the worker's.
	Topic string
	// Payload is the JSON that encoding/json/v2 made of the value given
	// to Enqueue; decode it with json.Unmarshal.
	Payload []byte
	// CreatedAt is when Enqueue ran, in UTC.
	CreatedAt time.Time
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

// message is a row of chok_jobs_messages: written once, by Enqueue in the
// caller's transaction, read by the workers, deleted only by the retention
// cleanup. (created_at, id) is the delivery order. One index serves a
// worker's scan of its topic, the other the scans across topics: the
// settled-frontier probe and the cleanup.
type message struct {
	ID        uint64    `gorm:"primaryKey;index:idx_chok_jobs_messages_scan,priority:2;index:idx_chok_jobs_messages_topic,priority:3"`
	CreatedAt time.Time `gorm:"not null;index:idx_chok_jobs_messages_scan,priority:1;index:idx_chok_jobs_messages_topic,priority:2"`
	Topic     string    `gorm:"size:200;not null;index:idx_chok_jobs_messages_topic,priority:1"`
	Payload   []byte
}

func (message) TableName() string { return "chok_jobs_messages" }

func (m *message) toMessage() Message {
	return Message{ID: m.ID, Topic: m.Topic, Payload: m.Payload, CreatedAt: m.CreatedAt.UTC()}
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

// tables are the jobs schema: plain tables, without a model base.
func tables() []db.TableSpec {
	return []db.TableSpec{
		dbx.PlainTable(&message{}).(db.TableSpec),
		dbx.PlainTable(&watermarkRow{}).(db.TableSpec),
	}
}

// migrationFiles are the jobs' SQL migrations for migrate: versioned,
// under migrations/{sqlite,mysql,postgres}: the two tables as migrate:
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
