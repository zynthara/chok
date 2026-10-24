package db

import (
	"strings"
	"testing"
)

func TestSplitSQLStatements(t *testing.T) {
	tests := []struct {
		name  string
		in    string
		want  int
		check func(t *testing.T, stmts []string)
	}{
		{"two plain", "CREATE TABLE a (id INT); CREATE TABLE b (id INT);", 2, nil},
		{"trailing no semi", "SELECT 1; SELECT 2", 2, nil},
		{"semicolon in string", "INSERT INTO t VALUES ('a;b'); SELECT 1;", 2, func(t *testing.T, s []string) {
			if !strings.Contains(s[0], "'a;b'") {
				t.Fatalf("string literal split: %q", s[0])
			}
		}},
		{"escaped quote doubling", "INSERT INTO t VALUES ('it''s; fine'); SELECT 1;", 2, nil},
		{"backslash escape", `INSERT INTO t VALUES ('a\'; b'); SELECT 1;`, 2, nil},
		{"line comment", "SELECT 1; -- comment; with semicolons\nSELECT 2;", 2, nil},
		{"block comment", "SELECT 1; /* multi;\nline; */ SELECT 2;", 2, nil},
		{"quoted identifier", `CREATE TABLE "we;ird" (id INT); SELECT 1;`, 2, nil},
		{"backtick identifier", "CREATE TABLE `we;ird` (id INT); SELECT 1;", 2, nil},
		{"dollar quoted body", "CREATE FUNCTION f() RETURNS void AS $$\nBEGIN\n  UPDATE t SET x = 1;\n  UPDATE t SET y = 2;\nEND;\n$$ LANGUAGE plpgsql; SELECT 1;", 2, func(t *testing.T, s []string) {
			if !strings.Contains(s[0], "SET y = 2;") {
				t.Fatalf("dollar-quoted body split: %q", s[0])
			}
		}},
		{"tagged dollar quote", `DO $tag$ SELECT 1; SELECT 2; $tag$; SELECT 3;`, 2, nil},
		{"only comments", "-- nothing\n/* here */", 0, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := splitSQLStatements(tt.in)
			if len(got) != tt.want {
				t.Fatalf("want %d statements, got %d: %q", tt.want, len(got), got)
			}
			if tt.check != nil {
				tt.check(t, got)
			}
		})
	}
}
