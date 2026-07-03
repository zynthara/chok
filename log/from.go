package log

import "github.com/zynthara/chok/v2/kernel"

// From returns the app's root logger as the rich log.Logger — the
// business-side companion of db.From for Routes callbacks:
//
//	posts := store.New[model.Post](db.From(k), log.From(k), ...)
//
// The kernel hands out its minimal kernel.Logger contract; the
// concrete root logger behind it is always a log.Logger (the App
// builds or receives one). A kernel wired with something poorer —
// possible only in hand-rolled test harnesses — yields Empty()
// rather than a nil that would panic at first use.
func From(k kernel.Kernel) Logger {
	if l, ok := k.Logger().(Logger); ok && l != nil {
		return l
	}
	return Empty()
}
