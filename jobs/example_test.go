package jobs_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/zynthara/chok/v3/db"
	"github.com/zynthara/chok/v3/jobs"
)

type orderPlaced struct {
	OrderID string `json:"order_id"`
}

// A worker per topic, fed by Enqueue in the transaction that makes the
// change; and a periodic job on a cron schedule. In an application,
// app.Use(d, j) starts and stops them; here Start runs until cancel.
func Example() {
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	d := db.MustOpen(db.Config{Driver: "sqlite", SQLite: db.SQLiteConfig{Path: ":memory:"}}, db.Logger(quiet))
	defer func() { _ = d.Close(context.Background()) }()

	delivered := make(chan string)
	j := jobs.MustNew(jobs.Config{Enabled: true, PollInterval: 10 * time.Millisecond}, d,
		// The payload arrives decoded. A message can come twice, with the
		// same ID: keep the effect idempotent.
		jobs.Worker("order.placed", func(ctx context.Context, m jobs.Message[orderPlaced]) error {
			delivered <- m.Payload.OrderID
			return nil
		}),
		jobs.Periodic("purge-carts", "@hourly", func(ctx context.Context) error { return nil }, jobs.OverlapSkip),
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stopped := make(chan error, 1)
	go func() { stopped <- j.Start(ctx) }()

	err := d.RunInTx(ctx, func(txCtx context.Context) error {
		// ... the order's own writes, with txCtx ...
		return j.Enqueue(txCtx, "order.placed", orderPlaced{OrderID: "ord_1"})
	})
	if err != nil {
		fmt.Println(err)
		return
	}
	fmt.Println("confirm", <-delivered)

	// Outside a transaction, Enqueue refuses: the message would not
	// commit or roll back with anything.
	err = j.Enqueue(ctx, "order.placed", orderPlaced{OrderID: "ord_2"})
	fmt.Println(errors.Is(err, jobs.ErrOutsideTx))

	cancel()
	fmt.Println("stopped:", <-stopped)
	// Output:
	// confirm ord_1
	// true
	// stopped: <nil>
}
