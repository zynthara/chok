package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/zynthara/chok/v2/store/where"
)

type listItem struct {
	Name string `json:"name"`
}

// pageLister fakes a QueryLister returning canned items + meta.
type pageLister struct {
	items []listItem
	total int64
	meta  where.PageInfo
}

func (f pageLister) ListFromQuery(context.Context, url.Values) ([]listItem, int64, where.PageInfo, error) {
	return f.items, f.total, f.meta, nil
}

// TestHandleList_EnvelopeRendersEffectivePagination: the envelope's
// page/size/has_more come from the lister's PageInfo — same-sourced
// with the SQL — not re-derived from the raw request. The request here
// deliberately contradicts the meta (size=5000&page=9): the old
// re-parse would have echoed it.
func TestHandleList_EnvelopeRendersEffectivePagination(t *testing.T) {
	lister := pageLister{
		items: []listItem{{Name: "a"}, {Name: "b"}},
		total: 3,
		meta:  where.PageInfo{Page: 2, Size: 2, Offset: 2, HasMore: false},
	}
	h := HandleList[listItem](lister)

	req := httptest.NewRequest(http.MethodGet, "/items?size=5000&page=9", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", w.Code, w.Body.String())
	}
	var resp struct {
		Items   []listItem `json:"items"`
		Total   int64      `json:"total"`
		Page    int        `json:"page"`
		Size    int        `json:"size"`
		HasMore bool       `json:"has_more"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v (%s)", err, w.Body.String())
	}
	if resp.Page != 2 || resp.Size != 2 || resp.HasMore {
		t.Fatalf("envelope must render the effective pagination, got page=%d size=%d has_more=%v",
			resp.Page, resp.Size, resp.HasMore)
	}
	if resp.Total != 3 || len(resp.Items) != 2 {
		t.Fatalf("payload passthrough broken: total=%d items=%d", resp.Total, len(resp.Items))
	}
}
