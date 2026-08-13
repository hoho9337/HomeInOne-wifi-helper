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
//
// /display/hold is how an app asks the screen to STAY on (an intercom call:
// the resident is talking, not touching, so the X idle clock keeps running and
// would blank the panel mid-call). It only records intent — hio-display-idle.sh
// remains the sole writer of the backlight. That matters: the backlight has two
// independent gates, `brightness` (PWM duty) and `bl_power` (FB blank state),
// and writing brightness alone while bl_power=4 leaves the PWM at duty 0. A
// second writer here would have to know that; the idle loop already does.

package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

var (
	backlightDir = envOr("HIO_BACKLIGHT_DIR", "/sys/class/backlight/backlight")
	stateFile    = envOr("HIO_BRIGHTNESS_STATE", "/var/lib/hio/brightness")
	// On tmpfs on purpose: a hold must never survive a reboot.
	holdFile = envOr("HIO_DISPLAY_HOLD", "/run/hio/display-hold")
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

// A hold is a lease the UI renews, never a latch it sets once: if the kiosk tab
// crashes or the call code wedges, the screen goes back to sleep within maxHoldSec
// instead of staying lit (and hot) forever.
const maxHoldSec = 300

type Hold struct {
	Sec int `json:"sec"` // 0-300 — seconds from now; 0 releases immediately
}

func displayHoldGetHandler(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, Hold{Sec: readHoldRemaining()})
}

func displayHoldSetHandler(w http.ResponseWriter, r *http.Request) {
	var req Hold
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	sec := clampHold(req.Sec)
	if err := writeHold(sec); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, Hold{Sec: sec})
}

// writeHold stores an ABSOLUTE expiry (unix seconds) rather than a duration, so
// the reader needs no state of its own and a stale file is self-invalidating.
// temp+rename because hio-display-idle.sh polls this once a second: a torn read
// would parse as garbage → "expired" → the screen blanks mid-call.
func writeHold(sec int) error {
	if err := os.MkdirAll(filepath.Dir(holdFile), 0o755); err != nil {
		return fmt.Errorf("mkdir hold dir: %w", err)
	}
	var exp int64 // 0 = released; any past value reads as expired
	if sec > 0 {
		exp = time.Now().Unix() + int64(sec)
	}
	tmp := holdFile + ".tmp"
	if err := os.WriteFile(tmp, []byte(strconv.FormatInt(exp, 10)+"\n"), 0o644); err != nil {
		return fmt.Errorf("write hold: %w", err)
	}
	if err := os.Rename(tmp, holdFile); err != nil {
		return fmt.Errorf("commit hold: %w", err)
	}
	return nil
}

func readHoldRemaining() int {
	b, err := os.ReadFile(holdFile)
	if err != nil {
		return 0
	}
	exp, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return 0
	}
	if rem := exp - time.Now().Unix(); rem > 0 {
		return int(rem)
	}
	return 0
}

func clampHold(n int) int {
	if n < 0 {
		return 0
	}
	if n > maxHoldSec {
		return maxHoldSec
	}
	return n
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
