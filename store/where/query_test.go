package where

import (
	"net/url"
	"testing"
)

func TestFromQuery_Defaults(t *testing.T) {
	fields := map[string]string{"name": "name"}
	opts, err := FromQuery(url.Values{}, fields)
	if err != nil {
		t.Fatal(err)
	}
	// Should produce WithPage(1, 20) + WithCount = 2 options.
	if len(opts) != 2 {
		t.Fatalf("expected 2 options (page + count), got %d", len(opts))
	}
}

func TestFromQuery_PageSize(t *testing.T) {
	fields := map[string]string{"name": "name"}
	opts, err := FromQuery(url.Values{"page": {"3"}, "size": {"5"}}, fields)
	if err != nil {
		t.Fatal(err)
	}
	// page + count = 2.
	if len(opts) != 2 {
		t.Fatalf("expected 2 options, got %d", len(opts))
	}
}

func TestFromQuery_InvalidPage(t *testing.T) {
	fields := map[string]string{}
	_, err := FromQuery(url.Values{"page": {"abc"}}, fields)
	if err == nil {
		t.Fatal("expected error for non-integer page")
	}
}

func TestFromQuery_InvalidSize(t *testing.T) {
	fields := map[string]string{}
	_, err := FromQuery(url.Values{"size": {"abc"}}, fields)
	if err == nil {
		t.Fatal("expected error for non-integer size")
	}
}

func TestFromQuery_Order(t *testing.T) {
	fields := map[string]string{"created_at": "created_at", "name": "name"}
	opts, err := FromQuery(url.Values{"order": {"created_at:desc"}}, fields)
	if err != nil {
		t.Fatal(err)
	}
	// page + order + count = 3.
	if len(opts) != 3 {
		t.Fatalf("expected 3 options, got %d", len(opts))
	}
}

func TestFromQuery_OrderUnknownField(t *testing.T) {
	fields := map[string]string{"name": "name"}
	_, err := FromQuery(url.Values{"order": {"unknown:desc"}}, fields)
	if err == nil {
		t.Fatal("expected error for unknown order field")
	}
}

func TestFromQuery_OrderBadDirection(t *testing.T) {
	fields := map[string]string{"name": "name"}
	_, err := FromQuery(url.Values{"order": {"name:sideways"}}, fields)
	if err == nil {
		t.Fatal("expected error for invalid order direction")
	}
}

func TestFromQuery_Filters(t *testing.T) {
	fields := map[string]string{"status": "status", "name": "name"}
	opts, err := FromQuery(url.Values{"status": {"published"}, "name": {"alice"}}, fields)
	if err != nil {
		t.Fatal(err)
	}
	// page + 2 filters + count = 4.
	if len(opts) != 4 {
		t.Fatalf("expected 4 options, got %d", len(opts))
	}
}

func TestFromQuery_UnknownParamsIgnored(t *testing.T) {
	fields := map[string]string{"name": "name"}
	opts, err := FromQuery(url.Values{"name": {"alice"}, "bogus": {"value"}}, fields)
	if err != nil {
		t.Fatal(err)
	}
	// page + 1 filter + count = 3 (bogus is ignored).
	if len(opts) != 3 {
		t.Fatalf("expected 3 options, got %d", len(opts))
	}
}

func TestFromQuery_EmptyValueIgnored(t *testing.T) {
	fields := map[string]string{"name": "name"}
	opts, err := FromQuery(url.Values{"name": {""}}, fields)
	if err != nil {
		t.Fatal(err)
	}
	// page + count = 2 (empty value not added as filter).
	if len(opts) != 2 {
		t.Fatalf("expected 2 options, got %d", len(opts))
	}
}

func TestFromQuery_NilFieldMap(t *testing.T) {
	// No fields configured — only pagination works, filters/order ignored.
	opts, err := FromQuery(url.Values{"page": {"1"}, "size": {"10"}, "name": {"alice"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	// page + count = 2 (name ignored because fieldMap is nil).
	if len(opts) != 2 {
		t.Fatalf("expected 2 options, got %d", len(opts))
	}
}
