package chok

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/zynthara/chok/log"
)

type signalEvent int

const (
	signalShutdown signalEvent = iota // SIGTERM / SIGINT
	signalQuit                        // SIGQUIT
)

// signalWatcher listens for OS signals and returns two channels:
//   - lifecycle: shutdown/quit events (buffer 1, non-blocking send)
//   - reload: SIGHUP notifications (buffer 1, non-blocking send)
//
// Separating the channels ensures SIGHUP never blocks SIGTERM delivery.
// Reload uses non-blocking send to an unbuffered channel: the send only
// succeeds when the main loop is actively selecting, so SIGHUPs arriving
// during a synchronous reload are automatically dropped and logged.
//
// Neither channel is closed on exit — see comment on nil-channel safety.
func signalWatcher(ctx context.Context, logger log.Logger) (<-chan signalEvent, <-chan struct{}) {
	lcCh := make(chan signalEvent, 1)
	rlCh := make(chan struct{})
	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP, syscall.SIGQUIT)

	go func() {
		defer signal.Stop(sigCh)
		for {
			select {
			case <-ctx.Done():
				return
			case sig := <-sigCh:
				switch sig {
				case syscall.SIGTERM, syscall.SIGINT:
					select {
					case lcCh <- signalShutdown:
					default:
					}
				case syscall.SIGQUIT:
					select {
					case lcCh <- signalQuit:
					default:
					}
				case syscall.SIGHUP:
					select {
					case rlCh <- struct{}{}:
					default:
						logger.Warn("SIGHUP ignored: previous reload still in progress")
					}
				}
			}
		}
	}()
	return lcCh, rlCh
}
