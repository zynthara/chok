package main

import (
	"context"
	"net/http"

	"github.com/zynthara/chok/v2/account"
	"github.com/zynthara/chok/v2/db"
	"github.com/zynthara/chok/v2/handler"
	"github.com/zynthara/chok/v2/kernel"
	"github.com/zynthara/chok/v2/log"
	"github.com/zynthara/chok/v2/store"
	"github.com/zynthara/chok/v2/web"
)

// routes wires the post CRUD behind the blessed auth guard. account
// mounts /auth (register/login/refresh/...) on its own; everything
// under /api/v1 requires the Bearer token those endpoints issue.
func routes(r kernel.Router, k kernel.Kernel) error {
	// Field allowlists ride the `store` tags on Post itself; pass
	// WithQueryFields / WithUpdateFields here only to narrow that
	// declaration for a specific consumer.
	posts := store.New[Post](db.From(k), log.From(k))
	h := &postHandlers{posts: posts}

	api := r.Group("/api/v1", account.Authn(k))
	web.POST(api, "/posts", h.create,
		handler.WithSuccessCode(http.StatusCreated),
		handler.WithSummary("Create a post"), handler.WithTags("posts"))
	api.Handle(http.MethodGet, "/posts", handler.HandleList[Post](posts,
		handler.WithSummary("List my posts"), handler.WithTags("posts")))
	web.GET(api, "/posts/{rid}", h.get,
		handler.WithSummary("Get one post"), handler.WithTags("posts"))
	web.PUT(api, "/posts/{rid}", h.update,
		handler.WithSummary("Update a post"), handler.WithTags("posts"))
	web.DELETE(api, "/posts/{rid}", h.delete,
		handler.WithSummary("Delete a post"), handler.WithTags("posts"))
	return nil
}

type postHandlers struct {
	posts *store.Store[Post]
}

func (h *postHandlers) create(ctx context.Context, req *createPostRequest) (*Post, error) {
	p := &Post{Title: req.Title, Content: req.Content, Status: StatusDraft}
	// OwnerID is filled from the authenticated principal by the store —
	// handlers never touch ownership.
	if err := h.posts.Create(ctx, p); err != nil {
		return nil, err
	}
	return p, nil
}

func (h *postHandlers) get(ctx context.Context, req *getPostRequest) (*Post, error) {
	return h.posts.Get(ctx, store.RID(req.RID))
}

func (h *postHandlers) update(ctx context.Context, req *updatePostRequest) (*Post, error) {
	p, err := h.posts.Get(ctx, store.RID(req.RID))
	if err != nil {
		return nil, err
	}
	var cols []string
	if req.Title != nil {
		p.Title = *req.Title
		cols = append(cols, "title")
	}
	if req.Content != nil {
		p.Content = *req.Content
		cols = append(cols, "content")
	}
	if req.Status != nil {
		p.Status = *req.Status
		cols = append(cols, "status")
	}
	if len(cols) == 0 {
		return p, nil
	}
	// Fields carries p.Version → optimistic lock: a concurrent editor
	// gets 409 instead of silently overwriting.
	if err := h.posts.Update(ctx, store.RID(p.RID), store.Fields(p, cols...)); err != nil {
		return nil, err
	}
	return p, nil
}

func (h *postHandlers) delete(ctx context.Context, req *deletePostRequest) error {
	if req.Version > 0 {
		return h.posts.Delete(ctx, store.RID(req.RID), store.WithVersion(req.Version))
	}
	return h.posts.Delete(ctx, store.RID(req.RID))
}

// --- request shapes ---------------------------------------------------------

type createPostRequest struct {
	Title   string `json:"title"   binding:"required,max=200"`
	Content string `json:"content" binding:"required"`
}

type getPostRequest struct {
	RID string `uri:"rid" binding:"required"`
}

type updatePostRequest struct {
	RID     string  `uri:"rid"     binding:"required"`
	Title   *string `json:"title"   binding:"omitempty,max=200"`
	Content *string `json:"content"`
	Status  *string `json:"status"  binding:"omitempty,oneof=draft published"`
}

type deletePostRequest struct {
	RID     string `uri:"rid" binding:"required"`
	Version int    `json:"version"`
}
