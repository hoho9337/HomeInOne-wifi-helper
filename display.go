// Panel backlight control.
//
// The kiosk UI can't write /sys/class/backlight (it's a browser, in a
// container), so brightness rides the same privileged Unix-socket API as wifi:
// Caddy forwards /api/display/* here with the prefix stripped.
//
// The persisted percentage in stateFile is the single source of truth for
// "how bright the user wants this panel". hio-display-idle.sh reads the same
// file when it wakes the screen, so a brightness set from Settings survives an
// idle/wake cycle instead of being clobbered by a stale captured value.

package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

var (
	backlightDir = envOr("HIO_BACKLIGHT_DIR", "/sys/class/backlight/backlight")
	stateFile    = envOr("HIO_BRIGHTNESS_STATE", "/var/lib/hio/brightness")
)

// Never let the UI black the screen out completely — at 0% the user can't see
// the slider to turn it back up, and the panel looks broken/bricked.
const (
	minPct     = 5
	defaultPct = 100
)

type Brightness struct {
	Pct int `json:"pct"` // 5-100
}

func brightnessGetHandler(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, Brightness{Pct: readDesiredPct()})
}

func brightnessSetHandler(w http.ResponseWriter, r *http.Request) {
	var req Brightness
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	pct := clampPct(req.Pct)

	if err := applyPct(pct); err != nil {
		writeErr(w, err)
		return
	}
	if err := persistPct(pct); err != nil {
		// The screen already changed; losing only the persistence is not worth
		// failing the request, but it does mean the level won't survive a wake.
		writeErr(w, err)
		return
	}
	writeJSON(w, Brightness{Pct: pct})
}

// applyPct scales the percentage against the panel's own max_brightness (255 on
// the RK3566 panels, but read it rather than assume).
func applyPct(pct int) error {
	max, err := readInt(filepath.Join(backlightDir, "max_brightness"))
	if err != nil {
		return fmt.Errorf("read max_brightness: %w", err)
	}
	raw := max * pct / 100
	if raw < 1 {
		raw = 1
	}
	p := filepath.Join(backlightDir, "brightness")
	if err := os.WriteFile(p, []byte(strconv.Itoa(raw)), 0o644); err != nil {
		return fmt.Errorf("write brightness: %w", err)
	}
	return nil
}

func readDesiredPct() int {
	b, err := os.ReadFile(stateFile)
	if err != nil {
		return defaultPct
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return defaultPct
	}
	return clampPct(n)
}

func persistPct(pct int) error {
	if err := os.MkdirAll(filepath.Dir(stateFile), 0o755); err != nil {
		return err
	}
	return os.WriteFile(stateFile, []byte(strconv.Itoa(pct)+"\n"), 0o644)
}

func clampPct(n int) int {
	if n < minPct {
		return minPct
	}
	if n > 100 {
		return 100
	}
	return n
}

func readInt(p string) (int, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(b)))
}
