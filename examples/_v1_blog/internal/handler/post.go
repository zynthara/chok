package handler

import (
	"context"

	"github.com/gin-gonic/gin"

	"github.com/zynthara/chok/examples/blog/internal/model"
	blogstore "github.com/zynthara/chok/examples/blog/internal/store"
	"github.com/zynthara/chok/handler"
	"github.com/zynthara/chok/store"
)

// RegisterPostRoutes registers blog post CRUD routes.
func RegisterPostRoutes(rg *gin.RouterGroup, posts *blogstore.PostStore) {
	h := &postHandler{posts: posts}

	rg.POST("/posts", handler.HandleRequest(h.create, handler.WithSuccessCode(201)))
	rg.GET("/posts", handler.HandleList[model.Post](posts))
	rg.GET("/posts/:rid", handler.HandleRequest(h.get))
	rg.PUT("/posts/:rid", handler.HandleRequest(h.update))
	rg.DELETE("/posts/:rid", handler.HandleAction(h.delete))
}

type postHandler struct {
	posts *blogstore.PostStore
}

func (h *postHandler) create(ctx context.Context, req *createPostRequest) (*model.Post, error) {
	p := &model.Post{
		Title:   req.Title,
		Content: req.Content,
		Status:  model.StatusDraft,
	}
	if err := h.posts.Create(ctx, p); err != nil {
		return nil, err
	}
	return p, nil
}

func (h *postHandler) get(ctx context.Context, req *getPostRequest) (*model.Post, error) {
	return h.posts.Get(ctx, store.RID(req.RID))
}

func (h *postHandler) update(ctx context.Context, req *updatePostRequest) (*model.Post, error) {
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
	if err := h.posts.Update(ctx, store.RID(p.RID), store.Fields(p, cols...)); err != nil {
		return nil, err
	}
	return p, nil
}

func (h *postHandler) delete(ctx context.Context, req *deletePostRequest) error {
	if req.Version > 0 {
		return h.posts.Delete(ctx, store.RID(req.RID), store.WithVersion(req.Version))
	}
	return h.posts.Delete(ctx, store.RID(req.RID))
}

// --- Requests ---

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
