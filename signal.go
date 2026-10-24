package chok

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
)

var errSignal = errors.New("chok: stop signal")

// Execute runs the App under OS signals and exits 1 if it fails:
// SIGINT and SIGTERM stop it gracefully, SIGQUIT stops it fast (drain
// skipped, the stop budget capped at 5s), SIGHUP re-reads the
// configuration file and applies log.level, logging that any other
// change needs a restart.
func (a *App) Execute() {
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	sig := make(chan os.Signal, 4)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT, syscall.SIGHUP)
	defer signal.Stop(sig)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case s := <-sig:
				switch s {
				case syscall.SIGHUP:
					a.reloadLogLevel()
				case syscall.SIGQUIT:
					a.logger.Info("chok: SIGQUIT received, stopping fast")
					cancel(errFastStop)
				default:
					a.logger.Info("chok: signal received, stopping", "signal", s.String())
					cancel(errSignal)
				}
			}
		}
	}()
	if err := a.Run(ctx); err != nil {
		exit(1)
	}
}

// reloadLogLevel is the SIGHUP handler.
func (a *App) reloadLogLevel() {
	if a.level == nil {
		a.logger.Warn("chok: SIGHUP ignored: the logger was injected, its level is not the App's to change")
		return
	}
	if a.reload == nil {
		a.logger.Warn("chok: SIGHUP ignored: this App's configuration did not come from chok.Load")
		return
	}
	level, othersChanged, err := a.reload.reread()
	if err != nil {
		a.logger.Error("chok: SIGHUP: configuration reload failed; keeping the running configuration", "error", err)
		return
	}
	old := a.level.Level()
	lv, ok := parseLevel(level)
	if !ok {
		lv = old
	}
	// Announce while the more verbose of the two levels is in force, at a
	// level it lets through, so neither line is filtered by the change.
	lo := min(old, lv)
	ctx := context.Background()
	announce := func() {
		if lv != old {
			a.logger.Log(ctx, max(slog.LevelInfo, lo), "chok: SIGHUP: log level changed", "from", old.String(), "to", lv.String())
		}
		if othersChanged {
			a.logger.Log(ctx, max(slog.LevelWarn, lo), "chok: SIGHUP: configuration changes other than log.level require a restart")
		}
	}
	if lv < old {
		a.level.Set(lv)
		announce()
		return
	}
	announce()
	a.level.Set(lv)
}
