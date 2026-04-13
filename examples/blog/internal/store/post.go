package store

import (
	"github.com/zynthara/chok/examples/blog/internal/model"
	"github.com/zynthara/chok/store"
)

// PostStore wraps the generic store with post-specific queries.
type PostStore struct {
	*store.Store[model.Post]
}

func NewPostStore(s *store.Store[model.Post]) *PostStore {
	return &PostStore{Store: s}
}
