package telemetry

import (
	"context"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel"

	"github.com/zynthara/chok"
	"github.com/zynthara/chok/internal/dbx"
)

func TestInstrument_PublishesInitializedObserver(t *testing.T) {
	tel := newTel(t, metricsOn())
	d := openNotes(t, time.Nanosecond)
	original := dbx.Observe
	defer func() { dbx.Observe = original }()
	// Force a real statement in the documented in-use attachment window.
	// The hook only pauses Instrument after its atomic publish.
	published, release := make(chan struct{}), make(chan struct{})
	dbx.Observe = func(d any, o dbx.Observer) dbx.Observed {
		desc := original(d, o)
		close(published)
		<-release
		return desc
	}
	var wg sync.WaitGroup
	wg.Go(func() { tel.Instrument(d) })
	<-published
	if err := d.Unsafe(t.Context()).Find(&[]note{}).Error; err != nil {
		t.Fatal(err)
	}
	close(release)
	wg.Wait()
	value, ok := metric(t, tel.Registry(), "db_client_operation_duration_seconds", map[string]string{"db_system_name": systemName(dbx.Describe(d).Driver), "db_operation_name": "SELECT", "db_collection_name": "notes", "error_type": ""})
	if !ok || value != 1 {
		t.Fatalf("uninitialized metric absent %v %v", value, ok)
	}

}

func TestInstrument_RecordsTransactionPhases(t *testing.T) {
	tel, spans := tracing(t, metricsOn())
	d := openNotes(t, 0)
	tel.Instrument(d)
	if err := d.RunInTx(t.Context(), func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for _, s := range spans.GetSpans() {
		found[s.Name] = true
	}
	if !found["BEGIN"] || !found["COMMIT"] {
		t.Fatalf("transaction spans: %v", found)
	}
}
func TestLogs_BridgesAreScopedToApp(t *testing.T) {
	base := testBase(t)
	base.Log.Output = nil
	a, b := chok.New(base), chok.New(base)
	t.Cleanup(func() {
		// t.Context() is canceled before cleanups run: close with a live one.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
		defer cancel()
		_ = a.Close(ctx)
		_ = b.Close(ctx)
	})
	cfg := Config{}
	cfg.Logs.OTel = true
	la, lb := &memLogs{}, &memLogs{}
	ta, tb := newTel(t, cfg, logExporterOption(la)), newTel(t, cfg, logExporterOption(lb))
	a.Use(ta)
	b.Use(tb)
	a.Logger().Info("only-a")
	b.Logger().Info("only-b")
	if len(la.records()) != 1 || la.records()[0].Body().AsString() != "only-a" || len(lb.records()) != 1 || lb.records()[0].Body().AsString() != "only-b" {
		t.Fatal("App log bridges crossed")
	}
}

func TestTracing_DefaultInstancesLeaveProcessProviderAlone(t *testing.T) {
	before := otel.GetTracerProvider()
	first, _ := tracing(t, Config{})
	second, spans := tracing(t, Config{})
	if otel.GetTracerProvider() != before {
		t.Fatal("default construction replaced the process provider")
	}
	if err := first.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	_, span := second.TracerProvider().Tracer("isolation").Start(t.Context(), "second survives")
	span.End()
	if len(spans.GetSpans()) != 1 || otel.GetTracerProvider() != before {
		t.Fatal("closing one instance affected another")
	}
}
