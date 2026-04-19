package log

import "context"

// Empty returns a no-op Logger (for testing).
func Empty() Logger { return empty{} }

type empty struct{}

func (empty) Debug(string, ...any)                         {}
func (empty) Info(string, ...any)                          {}
func (empty) Warn(string, ...any)                          {}
func (empty) Error(string, ...any)                         {}
func (empty) DebugContext(context.Context, string, ...any) {}
func (empty) InfoContext(context.Context, string, ...any)  {}
func (empty) WarnContext(context.Context, string, ...any)  {}
func (empty) ErrorContext(context.Context, string, ...any) {}
func (e empty) With(...any) Logger                         { return e }
func (empty) SetLevel(string) error                        { return nil }
