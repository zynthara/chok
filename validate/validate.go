package validate

import "context"

// Func is a typed validation function for request structs.
// Used as a complement to binding tags for cross-field / business-rule checks.
type Func[T any] func(ctx context.Context, req *T) error
