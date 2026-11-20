package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"workbuddy2api/internal/hotupdate"
)

// Set only by isolated CLI integration builds. Release builds use the standard
// release API and require the same embedded signing identity as ordinary updates.
var repairAPIBase string

func runRepairUpdate(configPath, action, target, explicitDir string, output io.Writer) error {
	previousLog := log.Writer()
	log.SetOutput(io.Discard)
	defer log.SetOutput(previousLog)
	if runtime.GOOS == "android" {
		return errors.New("repair_use_android_installer")
	}
	info, err := os.Stat(configPath)
	if err != nil || !info.Mode().IsRegular() {
		return errors.New("repair_existing_config_required")
	}
	cfg, err := loadConfigOrEnv(configPath)
	if err != nil {
		return errors.New("repair_config_unavailable")
	}
	cfg.Update.Repo = effectiveUpdateRepo(configPath, cfg.Update.Repo)
	dir := strings.TrimSpace(explicitDir)
	if dir == "" {
		dir = strings.TrimSpace(os.Getenv("WB2API_UPDATE_DIR"))
	}
	if dir == "" {
		dir = strings.TrimSpace(cfg.Update.Dir)
	}
	if dir == "" && filepath.IsAbs(cfg.StateFile) {
		dir = updateDir(cfg)
	}
	if !filepath.IsAbs(dir) {
		return errors.New("repair_update_directory_must_be_absolute")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	// The recovery command has no running server. Keep ordinary update logs
	// out of its allowlisted machine-readable response and error output.
	status, err := hotupdate.RepairUpdate(ctx, hotupdate.RepairOptions{
		Update: hotupdate.Options{Enabled: true, Repo: cfg.Update.Repo, Token: cfg.Update.Token,
			Proxy: cfg.Update.Proxy, Dir: dir, Bundle: hotupdate.BundleSupported()},
		Target: strings.TrimSpace(target), APIBase: repairAPIBase,
	}, action)
	if err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(status)
}
