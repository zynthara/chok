// Command configkeys counts the configuration keys an application built
// on chok can set — Base plus every battery's Config — against the
// config_keys limit in hack/budget.txt (plan §6). hack/configkeys.sh runs it.
package main

import (
	"fmt"
	"os"
	"reflect"
	"strconv"
	"strings"

	"github.com/zynthara/chok/v3"
	"github.com/zynthara/chok/v3/cache"
	"github.com/zynthara/chok/v3/db"
	"github.com/zynthara/chok/v3/identity"
	"github.com/zynthara/chok/v3/internal/conf"
	"github.com/zynthara/chok/v3/jobs"
	"github.com/zynthara/chok/v3/realtime"
	"github.com/zynthara/chok/v3/telemetry"
)

// sections are the configuration types counted; each battery adds its
// Config here when it lands.
var sections = []struct {
	name string
	typ  reflect.Type
}{
	{"chok.Base", reflect.TypeFor[chok.Base]()},
	{"db.Config", reflect.TypeFor[db.Config]()},
	{"identity.Config", reflect.TypeFor[identity.Config]()},
	{"jobs.Config", reflect.TypeFor[jobs.Config]()},
	{"telemetry.Config", reflect.TypeFor[telemetry.Config]()},
	{"cache.Config", reflect.TypeFor[cache.Config]()},
	{"realtime.Config", reflect.TypeFor[realtime.Config]()},
}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: configkeys hack/budget.txt")
		os.Exit(2)
	}
	limit, err := readLimit(os.Args[1], "config_keys")
	if err != nil {
		fmt.Fprintln(os.Stderr, "configkeys:", err)
		os.Exit(2)
	}
	total := 0
	fmt.Printf("%-36s %7s\n", "CONFIG SECTION", "KEYS")
	for _, s := range sections {
		n := len(conf.Keys(s.typ))
		total += n
		fmt.Printf("%-36s %7d\n", s.name, n)
	}
	status := "ok"
	if total > limit {
		status = "OVER"
	}
	fmt.Printf("%-36s %7d %7d  %s\n", "config keys total", total, limit, status)
	if total > limit {
		os.Exit(1)
	}
}

func readLimit(path, item string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	for line := range strings.Lines(string(data)) {
		fields := strings.Fields(line)
		if len(fields) >= 3 && fields[0] == "limit" && fields[1] == item {
			return strconv.Atoi(fields[2])
		}
	}
	return 0, fmt.Errorf("%s: no limit %s", path, item)
}
