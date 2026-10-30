package main

import (
	"context"
	"slices"
	"strings"

	"github.com/zynthara/chok"
	"github.com/zynthara/chok/db"
	"github.com/zynthara/chok/jobs"
	"github.com/zynthara/chok/realtime"
	"github.com/zynthara/chok/store"
	"github.com/zynthara/chok/store/where"
)

// Room is a chat room: every signed-in user sees the list and may join.
type Room struct {
	db.Model
	Name      string `json:"name"       store:"query" gorm:"size:100;not null"`
	CreatedBy string `json:"created_by" gorm:"size:64;not null"`
}

// RIDPrefix exposes rooms as room_xxx.
func (Room) RIDPrefix() string { return "room" }

// Member records that a user joined a room: only members read and post
// there, and only they may follow its topic.
type Member struct {
	db.Model
	RoomID string `json:"room_id" store:"query" gorm:"size:64;not null;uniqueIndex:uk_member_room_user"`
	UserID string `json:"user_id" store:"query" gorm:"size:64;not null;uniqueIndex:uk_member_room_user"`
}

// Message is one line of a room. A bot reply names the message it
// answers in ReplyTo.
type Message struct {
	db.Model
	RoomID  string `json:"room_id"  store:"query" gorm:"size:64;not null;index"`
	UserID  string `json:"user_id"  gorm:"size:64;not null"`
	Author  string `json:"author"   gorm:"size:100;not null"`
	Text    string `json:"text"     gorm:"size:2000;not null"`
	ReplyTo string `json:"reply_to" store:"query" gorm:"size:64;not null"`
}

// RIDPrefix exposes messages as msg_xxx.
func (Message) RIDPrefix() string { return "msg" }

// tables are the app's tables, for db.Open and choktest.DB alike.
func tables() []db.TableSpec {
	return []db.TableSpec{db.Table(&Room{}), db.Table(&Member{}), db.Table(&Message{})}
}

// chat holds what the handlers, the bot and the hub's rules share.
type chat struct {
	d        *db.DB
	rooms    *store.Store[Room]
	members  *store.Store[Member]
	messages *store.Store[Message]
	hub      *realtime.Hub
	jobs     *jobs.Jobs
}

var errNotMember = chok.ErrForbidden.Msgf("join the room first")

type newRoom struct {
	Name string `json:"name" validate:"required,max=100"`
}

// createRoom makes a room whose first member is its creator.
func (c *chat) createRoom(ctx context.Context, req *newRoom) (*Room, error) {
	p, _ := chok.PrincipalFrom(ctx)
	r := &Room{Name: req.Name, CreatedBy: p.UserID}
	err := c.d.RunInTx(ctx, func(tx context.Context) error {
		if err := c.rooms.Create(tx, r); err != nil {
			return err
		}
		return c.members.Create(tx, &Member{RoomID: r.RID, UserID: p.UserID})
	})
	return r, err
}

type roomID struct {
	ID string `path:"id" validate:"required"`
}

// join makes the caller a member of the room; joining again is a no-op.
func (c *chat) join(ctx context.Context, req *roomID) (chok.None, error) {
	if _, err := c.rooms.Get(ctx, store.RID(req.ID)); err != nil {
		return chok.None{}, err
	}
	p, _ := chok.PrincipalFrom(ctx)
	if member, err := c.isMember(ctx, req.ID, p.UserID); member || err != nil {
		return chok.None{}, err
	}
	return chok.None{}, c.members.Create(ctx, &Member{RoomID: req.ID, UserID: p.UserID})
}

// history is the room's last 50 messages, oldest first, for its members:
// what a page shows before the live ones, and again after a reset.
func (c *chat) history(ctx context.Context, req *roomID) ([]Message, error) {
	if err := c.mustBeMember(ctx, req.ID); err != nil {
		return nil, err
	}
	page, err := c.messages.List(ctx, where.Eq("room_id", req.ID), where.Order("created_at", true), where.Limit(50))
	if err != nil {
		return nil, err
	}
	slices.Reverse(page.Items)
	return page.Items, nil
}

type newMessage struct {
	ID   string `path:"id" validate:"required"`
	Text string `json:"text" validate:"required,max=2000"`
}

// post writes a message and, once the transaction commits, pushes it to
// the room's topic. A message that mentions @bot also enqueues, in the
// same transaction, the bot's turn: the reply comes from a worker.
func (c *chat) post(ctx context.Context, req *newMessage) (*Message, error) {
	if err := c.mustBeMember(ctx, req.ID); err != nil {
		return nil, err
	}
	p, _ := chok.PrincipalFrom(ctx)
	m := &Message{RoomID: req.ID, UserID: p.UserID, Author: nameOf(p), Text: req.Text}
	err := c.d.RunInTx(ctx, func(tx context.Context) error {
		if err := c.messages.Create(tx, m); err != nil {
			return err
		}
		if strings.Contains(m.Text, "@bot") {
			if err := c.jobs.Enqueue(tx, "bot.reply", m); err != nil {
				return err
			}
		}
		// A rollback drops this: nobody sees a message that was not saved.
		c.d.AfterCommit(tx, func(ctx context.Context) { c.publish(ctx, m) })
		return nil
	})
	return m, err
}

// publish pushes a new message to the browsers that follow its room.
func (c *chat) publish(ctx context.Context, m *Message) {
	if err := c.hub.Publish(ctx, "room:"+m.RoomID, "message.created", m); err != nil {
		chok.LoggerFrom(ctx).WarnContext(ctx, "chatter: publish a message", "error", err)
	}
}

// mayWatch is the hub's Authorize: a room's topic is for its members, the
// jobs panel for everyone signed in — every ticket here is, Authn sees to
// it.
func (c *chat) mayWatch(ctx context.Context, topic string) error {
	if topic == "jobs" {
		return nil
	}
	if id, ok := strings.CutPrefix(topic, "room:"); ok {
		return c.mustBeMember(ctx, id)
	}
	return chok.ErrForbidden.Msgf("no topic %s here", topic)
}

func (c *chat) mustBeMember(ctx context.Context, roomID string) error {
	p, _ := chok.PrincipalFrom(ctx)
	member, err := c.isMember(ctx, roomID, p.UserID)
	if err == nil && !member {
		err = errNotMember
	}
	return err
}

func (c *chat) isMember(ctx context.Context, roomID, userID string) (bool, error) {
	return c.members.Exists(ctx, store.Where(where.Eq("room_id", roomID), where.Eq("user_id", userID)))
}

// nameOf is how a user signs messages: the account's name or, for an
// account made by its first sign-in, which has none, the address's local
// part.
func nameOf(p chok.Principal) string {
	if name := p.Extra["name"]; name != "" {
		return name
	}
	name, _, _ := strings.Cut(p.Email, "@")
	return name
}
