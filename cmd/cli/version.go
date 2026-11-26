package main

import "runtime/debug"

// version is the release version. gitSHA may be stamped at build time with
//
//	-ldflags "-X main.gitSHA=<sha>"
//
// (the Dockerfile does this from the GIT_SHA build arg). When it is not,
// buildSHA falls back to the VCS revision Go embeds in the binary.
var (
	version = "0.2.8"
	gitSHA  = ""
)

func buildSHA() string {
	if gitSHA != "" {
		return gitSHA
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			if s.Key == "vcs.revision" && s.Value != "" {
				return s.Value
			}
		}
	}
	return "unknown"
}
