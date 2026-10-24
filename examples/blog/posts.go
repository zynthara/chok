package main

import (
	"context"

	"github.com/zynthara/chok/v3"
	"github.com/zynthara/chok/v3/store"
)

type createPostRequest struct {
	Title   string `json:"title"   validate:"required,max=200"`
	Content string `json:"content" validate:"required"`
}

type postID struct {
	ID string `path:"id" validate:"required"`
}

// updatePostRequest is a partial update: a nil field was not sent.
// Version is optional; when present it must match the stored row.
type updatePostRequest struct {
	ID      string  `path:"id" validate:"required"`
	Version *int    `json:"version"`
	Title   *string `json:"title"   validate:"omitempty,max=200"`
	Content *string `json:"content"`
	Status  *string `json:"status"  validate:"omitempty,oneof=draft published"`
}

func createPost(posts *store.Store[Post]) chok.HandlerFunc[createPostRequest, *Post] {
	return func(ctx context.Context, req *createPostRequest) (*Post, error) {
		p := &Post{Title: req.Title, Content: req.Content, Status: "draft"}
		// The store stamps OwnerID from the authenticated principal.
		if err := posts.Create(ctx, p); err != nil {
			return nil, err
		}
		return p, nil
	}
}

func getPost(posts *store.Store[Post]) chok.HandlerFunc[postID, *Post] {
	return func(ctx context.Context, req *postID) (*Post, error) {
		return posts.Get(ctx, store.RID(req.ID))
	}
}

func updatePost(posts *store.Store[Post]) chok.HandlerFunc[updatePostRequest, *Post] {
	return func(ctx context.Context, req *updatePostRequest) (*Post, error) {
		p, err := posts.Get(ctx, store.RID(req.ID))
		if err != nil {
			return nil, err
		}
		// Patch takes the non-nil json fields; Onto applies them to p and
		// locks on the client's version (or p's), so a stale edit is a 409.
		ch := store.Patch(req).Onto(p)
		if ch.IsEmpty() {
			return p, nil
		}
		if err := posts.Update(ctx, store.RID(req.ID), ch); err != nil {
			return nil, err
		}
		return p, nil
	}
}

func deletePost(posts *store.Store[Post]) chok.HandlerFunc[postID, chok.None] {
	return func(ctx context.Context, req *postID) (chok.None, error) {
		return chok.None{}, posts.Delete(ctx, store.RID(req.ID))
	}
}
