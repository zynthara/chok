package db

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/zynthara/chok/v2/conf"
)

func validSQLiteOptions() Options {
	return Options{Enabled: true, Driver: "sqlite", Migrate: MigrateAuto,
		SQLite: SQLiteOptions{Path: ":memory:"}}
}

func TestOptions_Validate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Options)
		wantErr string // "" = valid
	}{
		{"valid sqlite", func(o *Options) {}, ""},
		{"disabled skips checks", func(o *Options) { o.Enabled = false; o.Driver = "" }, ""},
		{"empty driver", func(o *Options) { o.Driver = "" }, "driver must be set"},
		{"unknown driver", func(o *Options) { o.Driver = "oracle" }, "unsupported driver"},
		{"bad migrate mode", func(o *Options) { o.Migrate = "sometimes" }, "migrate must be one of"},
		{"negative migration status interval", func(o *Options) { o.MigrationStatusInterval = -time.Second }, "migration_status_interval must be >= 0"},
		{"sqlite empty path", func(o *Options) { o.SQLite.Path = "" }, "path must not be empty"},
		{"mysql missing host", func(o *Options) {
			o.Driver = "mysql"
			o.MySQL = MySQLOptions{Port: 3306, Database: "d"}
		}, "mysql: host"},
		{"mysql bad port", func(o *Options) {
			o.Driver = "mysql"
			o.MySQL = MySQLOptions{Host: "h", Port: 70000, Database: "d"}
		}, "port must be 1-65535"},
		{"mysql bad tls enum", func(o *Options) {
			o.Driver = "mysql"
			o.MySQL = MySQLOptions{Host: "h", Port: 3306, Database: "d", TLS: "yes-please"}
		}, "tls must be one of"},
		{"mysql valid with tls and ca", func(o *Options) {
			o.Driver = "mysql"
			o.MySQL = MySQLOptions{Host: "h", Port: 3306, Database: "d", TLS: "skip-verify", CACert: "/etc/ca.pem"}
		}, ""},
		{"postgres discrete valid", func(o *Options) {
			o.Driver = "postgres"
			o.Postgres = PostgresOptions{Host: "h", Port: 5432, Database: "d", SSLMode: "disable"}
		}, ""},
		{"postgres dsn valid", func(o *Options) {
			o.Driver = "postgres"
			o.Postgres = PostgresOptions{DSN: "postgres://u:p@h/d"}
		}, ""},
		{"postgres dsn plus discrete conflict", func(o *Options) {
			o.Driver = "postgres"
			o.Postgres = PostgresOptions{DSN: "postgres://u:p@h/d", Database: "d"}
		}, "mutually exclusive"},
		{"postgres bad sslmode", func(o *Options) {
			o.Driver = "postgres"
			o.Postgres = PostgresOptions{Host: "h", Port: 5432, Database: "d", SSLMode: "sorta"}
		}, "ssl_mode must be one of"},
		{"postgres missing database", func(o *Options) {
			o.Driver = "postgres"
			o.Postgres = PostgresOptions{Host: "h", Port: 5432, SSLMode: "disable"}
		}, "database must not be empty"},
		{"store policy valid", func(o *Options) {
			o.Store = StorePolicy{Strict: true, RequirePrincipal: true, MaxPageSize: 100, DefaultPageSize: 20}
		}, ""},
		{"store negative max page size", func(o *Options) {
			o.Store = StorePolicy{MaxPageSize: -1}
		}, "max_page_size must be >= 0"},
		{"store negative default page size", func(o *Options) {
			o.Store = StorePolicy{DefaultPageSize: -1}
		}, "default_page_size must be >= 0"},
		{"store default exceeds max", func(o *Options) {
			o.Store = StorePolicy{MaxPageSize: 10, DefaultPageSize: 50}
		}, "exceeds max_page_size"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := validSQLiteOptions()
			tt.mutate(&o)
			err := o.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("want valid, got %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("want error containing %q, got %v", tt.wantErr, err)
			}
		})
	}
}

// Options is a discriminator config: the conf walker must not descend
// into unselected branches (a sqlite config must not fail on the empty
// mysql block).
func TestOptions_IsSelfValidating(t *testing.T) {
	var v conf.SelfValidating = &Options{}
	_ = v
}

func TestOptions_GoStringRedactsCredentials(t *testing.T) {
	o := Options{
		Enabled: true, Driver: "mysql", Migrate: MigrateAuto,
		MySQL:    MySQLOptions{Host: "db.internal", Username: "app", Password: "hunter2", Database: "prod"},
		Postgres: PostgresOptions{DSN: "postgres://app:sw0rdf1sh@pg/prod", Password: "pgpw"},
	}
	for _, format := range []string{"%#v", "%v", "%+v"} {
		got := fmt.Sprintf(format, o)
		for _, secret := range []string{"hunter2", "sw0rdf1sh", "pgpw"} {
			if strings.Contains(got, secret) {
				t.Fatalf("%s leaked %q: %s", format, secret, got)
			}
		}
	}
	// Non-sensitive context must survive for diagnostics.
	got := fmt.Sprintf("%#v", o)
	if !strings.Contains(got, "db.internal") || !strings.Contains(got, "prod") {
		t.Fatalf("GoString lost non-sensitive fields: %s", got)
	}
}

func TestPostgresKeywordDSN_QuotesAwkwardValues(t *testing.T) {
	dsn := postgresKeywordDSN(&PostgresOptions{
		Host: "h", Port: 5432, Username: "user",
		Password: `p a'ss\word`, Database: "d", SSLMode: "disable",
	})
	if !strings.Contains(dsn, `password='p a\'ss\\word'`) {
		t.Fatalf("password not quoted/escaped: %s", dsn)
	}
	if !strings.Contains(dsn, "host='h'") || !strings.Contains(dsn, "sslmode='disable'") {
		t.Fatalf("keyword dsn malformed: %s", dsn)
	}
}
