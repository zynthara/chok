package store

import (
	"context"
	"reflect"
	"sort"

	"github.com/zynthara/chok/v2/kernel/event"
)

// Op names the write that produced an EntityChanged event.
type Op string

// Write operations carried by EntityChanged.Op. Upsert and BatchUpsert publish
// OpUpsert without Object: supported SQL dialects do not expose a portable way
// to tell insert from conflict-update or return the truthful persisted row
// identity. BatchUpsert publishes one such event per call, not per input.
// Subscribers should treat it as type-wide invalidation. Restore publishes
// OpRestore with the locator: subscribers tracking row liveness (caches,
// projections) treat it as the inverse of OpDelete rather than a field update.
const (
	OpCreate  Op = "create"
	OpUpdate  Op = "update"
	OpDelete  Op = "delete"
	OpRestore Op = "restore"
	OpUpsert  Op = "upsert"
)

// EntityChanged is the typed event a WithBus store publishes after a
// successful write. Which fields are set depends on the operation,
// mirroring the information v1's after-hooks received:
//
//	OpCreate — Object.Value() (a recursive snapshot; each read returns a copy)
//	OpUpdate — Locator + Changes
//	OpDelete — Locator
//	OpRestore — Locator
//	OpUpsert — no payload; invalidate this entity type and re-read through a
//	           domain key when needed
//
// Subscribe by concrete entity type:
//
//	event.Subscribe(bus, func(ctx context.Context, ev store.EntityChanged[model.Post]) {
//	    ...
//	})
//
// Publication is anchored to transaction commit (see WithBus): events
// from rolled-back writes never fire.
type EntityChanged[T any] struct {
	Op      Op
	Object  ObjectSnapshot[T]
	Locator LocatorSnapshot
	Changes ChangeSnapshot
}

// ObjectSnapshot is the immutable create payload carried by EntityChanged.
// Value returns a fresh recursive copy for each caller, so asynchronous
// subscribers cannot mutate shared event state.
type ObjectSnapshot[T any] struct {
	value *T
}

// Empty reports whether the event carries no object payload.
func (o ObjectSnapshot[T]) Empty() bool { return o.value == nil }

// Value returns a recursive copy of the created object, or nil when absent.
func (o ObjectSnapshot[T]) Value() *T {
	if o.value == nil {
		return nil
	}
	return cloneAny(o.value).(*T)
}

// ChangeSnapshot is the immutable update payload carried by EntityChanged
// and handed to before-update hooks (WithBeforeUpdate). Keys are the public
// update-field names used by the caller, not database columns. Its accessors
// return recursive copies so one subscriber or hook cannot race or corrupt
// another's view.
type ChangeSnapshot struct {
	values map[string]any
}

func newChangeSnapshot(values map[string]any) ChangeSnapshot {
	if len(values) == 0 {
		return ChangeSnapshot{}
	}
	return ChangeSnapshot{values: cloneMap(values)}
}

// Empty reports whether the update carried no field payload.
func (c ChangeSnapshot) Empty() bool { return len(c.values) == 0 }

// Fields returns the sorted public field names changed by the update.
func (c ChangeSnapshot) Fields() []string {
	fields := make([]string, 0, len(c.values))
	for field := range c.values {
		fields = append(fields, field)
	}
	sort.Strings(fields)
	return fields
}

// Values returns a recursive copy of all public field values.
func (c ChangeSnapshot) Values() map[string]any { return cloneMap(c.values) }

// Value returns a recursive copy of one public field value.
func (c ChangeSnapshot) Value(field string) (any, bool) {
	value, ok := c.values[field]
	if !ok {
		return nil, false
	}
	return cloneAny(value), true
}

// publishChanged emits ev on the Store's bus, if any. Inside a
// transaction the event is staged on the after-commit buffer
// (stageOnCommit carries the ownership rules) so rollbacks drop it —
// a foreign rollback must never drop the event of a committed write.
// Outside any transaction it publishes immediately.
func (s *Store[T]) publishChanged(ctx context.Context, ev EntityChanged[T]) {
	if s.bus == nil {
		return
	}
	publish := func(c context.Context) { event.Publish(c, s.bus, ev) }
	if s.stageOnCommit(ctx, publish) {
		return
	}
	publish(ctx)
}

// createdEvent builds the OpCreate event with a recursive snapshot so
// asynchronous subscribers never share caller-owned mutable descendants.
func createdEvent[T any](obj *T) EntityChanged[T] {
	cp := cloneAny(obj).(*T)
	return EntityChanged[T]{Op: OpCreate, Object: ObjectSnapshot[T]{value: cp}}
}

func upsertEvent[T any]() EntityChanged[T] {
	return EntityChanged[T]{Op: OpUpsert}
}

type cloneVisit struct {
	typ reflect.Type
	ptr uintptr
	len int
	cap int
}

func cloneMap(src map[string]any) map[string]any {
	if src == nil {
		return nil
	}
	cloned := cloneAny(src)
	if cloned == nil {
		return nil
	}
	return cloned.(map[string]any)
}

func cloneAny(value any) any {
	if value == nil {
		return nil
	}
	return cloneReflect(reflect.ValueOf(value), make(map[cloneVisit]reflect.Value), 0).Interface()
}

func cloneReflect(value reflect.Value, seen map[cloneVisit]reflect.Value, depth int) reflect.Value {
	if !value.IsValid() {
		return value
	}
	switch value.Kind() {
	case reflect.Interface:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		cloned := cloneReflect(value.Elem(), seen, depth+1)
		out := reflect.New(value.Type()).Elem()
		out.Set(cloned)
		return out
	case reflect.Pointer:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		visit := cloneVisit{typ: value.Type(), ptr: value.Pointer()}
		if prior, ok := seen[visit]; ok {
			return prior
		}
		out := reflect.New(value.Type().Elem())
		seen[visit] = out
		out.Elem().Set(cloneReflect(value.Elem(), seen, depth+1))
		return out
	case reflect.Map:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		visit := cloneVisit{typ: value.Type(), ptr: value.Pointer()}
		if prior, ok := seen[visit]; ok {
			return prior
		}
		out := reflect.MakeMapWithSize(value.Type(), value.Len())
		seen[visit] = out
		iter := value.MapRange()
		for iter.Next() {
			out.SetMapIndex(iter.Key(), cloneReflect(iter.Value(), seen, depth+1))
		}
		return out
	case reflect.Slice:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		visit := cloneVisit{typ: value.Type(), ptr: value.Pointer(), len: value.Len(), cap: value.Cap()}
		if prior, ok := seen[visit]; ok {
			return prior
		}
		out := reflect.MakeSlice(value.Type(), value.Len(), value.Len())
		seen[visit] = out
		for i := 0; i < value.Len(); i++ {
			out.Index(i).Set(cloneReflect(value.Index(i), seen, depth+1))
		}
		return out
	case reflect.Array:
		out := reflect.New(value.Type()).Elem()
		for i := 0; i < value.Len(); i++ {
			out.Index(i).Set(cloneReflect(value.Index(i), seen, depth+1))
		}
		return out
	case reflect.Struct:
		// Copy the whole value first so immutable structs with unexported
		// internals (time.Time, decimal types) retain their representation.
		out := reflect.New(value.Type()).Elem()
		out.Set(value)
		for i := 0; i < value.NumField(); i++ {
			if out.Field(i).CanSet() && value.Type().Field(i).PkgPath == "" {
				out.Field(i).Set(cloneReflect(value.Field(i), seen, depth+1))
			}
		}
		return out
	default:
		return value
	}
}
