package model

import "github.com/zynthara/chok/db"

// Post is a blog post owned by the authenticated user.
type Post struct {
	db.OwnedSoftDeleteModel
	Title   string `json:"title"   gorm:"size:200;not null"`
	Content string `json:"content" gorm:"type:text;not null"`
	Status  string `json:"status"  gorm:"size:20;default:'draft';not null"`
}

func (Post) RIDPrefix() string { return "pst" }

// Post statuses.
const (
	StatusDraft     = "draft"
	StatusPublished = "published"
)
