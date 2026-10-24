package where_test

import (
	"fmt"
	"net/url"

	"github.com/zynthara/chok/v3/store/where"
)

// A list request's query string becomes options; the allowed map is the
// store's query fields, so nothing else reaches the SQL.
func ExampleFromQuery() {
	allowed := map[string]string{"status": "status", "created_at": "created_at"}
	q, _ := url.ParseQuery("status=draft&order=created_at:desc&page=2&size=10")
	opts, err := where.FromQuery(q, allowed, true)
	fmt.Println(len(opts), err)

	_, err = where.FromQuery(url.Values{"author": {"bob"}}, allowed, true)
	fmt.Println(err)
	// Output:
	// 3 <nil>
	// where: unknown field "author": filter on one of created_at, status
}

// Contains, StartsWith and EndsWith match their input literally: a user
// searching for "50%" does not get a wildcard.
func ExampleContains() {
	_ = []where.Option{
		where.Contains("title", "50%"),          // title LIKE '%50!%%' ESCAPE '!'
		where.Like("sku", "AB-%"),               // the caller's wildcard
		where.In("status", "draft", "archived"), // status IN (...)
		where.Order("created_at", true),         // newest first
		where.Page(1, 20),
	}
}
