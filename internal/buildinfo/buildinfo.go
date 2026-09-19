package buildinfo

import "runtime/debug"

const Version = "0.1.0"

// Revision is injected by the upgrader; ordinary go builds use VCS metadata.
var Revision = "dev"

func init() {
	if Revision != "dev" {
		return
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range info.Settings {
			if setting.Key == "vcs.revision" {
				Revision = setting.Value
			}
		}
		for _, setting := range info.Settings {
			if setting.Key == "vcs.modified" && setting.Value == "true" {
				Revision += "-dirty"
			}
		}
	}
}
