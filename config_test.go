package chok

import (
	"bytes"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// exitCode is what the exit stub panics with.
type exitCode int

// stubProcess replaces exit, stdout and stderr for one test.
func stubProcess(t *testing.T) (out, errOut *bytes.Buffer) {
	t.Helper()
	out, errOut = &bytes.Buffer{}, &bytes.Buffer{}
	oldExit, oldOut, oldErr := exit, stdout, stderr
	exit = func(code int) { panic(exitCode(code)) }
	stdout, stderr = out, errOut
	t.Cleanup(func() { exit, stdout, stderr = oldExit, oldOut, oldErr })
	return out, errOut
}

// exited runs fn and returns the code it exited with, or -1.
func exited(fn func()) (code int) {
	defer func() {
		if p := recover(); p != nil {
			c, ok := p.(exitCode)
			if !ok {
				panic(p)
			}
			code = int(c)
		}
	}()
	fn()
	return -1
}

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "chok.yaml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoad_BaseOnItsOwn(t *testing.T) {
	t.Chdir(t.TempDir())
	b := MustLoad[Base]("hello", Args(nil))
	if !b.HTTP.Enabled || b.HTTP.Addr != ":8080" || b.Log.Level != "info" || b.HTTP.OpenAPI.Path != "/openapi.json" ||
		b.App.ShutdownTimeout != 30*time.Second || b.HTTP.MaxBodyBytes != 4<<20 {
		t.Fatalf("Base defaults: %+v", b)
	}
}

type appConfig struct {
	Base
	Store struct {
		DSN  string `yaml:"dsn" secret:"true"`
		Pool int    `yaml:"pool" default:"4"`
	} `yaml:"store"`
}

func TestLoad_EmbeddedBaseIsInline(t *testing.T) {
	p := writeConfig(t, "http:\n  addr: \"127.0.0.1:0\"\nstore:\n  dsn: postgres://u:p@h/db\n")
	c, err := Load[appConfig]("app", ConfigFile(p), Args(nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.HTTP.Addr != "127.0.0.1:0" || c.Store.Pool != 4 || c.Store.DSN == "" {
		t.Fatalf("got %+v", c)
	}
}

func TestLoad_TyposFailWithSuggestions(t *testing.T) {
	p := writeConfig(t, "htpp:\n  addr: x\nlog:\n  levl: debug\n")
	_, err := Load[appConfig]("app", ConfigFile(p), Args(nil))
	msg := fmt.Sprint(err)
	for _, want := range []string{"unknown section htpp (did you mean http?)", "unknown key log.levl (did you mean level?)"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("missing %q in %v", want, err)
		}
	}
}

func TestLoad_EnvTypoIsWarningNotError(t *testing.T) {
	t.Chdir(t.TempDir())
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	t.Setenv("TYPO_HTTP_ADR", ":1")
	if _, err := Load[Base]("typo", Args(nil)); err != nil {
		t.Fatalf("an unmatched variable is not an error: %v", err)
	}
	if !strings.Contains(buf.String(), "TYPO_HTTP_ADR matches no config key (did you mean TYPO_HTTP_ADDR?)") {
		t.Fatalf("warning: %s", buf.String())
	}
}

func TestLoad_ConfigFlagOnCommandLine(t *testing.T) {
	p := writeConfig(t, "http:\n  addr: \":7\"\n")
	for _, args := range [][]string{{"--config", p}, {"--config=" + p}, {"-v", "-config", p, "serve"}} {
		b, err := Load[Base]("app", Args(args))
		if err != nil || b.HTTP.Addr != ":7" {
			t.Fatalf("%v: addr=%q err=%v", args, b.HTTP.Addr, err)
		}
	}
}

func TestLoad_CheckConfigPrintsMaskedAndExits0(t *testing.T) {
	out, _ := stubProcess(t)
	p := writeConfig(t, "store:\n  dsn: postgres://user:hunter2@db/app\n")
	code := exited(func() { _, _ = Load[appConfig]("app", Args([]string{"--config", p, "--check-config"})) })
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	s := out.String()
	if strings.Contains(s, "hunter2") || !strings.Contains(s, "dsn: '***'") || !strings.Contains(s, "configuration is valid") ||
		!strings.Contains(s, "addr: :8080") {
		t.Fatalf("output:\n%s", s)
	}
}

func TestLoad_CheckConfigFailureExits2(t *testing.T) {
	_, errOut := stubProcess(t)
	p := writeConfig(t, "http:\n  adr: x\n")
	code := exited(func() { _, _ = Load[Base]("app", Args([]string{"--check-config", "--config=" + p})) })
	if code != 2 || !strings.Contains(errOut.String(), "unknown key http.adr") {
		t.Fatalf("exit %d, stderr %q", code, errOut.String())
	}
}

func TestMustLoad_ExitsOnError(t *testing.T) {
	_, errOut := stubProcess(t)
	p := writeConfig(t, "log:\n  level: loud\n")
	code := exited(func() { MustLoad[Base]("app", ConfigFile(p), Args(nil)) })
	if code != 2 || !strings.Contains(errOut.String(), "log.level: must be one of") {
		t.Fatalf("exit %d, stderr %q", code, errOut.String())
	}
}

func TestLogConfig_RejectsBlankOutput(t *testing.T) {
	p := writeConfig(t, "log:\n  output: [stdout, \" \"]\n")
	_, err := Load[Base]("app", ConfigFile(p), Args(nil))
	if err == nil || !strings.Contains(err.Error(), "log: output[1] must not be empty") {
		t.Fatalf("got %v", err)
	}
}

func TestLogConfig_AllBlankFilesIsEmpty(t *testing.T) {
	p := writeConfig(t, "log:\n  output: []\n  files:\n    - path: \"\"\n")
	_, err := Load[Base]("app", ConfigFile(p), Args(nil))
	if err == nil || !strings.Contains(err.Error(), "output and files must not both be empty") {
		t.Fatalf("got %v", err)
	}
}

func TestHTTPConfig_Validate(t *testing.T) {
	p := writeConfig(t, "http:\n  trusted_proxies: [\"10.0.0.0/33\"]\n  cors:\n    allow_origins: [\"*\"]\n    allow_credentials: true\n")
	_, err := Load[Base]("app", ConfigFile(p), Args(nil))
	if err == nil || !strings.Contains(err.Error(), "trusted_proxies") {
		t.Fatalf("got %v", err)
	}
	p = writeConfig(t, "http:\n  cors:\n    allow_origins: [\"*\"]\n    allow_credentials: true\n")
	_, err = Load[Base]("app", ConfigFile(p), Args(nil))
	if err == nil || !strings.Contains(err.Error(), "allow_credentials") {
		t.Fatalf("got %v", err)
	}
}

func TestParseCommandLine(t *testing.T) {
	cl := parseCommandLine([]string{"run", "--check-config", "--config", "a.yaml", "--", "--config=b.yaml"})
	if !cl.check || cl.config != "a.yaml" {
		t.Fatalf("got %+v", cl)
	}
	if cl := parseCommandLine([]string{"-test.v", "-test.run=X"}); cl.check || cl.config != "" {
		t.Fatalf("test flags are not ours: %+v", cl)
	}
}
