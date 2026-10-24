package conf

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/spf13/pflag"
)

type logSection struct {
	Level  string       `yaml:"level" default:"info" validate:"oneof=debug info warn error"`
	Output []string     `yaml:"output" default:"stdout"`
	Files  []fileConfig `yaml:"files"`
}

type fileConfig struct {
	Path    string `yaml:"path"`
	MaxSize int    `yaml:"max_size" default:"100"`
}

type httpSection struct {
	Addr    string        `yaml:"addr" default:":8080"`
	Timeout time.Duration `yaml:"timeout" default:"30s"`
	Enabled bool          `yaml:"enabled" default:"true"`
}

type base struct {
	HTTP httpSection `yaml:"http"`
	Log  logSection  `yaml:"log"`
}

type provider struct {
	ClientID     string `yaml:"client_id"`
	ClientSecret string `yaml:"client_secret" secret:"true"`
	Scope        string `yaml:"scope" default:"email"`
}

type dbSection struct {
	Driver   string `yaml:"driver" default:"sqlite" validate:"oneof=sqlite mysql"`
	Password string `yaml:"password" secret:"true"`
	Port     int    `yaml:"port" default:"3306"`
}

// appConfig embeds base without a tag, as applications embed chok.Base.
type appConfig struct {
	base
	DB        dbSection           `yaml:"db"`
	Providers map[string]provider `yaml:"providers"`
}

func writeFile(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func load(t *testing.T, dst any, o Options) (Result, error) {
	t.Helper()
	if o.Name == "" {
		o.Name = "app"
	}
	if o.Environ == nil {
		o.Environ = []string{}
	}
	return Load(dst, o)
}

func problems(t *testing.T, err error) []string {
	t.Helper()
	var ce *Error
	if !errors.As(err, &ce) {
		t.Fatalf("want *conf.Error, got %v", err)
	}
	return ce.Problems
}

func TestLoad_DefaultTag(t *testing.T) {
	t.Chdir(t.TempDir())
	var c appConfig
	if _, err := load(t, &c, Options{}); err != nil {
		t.Fatal(err)
	}
	if c.HTTP.Addr != ":8080" || c.HTTP.Timeout != 30*time.Second || !c.HTTP.Enabled {
		t.Fatalf("http defaults: %+v", c.HTTP)
	}
	if c.Log.Level != "info" || !slices.Equal(c.Log.Output, []string{"stdout"}) || c.DB.Port != 3306 {
		t.Fatalf("defaults: %+v %+v", c.Log, c.DB)
	}
}

func TestLoad_FileOverridesDefault(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "c.yaml", "http:\n  addr: \":9000\"\n  enabled: false\nlog:\n  output: [stderr, /tmp/x.log]\n")
	var c appConfig
	if _, err := load(t, &c, Options{File: p}); err != nil {
		t.Fatal(err)
	}
	if c.HTTP.Addr != ":9000" || c.HTTP.Enabled || c.HTTP.Timeout != 30*time.Second {
		t.Fatalf("file must override only what it sets: %+v", c.HTTP)
	}
	if !slices.Equal(c.Log.Output, []string{"stderr", "/tmp/x.log"}) {
		t.Fatalf("a list from the file replaces the default: %v", c.Log.Output)
	}
}

func TestLoad_EnvOverridesFile(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "c.yaml", "http:\n  addr: \":9000\"\n")
	var c appConfig
	_, err := load(t, &c, Options{File: p, Environ: []string{"APP_HTTP_ADDR=:7000", "APP_LOG_OUTPUT=stdout, stderr", "APP_HTTP_TIMEOUT=5s"}})
	if err != nil {
		t.Fatal(err)
	}
	if c.HTTP.Addr != ":7000" || c.HTTP.Timeout != 5*time.Second {
		t.Fatalf("env must win over the file: %+v", c.HTTP)
	}
	if !slices.Equal(c.Log.Output, []string{"stdout", "stderr"}) {
		t.Fatalf("env lists are comma-separated: %v", c.Log.Output)
	}
}

func TestLoad_FlagsOverrideEnv(t *testing.T) {
	t.Chdir(t.TempDir())
	fs := pflag.NewFlagSet("t", pflag.ContinueOnError)
	fs.String("http.addr", "", "")
	fs.StringSlice("log.output", nil, "")
	fs.String("unrelated", "", "")
	if err := fs.Parse([]string{"--http.addr=:6000", "--log.output=a,b", "--unrelated=x"}); err != nil {
		t.Fatal(err)
	}
	var c appConfig
	if _, err := load(t, &c, Options{Flags: fs, Environ: []string{"APP_HTTP_ADDR=:7000"}}); err != nil {
		t.Fatal(err)
	}
	if c.HTTP.Addr != ":6000" || !slices.Equal(c.Log.Output, []string{"a", "b"}) {
		t.Fatalf("flags must win: %+v %v", c.HTTP, c.Log.Output)
	}
}

func TestLoad_ExplicitPathNotFound_Error(t *testing.T) {
	var c appConfig
	if _, err := load(t, &c, Options{File: filepath.Join(t.TempDir(), "missing.yaml")}); err == nil {
		t.Fatal("a missing explicit file must fail")
	}
}

func TestLoad_DefaultPathNotFound_Skip(t *testing.T) {
	t.Chdir(t.TempDir())
	var c appConfig
	res, err := load(t, &c, Options{})
	if err != nil || res.File != "" {
		t.Fatalf("no file to detect: res=%+v err=%v", res, err)
	}
}

func TestLoad_DefaultPathDetection(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	writeFile(t, dir, "app.yaml", "http:\n  addr: \":1\"\n")
	var c appConfig
	res, err := load(t, &c, Options{})
	if err != nil || res.File != "app.yaml" || c.HTTP.Addr != ":1" {
		t.Fatalf("res=%+v err=%v addr=%q", res, err, c.HTTP.Addr)
	}
	// chok.yaml comes first.
	writeFile(t, dir, "chok.yaml", "http:\n  addr: \":2\"\n")
	res, err = load(t, &c, Options{})
	if err != nil || res.File != "chok.yaml" || c.HTTP.Addr != ":2" {
		t.Fatalf("chok.yaml first: res=%+v err=%v addr=%q", res, err, c.HTTP.Addr)
	}
}

func TestLoad_ConfigsSubdirFallback(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	writeFile(t, dir, "configs/app.yaml", "http:\n  addr: \":3\"\n")
	var c appConfig
	res, err := load(t, &c, Options{})
	if err != nil || res.File != "configs/app.yaml" || c.HTTP.Addr != ":3" {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

func TestLoad_PrefixEnvConfigPath(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "elsewhere.yaml", "http:\n  addr: \":4\"\n")
	code := writeFile(t, dir, "code.yaml", "http:\n  addr: \":5\"\n")
	cmd := writeFile(t, dir, "cmd.yaml", "http:\n  addr: \":6\"\n")
	var c appConfig
	if _, err := load(t, &c, Options{File: code, Environ: []string{"APP_CONFIG=" + p}}); err != nil || c.HTTP.Addr != ":4" {
		t.Fatalf("{PREFIX}_CONFIG beats the code's file: %q %v", c.HTTP.Addr, err)
	}
	if _, err := load(t, &c, Options{Override: cmd, File: code, Environ: []string{"APP_CONFIG=" + p}}); err != nil || c.HTTP.Addr != ":6" {
		t.Fatalf("--config beats everything: %q %v", c.HTTP.Addr, err)
	}
	if _, err := load(t, &c, Options{Environ: []string{"APP_CONFIG=" + filepath.Join(dir, "nope.yaml")}}); err == nil {
		t.Fatal("a missing {PREFIX}_CONFIG file must fail")
	}
}

func TestLoad_DynamicMapKeyEnvOverride(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "c.yaml", "providers:\n  google:\n    client_id: abc\n")
	var c appConfig
	res, err := load(t, &c, Options{File: p, Environ: []string{
		"APP_PROVIDERS_GOOGLE_CLIENT_SECRET=s3cret",
		"APP_PROVIDERS_GITHUB_CLIENT_ID=nope",
	}})
	if err != nil {
		t.Fatal(err)
	}
	g := c.Providers["google"]
	if g.ClientID != "abc" || g.ClientSecret != "s3cret" || g.Scope != "email" {
		t.Fatalf("map entry from the file, secret from env, scope defaulted: %+v", g)
	}
	if _, ok := c.Providers["github"]; ok {
		t.Fatal("env must not invent a map key the file does not mention")
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "APP_PROVIDERS_GITHUB_CLIENT_ID") {
		t.Fatalf("the unmatched variable is a warning: %v", res.Warnings)
	}
}

func TestLoad_ValidateCollectsAllErrors(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "c.yaml", "log:\n  level: verbose\ndb:\n  driver: oracle\n")
	var c appConfig
	_, err := load(t, &c, Options{File: p})
	ps := problems(t, err)
	if len(ps) != 2 || !strings.Contains(ps[0], "db.driver: must be one of sqlite mysql") ||
		!strings.Contains(ps[1], "log.level: must be one of") {
		t.Fatalf("both rule failures, by yaml path: %q", ps)
	}
}

type selfChecked struct {
	Min int `yaml:"min"`
	Max int `yaml:"max"`
}

func (s *selfChecked) Validate() error {
	if s.Min > s.Max {
		return errors.New("min must not exceed max")
	}
	return nil
}

type withValidate struct {
	Range selfChecked `yaml:"range"`
	Items []struct {
		Range selfChecked `yaml:"range"`
	} `yaml:"items"`
}

func TestLoad_ValidateMethodsByPath(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "c.yaml", "range: {min: 3, max: 1}\nitems:\n  - range: {min: 1, max: 2}\n  - range: {min: 9, max: 2}\n")
	var c withValidate
	_, err := load(t, &c, Options{File: p})
	ps := problems(t, err)
	if !slices.Equal(ps, []string{"items[1].range: min must not exceed max", "range: min must not exceed max"}) {
		t.Fatalf("Validate errors carry their path: %q", ps)
	}
}

func TestLoad_UnknownSectionSuggests(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "c.yaml", "htpp:\n  addr: \":1\"\n")
	var c appConfig
	_, err := load(t, &c, Options{File: p})
	ps := problems(t, err)
	if len(ps) != 1 || ps[0] != "unknown section htpp (did you mean http?) (line 1)" {
		t.Fatalf("got %q", ps)
	}
}

func TestLoad_UnknownKeySuggests(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "c.yaml", "http:\n  adr: \":1\"\n  zzzzzz: 1\n")
	var c appConfig
	_, err := load(t, &c, Options{File: p})
	ps := problems(t, err)
	want := []string{"unknown key http.adr (did you mean addr?) (line 2)", "unknown key http.zzzzzz (line 3)"}
	if !slices.Equal(ps, want) {
		t.Fatalf("got %q", ps)
	}
	if !strings.Contains(err.Error(), "2 problems") {
		t.Fatalf("message counts the problems: %v", err)
	}
}

// TestLoad_OnlySections: with Only, the file's other sections are not
// unknown, the loaded ones are as strict as ever, and an environment
// variable outside them does not warn.
func TestLoad_OnlySections(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "c.yaml", "db:\n  port: 5432\nhttp:\n  addr: \":1\"\nauth:\n  signing_key: x\n")
	var c struct {
		DB dbSection `yaml:"db"`
	}
	res, err := load(t, &c, Options{File: p, Only: []string{"db"}, Environ: []string{"APP_DB_DRIVER=mysql", "APP_HTTP_ADDR=:2", "APP_DB_PORTT=1"}})
	if err != nil {
		t.Fatal(err)
	}
	if c.DB.Port != 5432 || c.DB.Driver != "mysql" {
		t.Fatalf("the db section from the file and the environment: %+v", c.DB)
	}
	if len(res.Warnings) != 1 || !strings.HasPrefix(res.Warnings[0], "environment variable APP_DB_PORTT matches no config key") {
		t.Fatalf("only the db section's variables warn: %q", res.Warnings)
	}
	bad := writeFile(t, dir, "bad.yaml", "db:\n  prot: 1\nhttp:\n  adr: x\n")
	_, err = load(t, &c, Options{File: bad, Only: []string{"db"}})
	if ps := problems(t, err); len(ps) != 1 || !strings.HasPrefix(ps[0], "unknown key db.prot (did you mean port?)") {
		t.Fatalf("the loaded section stays strict, the others are skipped: %q", ps)
	}
}

func TestLoad_EnvTypoWarns(t *testing.T) {
	t.Chdir(t.TempDir())
	var c appConfig
	res, err := load(t, &c, Options{Environ: []string{"APP_HTTP_ADR=:1", "APP_CONFIG=", "OTHER_X=1"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Warnings) != 1 || res.Warnings[0] != "environment variable APP_HTTP_ADR matches no config key (did you mean APP_HTTP_ADDR?)" {
		t.Fatalf("got %q", res.Warnings)
	}
}

func TestLoad_InlineEmbedding(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "c.yaml", "http:\n  addr: \":1\"\nbase:\n  x: 1\n")
	var c appConfig
	_, err := load(t, &c, Options{File: p})
	ps := problems(t, err)
	if len(ps) != 1 || !strings.HasPrefix(ps[0], "unknown section base") {
		t.Fatalf("an untagged embed is inlined, so base is not a section: %q", ps)
	}
	if !strings.Contains(strings.Join(fieldKeys(reflect.TypeFor[appConfig]()), ","), "http,log,db,providers") {
		t.Fatalf("keys: %v", fieldKeys(reflect.TypeFor[appConfig]()))
	}
}

func fieldKeys(t reflect.Type) []string {
	var out []string
	for _, f := range fieldsOf(t) {
		out = append(out, f.key)
	}
	return out
}

func TestLoad_TypeErrorsNamePath(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "c.yaml", "http:\n  timeout: 30\n  enabled: yes\ndb:\n  driver: sqlite\n  port: [1]\n")
	var c appConfig
	_, err := load(t, &c, Options{File: p})
	ps := problems(t, err)
	want := []string{
		`db.port: expected a single value, got a list (line 6)`,
		`http.enabled: "yes" is not true or false (line 3)`,
		`http.timeout: "30" is not a duration (write it like 30s, 5m or 1h30m) (line 2)`,
	}
	if !slices.Equal(ps, want) {
		t.Fatalf("got %q", ps)
	}
}

func TestLoad_ElementDefaults(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "c.yaml", "log:\n  files:\n    - path: a.log\n    - path: b.log\n      max_size: 5\n")
	var c appConfig
	if _, err := load(t, &c, Options{File: p}); err != nil {
		t.Fatal(err)
	}
	if len(c.Log.Files) != 2 || c.Log.Files[0].MaxSize != 100 || c.Log.Files[1].MaxSize != 5 {
		t.Fatalf("list elements get their defaults: %+v", c.Log.Files)
	}
}

func TestLoad_NullKeepsDefault(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "c.yaml", "http:\n  addr:\nlog:\n")
	var c appConfig
	if _, err := load(t, &c, Options{File: p}); err != nil {
		t.Fatal(err)
	}
	if c.HTTP.Addr != ":8080" || c.Log.Level != "info" {
		t.Fatalf("an empty value or section keeps the defaults: %+v %+v", c.HTTP, c.Log)
	}
}

func TestLoad_DuplicateYAMLKey(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "c.yaml", "http:\n  addr: a\n  addr: b\n")
	var c appConfig
	_, err := load(t, &c, Options{File: p})
	if err == nil || !strings.Contains(err.Error(), "duplicate key http.addr") {
		t.Fatalf("got %v", err)
	}
}

func TestLoad_MergeKeys(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "c.yaml", "x: &common\n  client_id: shared\n  scope: openid\nproviders:\n  google:\n    <<: *common\n    scope: profile\n")
	type cfg struct {
		X         provider            `yaml:"x"`
		Providers map[string]provider `yaml:"providers"`
	}
	var c cfg
	if _, err := load(t, &c, Options{File: p}); err != nil {
		t.Fatal(err)
	}
	if g := c.Providers["google"]; g.ClientID != "shared" || g.Scope != "profile" {
		t.Fatalf("merge keys fill what the mapping leaves out: %+v", g)
	}
}

func TestLoad_TopLevelMustBeMapping(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "c.yaml", "- a\n- b\n")
	var c appConfig
	if _, err := load(t, &c, Options{File: p}); err == nil || !strings.Contains(err.Error(), "mapping") {
		t.Fatalf("got %v", err)
	}
}

func TestLoad_RejectsNonStruct(t *testing.T) {
	var n int
	if _, err := Load(&n, Options{}); err == nil {
		t.Fatal("a non-struct type must be rejected")
	}
}

func TestDefaults(t *testing.T) {
	c := appConfig{DB: dbSection{Password: "kept"}}
	if err := Defaults(&c); err != nil {
		t.Fatal(err)
	}
	if c.HTTP.Addr != ":8080" || c.DB.Port != 3306 || c.DB.Driver != "sqlite" || c.DB.Password != "kept" {
		t.Fatalf("defaults fill tagged fields and keep the rest: %+v", c)
	}
}

func TestDump_MasksSecretsRecursively(t *testing.T) {
	c := appConfig{DB: dbSection{Driver: "mysql", Password: "hunter2"}, Providers: map[string]provider{
		"google": {ClientID: "id", ClientSecret: "shh"},
	}}
	c.HTTP.Timeout = 90 * time.Second
	out, err := Dump(c)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, leak := range []string{"hunter2", "shh"} {
		if strings.Contains(s, leak) {
			t.Fatalf("secret %q leaked:\n%s", leak, s)
		}
	}
	for _, want := range []string{"password: '***'", "client_secret: '***'", "client_id: id", "timeout: 1m30s", "http:\n"} {
		if !strings.Contains(s, want) {
			t.Fatalf("dump lacks %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "base:") {
		t.Fatalf("the inlined embed must not appear as a key:\n%s", s)
	}
}

func TestDump_EmptySecretStaysEmpty(t *testing.T) {
	out, err := Dump(dbSection{Driver: "sqlite"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `password: ""`) {
		t.Fatalf("an unset secret shows as unset:\n%s", out)
	}
}

func TestKeys_ListsLeaves(t *testing.T) {
	var paths []string
	secrets := map[string]bool{}
	for _, k := range Keys(reflect.TypeFor[appConfig]()) {
		paths = append(paths, k.Path)
		secrets[k.Path] = k.Secret
	}
	want := []string{
		"db.driver", "db.password", "db.port",
		"http.addr", "http.enabled", "http.timeout",
		"log.files[].max_size", "log.files[].path", "log.level", "log.output",
		"providers.<name>.client_id", "providers.<name>.client_secret", "providers.<name>.scope",
	}
	if !slices.Equal(paths, want) {
		t.Fatalf("got %q", paths)
	}
	if !secrets["db.password"] || secrets["db.port"] {
		t.Fatalf("secret flags: %v", secrets)
	}
}

func TestField_FindsByKeyPath(t *testing.T) {
	c := appConfig{}
	c.Log.Level = "warn"
	v, ok := Field(&c, "log.level")
	if !ok || v.String() != "warn" {
		t.Fatalf("got %v %v", v, ok)
	}
	if _, ok := Field(&c, "log.nope"); ok {
		t.Fatal("unknown path")
	}
}

func TestSuggest_EditDistance(t *testing.T) {
	if got := suggest("adr", []string{"addr", "port"}); got != " (did you mean addr?)" {
		t.Fatalf("got %q", got)
	}
	if got := suggest("completely", []string{"addr"}); got != "" {
		t.Fatalf("no near candidate: %q", got)
	}
}
