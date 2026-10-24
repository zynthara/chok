// Package jobs is chok's battery for background work: periodic jobs on
// cron schedules, and workers that deliver, at least once and in order,
// the messages the application enqueues in its own transactions.
//
//	j := jobs.MustNew(cfg.Jobs, d,
//		jobs.Periodic("purge-sessions", "@hourly", purgeSessions, jobs.OverlapSkip),
//		jobs.Worker("order.placed", sendConfirmation),
//	)
//	app.Use(d, j)
//
//	// In a handler, in the same transaction as the writes:
//	err := d.RunInTx(ctx, func(txCtx context.Context) error {
//		if err := orders.Create(txCtx, o); err != nil {
//			return err
//		}
//		return j.Enqueue(txCtx, "order.placed", OrderPlaced{OrderID: o.RID})
//	})
//
// # Periodic, or Worker and Enqueue?
//
// Ask what starts the work.
//
//   - The clock: a nightly report, purging expired rows, polling an
//     upstream API, anything that runs on a schedule whatever the
//     application did in between. Use Periodic. Each run gets a context
//     that ends at shutdown; Overlap decides what a trigger does while the
//     previous run is still going.
//   - A write: the work follows from a change the code just committed — an
//     order was placed, so send the confirmation; a user signed up, so
//     provision their space — and must happen for every such change, even
//     if the process dies right after the commit. Use a Worker for the
//     topic, and Enqueue in the transaction that makes the change: the
//     message commits or rolls back with it, so there is never a sent
//     confirmation for an order that rolled back, nor an order whose
//     confirmation was lost.
//   - A write, but losing the work in a crash is acceptable (refreshing a
//     cache, a best-effort notification): neither. Do it in the request,
//     or in a goroutine started from db.AfterCommit.
//
// # Delivery
//
// A worker sweeps its topic every PollInterval and hands each committed
// message to its function in (created_at, id) order. Delivery is at least
// once: a message the function failed on is retried on the next sweep,
// and the messages younger than the settle window are delivered again
// after a restart. So the function must be idempotent — dedupe on
// Message.ID, or on a business key in the payload. A failing message
// blocks the messages behind it in its topic until it succeeds (nothing
// is skipped); the other topics' workers keep their own pace.
//
// Each worker keeps its position, a watermark, in chok_jobs_watermarks.
// The watermark never passes messages younger than SettleWindow: a
// transaction that enqueued earlier but commits later is still found. The
// guarantee holds while every enqueuing transaction commits within
// SettleWindow of its Enqueue and the processes that enqueue keep their
// clocks in sync. With Retention set, messages every worker has passed are
// deleted once older than Retention.
//
// Workers and periodic jobs run in the processes where Config.Enabled is
// true; run them in one process of a deployment and set enabled: false in
// the others, which can still Enqueue.
package jobs
