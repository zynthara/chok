package chok

import (
	"context"
	"crypto/sha256"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/zynthara/chok/log"
)

// configFileWatcher watches the configuration file at path and emits a
// notification on the returned channel whenever the file is written or
// (re)created. Used by App.Run when WithConfigWatch() is enabled.
//
// Implementation notes:
//
//   - Watches the parent *directory* rather than the file path, because
//     common editors (vim, VS Code "atomic save") replace the file via
//     rename, which fsnotify delivers as Remove+Create — watching the
//     directory catches both the Write and the rename-create path.
//   - Events are debounced: rapid bursts (e.g. multiple fsync calls
//     during a save) collapse into one notification. Debounce window
//     is 100ms, short enough to feel immediate, long enough to absorb
//     most editor write patterns.
//   - The send is non-blocking: if a previous reload is still in
//     flight, additional events drop silently. The user's main loop
//     decides how to handle bursts.
//   - Errors from fsnotify are logged at Warn level but never close
//     the channel, so a transient inotify overflow won't permanently
//     kill the watcher.
//
// The watcher is torn down when ctx is Done — the goroutine exits and
// the fsnotify.Watcher is Close'd. Callers can also safely call the
// context's cancel to stop watching at any time.
func configFileWatcher(ctx context.Context, path string, logger log.Logger) <-chan struct{} {
	if path == "" {
		return nil
	}
	out := make(chan struct{}, 1)

	w, err := fsnotify.NewWatcher()
	if err != nil {
		logger.Warn("config watch: cannot create fsnotify watcher", "error", err)
		return nil
	}

	dir := filepath.Dir(path)
	target := filepath.Clean(path)

	if err := w.Add(dir); err != nil {
		logger.Warn("config watch: cannot watch directory", "dir", dir, "error", err)
		w.Close()
		return nil
	}

	go func() {
		defer w.Close()

		var (
			mu       sync.Mutex
			timer    *time.Timer
			lastHash [32]byte
			haveHash bool
		)
		trigger := func() {
			// In k8s ConfigMap deployments, ..data symlinks rotate on
			// every update of any key in the mount — including keys this
			// app doesn't care about. Before emitting, re-read the target
			// file and compare its content hash to the last known one;
			// if nothing changed, suppress the signal. Reduces reload
			// churn in side-car-heavy environments.
			//
			// timer.Stop() does NOT cancel an already-fired callback that
			// is still executing. If a fast burst of events triggers
			// schedule() while a previous callback is mid-flight, both
			// would race on lastHash/haveHash without this lock. The mu
			// Lock here is the same instance schedule() uses, so the two
			// timer iterations serialise their hash reads/writes too.
			mu.Lock()
			changed := didTargetContentChange(target, &lastHash, &haveHash)
			mu.Unlock()
			if !changed {
				return
			}
			select {
			case out <- struct{}{}:
			default:
				// previous reload still pending; drop
			}
		}
		schedule := func() {
			mu.Lock()
			defer mu.Unlock()
			if timer != nil {
				timer.Stop()
			}
			timer = time.AfterFunc(100*time.Millisecond, trigger)
		}

		// Seed the hash so the first event compares against the on-disk
		// state rather than a zero hash (which would cause the first
		// event to always fire even if the file is untouched). Held under
		// mu for symmetry with the trigger path even though no other
		// goroutine can race here yet.
		mu.Lock()
		_ = didTargetContentChange(target, &lastHash, &haveHash)
		mu.Unlock()

		for {
			select {
			case <-ctx.Done():
				mu.Lock()
				if timer != nil {
					timer.Stop()
				}
				mu.Unlock()
				return
			case ev, ok := <-w.Events:
				if !ok {
					return
				}
				// Only react to events targeting our file. Watching the
				// directory surfaces events for siblings too (dotfiles,
				// editor backup files). Comparison uses filepath.Clean
				// so "./cfg.yaml" and "cfg.yaml" match.
				//
				// Also accept events on "..data" — k8s ConfigMap atomic
				// updates rename a `..data` symlink in the mount dir to
				// switch configurations without touching individual file
				// names. The user's configured path (`.../config.yaml`)
				// is itself a symlink into the swapped tree, so the raw
				// event we see is on `..data`, not the target filename.
				name := filepath.Clean(ev.Name)
				base := filepath.Base(name)
				targetHit := name == target || base == "..data"
				if !targetHit {
					continue
				}
				if ev.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Rename) != 0 {
					schedule()
				}
			case err, ok := <-w.Errors:
				if !ok {
					return
				}
				logger.Warn("config watch: fsnotify error", "error", err)
			}
		}
	}()

	return out
}

// didTargetContentChange compares the sha256 of target against the last
// observed hash. Returns true when the content changed (or the file could
// not be read, in which case we conservatively let the caller reload so a
// rotated/replaced config doesn't silently stick with the old hash).
// Updates *last and *have in place.
//
// Used by configFileWatcher to suppress spurious reloads triggered by k8s
// ConfigMap `..data` rotations when the watched config file itself was
// not modified.
func didTargetContentChange(target string, last *[32]byte, have *bool) bool {
	f, err := os.Open(target)
	if err != nil {
		// Target is missing / unreadable. Reload anyway — the loader will
		// surface the error clearly, and we don't want to mask a real
		// removal by suppressing the signal.
		*have = false
		return true
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		*have = false
		return true
	}
	var next [32]byte
	copy(next[:], h.Sum(nil))
	if *have && *last == next {
		return false
	}
	*last = next
	*have = true
	return true
}
