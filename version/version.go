package version

import (
	"fmt"
	"runtime"
)

// Set via ldflags:
//
//	go build -ldflags "-X github.com/zynthara/chok/version.version=v1.0.0
//	  -X github.com/zynthara/chok/version.gitHash=abc1234
//	  -X github.com/zynthara/chok/version.buildTime=2024-01-01T00:00:00Z"
var (
	version   = "dev"
	gitHash   = "unknown"
	buildTime = "unknown"
)

type Info struct {
	Version   string `json:"version"`
	GitHash   string `json:"git_hash"`
	BuildTime string `json:"build_time"`
	GoVersion string `json:"go_version"`
}

func Get() Info {
	return Info{
		Version:   version,
		GitHash:   gitHash,
		BuildTime: buildTime,
		GoVersion: runtime.Version(),
	}
}

func (i Info) String() string {
	return fmt.Sprintf("%s (git: %s, built: %s, %s)", i.Version, i.GitHash, i.BuildTime, i.GoVersion)
}
