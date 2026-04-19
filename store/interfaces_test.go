package store

import "github.com/zynthara/chok/db"

// Compile-time assertions that *Store[T] satisfies the narrow interfaces.

type ifaceTestModel struct {
	db.Model
}

func (ifaceTestModel) RIDPrefix() string { return "ift" }

var (
	_ Reader[ifaceTestModel]     = (*Store[ifaceTestModel])(nil)
	_ Writer[ifaceTestModel]     = (*Store[ifaceTestModel])(nil)
	_ ReadWriter[ifaceTestModel] = (*Store[ifaceTestModel])(nil)
)
