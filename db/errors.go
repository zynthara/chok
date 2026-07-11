package db

import "errors"

// ErrReadOnly is returned when a write-capable operation is attempted on a
// database handle configured with read_only: true.
var ErrReadOnly = errors.New("db: read-only instance rejects write operation")
