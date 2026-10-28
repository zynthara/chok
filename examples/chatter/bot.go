package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/zynthara/chok/v3/jobs"
	"github.com/zynthara/chok/v3/store"
	"github.com/zynthara/chok/v3/store/where"
)

// botReply is the worker of bot.reply: it answers the message in the
// payload and pushes the answer to the room. A worker gets a message at
// least once, so an answer already written is not written again.
func (c *chat) botReply(ctx context.Context, msg jobs.Message[Message]) error {
	m := msg.Payload
	if answered, err := c.messages.Exists(ctx, store.Where(where.Eq("reply_to", m.RID))); answered || err != nil {
		return err
	}
	words := len(strings.Fields(m.Text))
	reply := &Message{RoomID: m.RoomID, UserID: "bot", Author: "bot", ReplyTo: m.RID,
		Text: fmt.Sprintf("%s, that was %d words. Noted.", m.Author, words)}
	if err := c.messages.Create(ctx, reply); err != nil {
		return err
	}
	c.publish(ctx, reply)
	return nil
}

// stats is a periodic job: the totals, for the jobs panel.
func (c *chat) stats(ctx context.Context) error {
	rooms, err := c.rooms.Count(ctx)
	if err != nil {
		return err
	}
	messages, err := c.messages.Count(ctx)
	if err != nil {
		return err
	}
	return c.hub.Publish(ctx, "jobs", "stats", map[string]int64{"rooms": rooms, "messages": messages})
}

// observe hands every run of the jobs to the panel's topic, and every
// message the bot worker gives up on, skipping the worker's sweeps that
// found nothing to do (one per poll_interval).
func (c *chat) observe(ev jobs.Event) {
	if ev.Type == "worker" && ev.Kind != "run.failed" && ev.Kind != "message.parked" && ev.Messages == 0 {
		return
	}
	_ = c.hub.Publish(context.Background(), "jobs", ev.Kind, ev)
}
