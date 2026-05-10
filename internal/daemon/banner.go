package daemon

import "fmt"

// bannerInfo collects the fields the boot banner needs from the
// boot path. Kept private so changes to the banner format don't
// ripple to callers.
type bannerInfo struct {
	Version       string
	Commit        string
	SchemaVersion int
	Bind          string
	DataDir       string
	DBPath        string
	ConfigPath    string
	SignerStatus  string
	SecretsStatus string
}

// formatBanner produces the multi-line boot banner per
// design/daemon.md §2.9. Format is intentionally stable — operators
// grep for "ready." and parse the lines above it.
func formatBanner(bi bannerInfo) []string {
	version := bi.Version
	if version == "" {
		version = "dev"
	}
	commit := bi.Commit
	if commit == "" {
		commit = "unknown"
	}
	configPath := bi.ConfigPath
	if configPath == "" {
		configPath = "(none — built-in defaults + env + flags)"
	}
	return []string{
		fmt.Sprintf("resy-snipe %s (commit %s, schema v%d)", version, commit, bi.SchemaVersion),
		"  bind:       " + bi.Bind,
		"  data-dir:   " + bi.DataDir,
		"  data-db:    " + bi.DBPath,
		"  config:     " + configPath,
		"  signer:     " + bi.SignerStatus,
		"  secrets:    " + bi.SecretsStatus,
		"ready.",
	}
}
