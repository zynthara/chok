package cache

import (
	"fmt"

	"github.com/zynthara/chok/log"
)

// badgerLogger adapts chok's log.Logger to badger's Logger interface.
type badgerLogger struct {
	l log.Logger
}

func (b *badgerLogger) Errorf(f string, v ...interface{})   { b.l.Error(fmt.Sprintf(f, v...)) }
func (b *badgerLogger) Warningf(f string, v ...interface{}) { b.l.Warn(fmt.Sprintf(f, v...)) }
func (b *badgerLogger) Infof(f string, v ...interface{})    { b.l.Info(fmt.Sprintf(f, v...)) }
func (b *badgerLogger) Debugf(f string, v ...interface{})   { b.l.Debug(fmt.Sprintf(f, v...)) }
