package chok

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/spf13/pflag"

	"github.com/zynthara/chok/v3/internal/clientip"
	"github.com/zynthara/chok/v3/internal/conf"
)

// Base is the standard configuration every application embeds: the app,
// http and log sections. Embedded without a yaml tag, its sections sit at
// the top level of chok.yaml next to the application's own.
//
//	type Config struct {
//		chok.Base
//		DB db.Config `yaml:"db"`
//	}
//
// Base on its own is a valid configuration type too:
// chok.New(chok.MustLoad[chok.Base]("hello")).
type Base struct {
	App  AppConfig  `yaml:"app"`
	HTTP HTTPConfig `yaml:"http"`
	Log  LogConfig  `yaml:"log"`
}

// AppConfig is the app section.
type AppConfig struct {
	// ShutdownTimeout bounds the whole stop phase: drain, HTTP shutdown,
	// stopping the Starters and closing every component.
	ShutdownTimeout time.Duration `yaml:"shutdown_timeout" default:"30s"`
	// StartTimeout bounds the start phase: every Healther must report
	// healthy within it before the HTTP server listens.
	StartTimeout time.Duration `yaml:"start_timeout" default:"30s"`
	// Debug mounts /debug/pprof/* and /debug/pprof/goroutineleak.
	Debug bool `yaml:"debug"`
}

// HTTPConfig is the http section.
type HTTPConfig struct {
	// Enabled starts the HTTP server; false runs a worker process whose
	// Handler still serves in-process requests.
	Enabled           bool          `yaml:"enabled" default:"true"`
	Addr              string        `yaml:"addr" default:":8080"`
	ReadTimeout       time.Duration `yaml:"read_timeout" default:"30s"`
	ReadHeaderTimeout time.Duration `yaml:"read_header_timeout" default:"10s"`
	WriteTimeout      time.Duration `yaml:"write_timeout" default:"30s"`
	IdleTimeout       time.Duration `yaml:"idle_timeout" default:"120s"`
	// RequestTimeout cancels a request's context after it; 0 means none.
	RequestTimeout time.Duration `yaml:"request_timeout"`
	// ShutdownTimeout bounds http.Server.Shutdown.
	ShutdownTimeout time.Duration `yaml:"shutdown_timeout" default:"10s"`
	// DrainDelay is how long /readyz reports 503 before the server stops
	// accepting, so load balancers move traffic away first.
	DrainDelay time.Duration `yaml:"drain_delay" default:"5s"`
	// TrustedProxies lists the proxy addresses or CIDRs whose
	// X-Forwarded-For and X-Real-Ip headers are believed.
	TrustedProxies []string `yaml:"trusted_proxies"`
	// MaxBodyBytes caps a request body; a route can change it with MaxBody.
	MaxBodyBytes int64 `yaml:"max_body_bytes" default:"4194304" validate:"gte=0"`
	// H2C serves HTTP/2 without TLS (behind a proxy that speaks it).
	H2C  bool       `yaml:"h2c"`
	CORS CORSConfig `yaml:"cors"`
	// CSRF rejects cross-origin unsafe requests (http.CrossOriginProtection).
	CSRF    bool          `yaml:"csrf"`
	OpenAPI OpenAPIConfig `yaml:"openapi"`
	Health  HealthConfig  `yaml:"health"`
}

// CORSConfig is http.cors. CORS is active when AllowOrigins is set.
type CORSConfig struct {
	// AllowOrigins lists the allowed origins; "*" allows any.
	AllowOrigins     []string      `yaml:"allow_origins"`
	AllowMethods     []string      `yaml:"allow_methods" default:"GET,POST,PUT,PATCH,DELETE,HEAD,OPTIONS"`
	AllowHeaders     []string      `yaml:"allow_headers" default:"Origin,Content-Type,Accept,Authorization,X-Request-Id"`
	ExposeHeaders    []string      `yaml:"expose_headers" default:"X-Request-Id"`
	AllowCredentials bool          `yaml:"allow_credentials"`
	MaxAge           time.Duration `yaml:"max_age" default:"12h"`
}

// OpenAPIConfig is http.openapi.
type OpenAPIConfig struct {
	Enabled  bool   `yaml:"enabled" default:"true"`
	Title    string `yaml:"title"`
	Version  string `yaml:"version"`
	Path     string `yaml:"path" default:"/openapi.json"`
	DocsPath string `yaml:"docs_path" default:"/docs"`
}

// HealthConfig is http.health.
type HealthConfig struct {
	Enabled bool   `yaml:"enabled" default:"true"`
	Path    string `yaml:"path" default:"/healthz"`
	// ProbeTimeout bounds each component's Health call.
	ProbeTimeout time.Duration `yaml:"probe_timeout" default:"3s"`
}

// LogConfig is the log section.
type LogConfig struct {
	// Level is the one setting SIGHUP applies while running.
	Level  string `yaml:"level" default:"info" validate:"oneof=debug info warn warning error"`
	Format string `yaml:"format" default:"json" validate:"oneof=json text"`
	// Output lists sinks: stdout, stderr or a file path.
	Output []string     `yaml:"output" default:"stdout"`
	Files  []FileConfig `yaml:"files"`
	// Access writes one line per request (method, path, route, status,
	// bytes, duration, request_id, client_ip).
	Access      bool         `yaml:"access" default:"true"`
	AccessFiles []FileConfig `yaml:"access_files"`
}

// Validate rejects a blank output entry and a log with no sink at all.
func (c *LogConfig) Validate() error {
	for i, o := range c.Output {
		if strings.TrimSpace(o) == "" {
			return fmt.Errorf("output[%d] must not be empty", i)
		}
	}
	if len(c.Output) == 0 && !hasPath(c.Files) {
		return errors.New("output and files must not both be empty")
	}
	return nil
}

// Validate checks the proxy list and the CORS combination browsers refuse.
func (c *HTTPConfig) Validate() error {
	if _, err := clientip.NewResolver(c.TrustedProxies); err != nil {
		return err
	}
	if c.CORS.AllowCredentials && slices.Contains(c.CORS.AllowOrigins, "*") {
		return errors.New("cors.allow_credentials cannot be combined with the * origin: it would hand credentials to any site")
	}
	return nil
}

// FileConfig is one rotating log file.
type FileConfig struct {
	Path       string `yaml:"path"`
	MaxSizeMB  int    `yaml:"max_size_mb" default:"100" validate:"gte=0"`
	MaxBackups int    `yaml:"max_backups" default:"7" validate:"gte=0"`
	MaxAgeDays int    `yaml:"max_age_days" default:"30" validate:"gte=0"`
	Compress   bool   `yaml:"compress" default:"true"`
}

// LoadOption adjusts Load.
type LoadOption func(*loadConfig)

type loadConfig struct {
	file      string
	envPrefix string
	flags     *pflag.FlagSet
	args      []string
	argsSet   bool
}

// ConfigFile names the configuration file; a missing file is an error.
// --config and {PREFIX}_CONFIG still take precedence over it.
func ConfigFile(path string) LoadOption { return func(c *loadConfig) { c.file = path } }

// EnvPrefix replaces the environment prefix derived from the app name.
func EnvPrefix(prefix string) LoadOption { return func(c *loadConfig) { c.envPrefix = prefix } }

// Flags adds a parsed flag set as the highest-priority source: a changed
// flag named after a key path (--http.addr) sets that key.
func Flags(fs *pflag.FlagSet) LoadOption { return func(c *loadConfig) { c.flags = fs } }

// Args replaces the command line Load reads (os.Args[1:] by default).
// Load looks only for --config <file> and --check-config there and
// ignores everything else; Args(nil) makes it read nothing, as tests do.
func Args(args []string) LoadOption {
	return func(c *loadConfig) { c.args, c.argsSet = args, true }
}

// Load builds a T from, lowest to highest: `default` tags, the YAML file,
// environment variables and flags. name derives the environment prefix
// (upper case, - and . become _) and the file candidates.
//
// The file is the first of: --config <file>, {PREFIX}_CONFIG, ConfigFile,
// ./chok.yaml, ./{name}.yaml, ./configs/{name}.yaml; without one only
// defaults and the environment apply. Every key can be set through the
// environment: http.addr is {PREFIX}_HTTP_ADDR, lists are comma-separated.
//
// Load is strict. An unknown key fails with a suggestion ("unknown key
// http.adr (did you mean addr?)"), a value of the wrong type names its
// path, validate tags and Validate methods run on the result, and every
// problem is reported in one error. An environment variable with the
// prefix that matches no key is logged as a warning.
//
// Struct tags: yaml:"key" names the key; default:"value" is its default;
// secret:"true" masks it wherever the configuration is printed.
//
// With --check-config on the command line, Load prints the effective
// configuration with secrets masked and exits 0, or prints the problems
// and exits 2, without returning.
func Load[T any](name string, opts ...LoadOption) (T, error) {
	var cfg T
	var lc loadConfig
	for _, o := range opts {
		o(&lc)
	}
	args := lc.args
	if !lc.argsSet && len(os.Args) > 1 {
		args = os.Args[1:]
	}
	cl := parseCommandLine(args)
	o := conf.Options{Name: name, EnvPrefix: lc.envPrefix, Override: cl.config, File: lc.file, Flags: lc.flags}
	res, err := conf.Load(&cfg, o)
	for _, w := range res.Warnings {
		slog.Warn("chok: " + w)
	}
	if cl.check {
		checkConfig(name, res.File, cfg, err)
	}
	if err != nil {
		return cfg, err
	}
	remember(name, o, cfg)
	return cfg, nil
}

// MustLoad is Load that prints the problems to stderr and exits 2 on error.
func MustLoad[T any](name string, opts ...LoadOption) T {
	cfg, err := Load[T](name, opts...)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "%s: %v\n", name, err)
		exit(2)
	}
	return cfg
}

// Process hooks, replaced by tests.
var (
	exit             = os.Exit
	stdout io.Writer = os.Stdout
	stderr io.Writer = os.Stderr
)

type commandLine struct {
	config string
	check  bool
}

func parseCommandLine(args []string) commandLine {
	var cl commandLine
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			break
		}
		name, val, hasVal := strings.Cut(strings.TrimLeft(a, "-"), "=")
		if !strings.HasPrefix(a, "-") {
			continue
		}
		switch name {
		case "config":
			if !hasVal && i+1 < len(args) {
				i++
				val = args[i]
			}
			cl.config = val
		case "check-config":
			cl.check = true
		}
	}
	return cl
}

func checkConfig(name, file string, cfg any, err error) {
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "%s: %v\n", name, err)
		exit(2)
		return
	}
	out, derr := conf.Dump(cfg)
	if derr != nil {
		_, _ = fmt.Fprintf(stderr, "%s: %v\n", name, derr)
		exit(2)
		return
	}
	source := "defaults and environment only"
	if file != "" {
		source = "file " + file
	}
	_, _ = fmt.Fprintf(stdout, "# %s: configuration is valid (%s; secrets masked)\n%s", name, source, out)
	exit(0)
}

// loaded is the most recent successful Load, kept so SIGHUP can re-read
// the same sources and apply log.level.
type loaded struct {
	reread func() (level string, othersChanged bool, err error)
}

var lastLoad atomic.Pointer[loaded]

func remember[T any](name string, o conf.Options, cfg T) {
	level, ok := logLevel(&cfg)
	if !ok {
		lastLoad.Store(nil)
		return
	}
	lastLoad.Store(&loaded{reread: func() (string, bool, error) {
		var fresh T
		if _, err := conf.Load(&fresh, o); err != nil {
			return "", false, err
		}
		newLevel, _ := logLevel(&fresh)
		setLogLevel(&fresh, level)
		return newLevel, !reflect.DeepEqual(cfg, fresh), nil
	}})
	_ = name
}

func logLevel(cfg any) (string, bool) {
	v, ok := conf.Field(cfg, "log.level")
	if !ok || v.Kind() != reflect.String {
		return "", false
	}
	return v.String(), true
}

func setLogLevel(cfg any, level string) {
	if v, ok := conf.Field(cfg, "log.level"); ok && v.CanSet() {
		v.SetString(level)
	}
}
