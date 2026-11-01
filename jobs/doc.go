// Package jobs is chok's battery for background work: periodic jobs on
// cron schedules, and workers that deliver, at least once,
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
// A worker sweeps its topic every PollInterval and hands committed messages
// to its function in (created_at, id) scan order, decoded into the worker's
// type. Late commits can arrive after newer messages: there is no total
// enqueue or commit order. Delivery is at least once. The durable
// acknowledgement is written AFTER the handler succeeds; a crash or write
// failure between an external effect and acknowledgement can repeat the
// effect. Handlers must dedupe by Message.ID or a business idempotency key.
// Successful acknowledgements survive crashes and graceful restarts.
//
// A failing message blocks the messages behind it in its topic (they keep
// their order); the other topics' workers keep their own pace. The worker
// tries it again after RetryBackoff, doubling the wait up to MaxBackoff,
// and counts the failures on the message's row, so a restart goes on
// from the count. The MaxAttempts-th failure parks the message in the
// dead letters, chok_jobs_dead, and the topic moves on; a payload that
// does not decode is parked at once. Dead lists the parked messages,
// Requeue enqueues them again once the cause is fixed, Discard drops
// them — from any process, the jobs enabled or not.
//
// Each worker keeps a watermark in chok_jobs_watermarks to accelerate
// scanning. Pending rows behind it are always revisited, so arbitrarily
// late commits are found without a settle deadline or synchronized clocks.
// CompletedAt on the original message is the correctness boundary, also
// for parking, Requeue and Discard. Retention only deletes acknowledged
// messages of registered topics below the watermark floor; unknown topics
// stay. Dead letters remain until Requeue or Discard.
//
// Attempts count persisted handler failures, not effects or deliveries.
// Actual cancellation errors do not consume an attempt. A success or a
// real error racing shutdown is still recorded, with a separate bounded
// context, when the DB permits: a stop that begins while the function
// runs does not repeat a message it finished, and the function gets no
// further message until the next start.
//
// Workers and periodic jobs run in the processes where Config.Enabled is
// true; run them in one process of a deployment and set enabled: false in
// the others, which can still Enqueue.
//
// # Watching the runs
//
// Stats is a snapshot; Observe is the stream. Its function receives an
// Event as each run starts, finishes or fails, as a trigger is skipped
// and as a worker parks a message, so a dashboard can show the background
// work live — hand the
// events to the realtime battery, in the application, since batteries do
// not import each other:
//
//	jobs.Observe(func(ev jobs.Event) {
//		_ = hub.Publish(context.Background(), "jobs", ev.Kind, ev)
//	})
package jobs
