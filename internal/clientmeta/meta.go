package clientmeta

import (
	"fmt"
	"runtime/debug"
	"strings"
	"sync"
)

const RepositoryURL = "https://github.com/wojas/ob1"

var (
	userAgentOnce sync.Once
	userAgent     string
)

func UserAgent() string {
	userAgentOnce.Do(func() {
		userAgent = buildUserAgent()
	})

	return userAgent
}

func buildUserAgent() string {
	version := "dev"
	revision := ""
	modified := false

	if info, ok := debug.ReadBuildInfo(); ok {
		if strings.TrimSpace(info.Main.Version) != "" && info.Main.Version != "(devel)" {
			version = info.Main.Version
		}

		for _, setting := range info.Settings {
			switch setting.Key {
			case "vcs.revision":
				revision = shortRevision(setting.Value)
			case "vcs.modified":
				modified = setting.Value == "true"
			}
		}
	}

	if revision == "" {
		return fmt.Sprintf("ob1/%s (%s)", version, RepositoryURL)
	}

	if modified {
		revision += "-dirty"
	}

	return fmt.Sprintf("ob1/%s (%s; commit %s)", version, RepositoryURL, revision)
}

func shortRevision(value string) string {
	trimmed := strings.TrimSpace(value)
	if len(trimmed) > 12 {
		return trimmed[:12]
	}

	return trimmed
}
