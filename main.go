// hio-wifi-helper — the panel's privileged local API, behind a Unix socket.
//
// On the HIO pad the Chromium-kiosk UI lives in a Docker container, so it can
// reach neither NetworkManager nor /sys. This daemon runs as root on the host
// and exposes the few things the UI legitimately needs to touch. Caddy in the
// pad-ui container forwards to /run/hio-wifi.sock with the prefix stripped:
//
//	/api/wifi/*    → /scan /status /connect /disconnect   (nmcli, this file)
//	/api/display/* → /display/brightness                  (backlight, display.go)
//
// (The name is historical — wifi was the first consumer.)
//
// JSON response shapes match HomeInOne-pad/src/lib/{wifi,display}/client.ts.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Configurable seams. Defaults match production on the pad; tests override
// HIO_WIFI_SOCKET (run unprivileged) and HIO_NMCLI (point at a mock).
var (
	socketPath = envOr("HIO_WIFI_SOCKET", "/run/hio-wifi.sock")
	nmcliBin   = envOr("HIO_NMCLI", "nmcli")
)

type Network struct {
	SSID     string `json:"ssid"`
	Signal   int    `json:"signal"` // 0-100, nmcli's percentage
	Security string `json:"security,omitempty"`
}

type Status struct {
	Connected bool    `json:"connected"`
	SSID      *string `json:"ssid"`
	IP        *string `json:"ip"`
}

type ConnectReq struct {
	SSID     string `json:"ssid"`
	Password string `json:"password"`
}

func main() {
	_ = os.Remove(socketPath)
	l, err := net.Listen("unix", socketPath)
	if err != nil {
		log.Fatalf("listen %s: %v", socketPath, err)
	}
	if err := setSocketPerms(socketPath); err != nil {
		log.Printf("warn: socket perms: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /scan", scanHandler)
	mux.HandleFunc("GET /status", statusHandler)
	mux.HandleFunc("POST /connect", connectHandler)
	mux.HandleFunc("POST /disconnect", disconnectHandler)
	// Backlight — see display.go. Caddy maps /api/display/* onto the same socket.
	mux.HandleFunc("GET /display/brightness", brightnessGetHandler)
	mux.HandleFunc("POST /display/brightness", brightnessSetHandler)

	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}

	// Graceful shutdown so the socket file gets cleaned up on SIGTERM.
	go func() {
		sigs := make(chan os.Signal, 1)
		signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
		<-sigs
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		_ = os.Remove(socketPath)
	}()

	log.Printf("hio-wifi-helper listening on %s (nmcli=%s)", socketPath, nmcliBin)
	if err := srv.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("serve: %v", err)
	}
}

// 0660 root:docker — the Caddy container bind-mounts the socket and connects
// as a member of the docker group. If the docker group is absent (e.g. a dev
// box without Docker) we fall through with just chmod and warn.
func setSocketPerms(p string) error {
	if err := os.Chmod(p, 0o660); err != nil {
		return err
	}
	g, err := user.LookupGroup("docker")
	if err != nil {
		return nil
	}
	gid, _ := strconv.Atoi(g.Gid)
	return os.Chown(p, 0, gid)
}

func scanHandler(w http.ResponseWriter, _ *http.Request) {
	out, err := nmcli("-t", "-f", "SSID,SIGNAL,SECURITY", "device", "wifi", "list", "--rescan", "auto")
	if err != nil {
		writeErr(w, err)
		return
	}
	nets := []Network{}
	seen := map[string]bool{}
	for _, line := range nonEmptyLines(out) {
		f := splitTerse(line, 3)
		if len(f) < 3 || f[0] == "" || f[0] == "--" || seen[f[0]] {
			continue
		}
		seen[f[0]] = true
		sig, _ := strconv.Atoi(f[1])
		nets = append(nets, Network{SSID: f[0], Signal: sig, Security: f[2]})
	}
	writeJSON(w, nets)
}

func statusHandler(w http.ResponseWriter, _ *http.Request) {
	s, err := readStatus()
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, s)
}

func connectHandler(w http.ResponseWriter, r *http.Request) {
	var req ConnectReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	if req.SSID == "" {
		http.Error(w, "ssid required", http.StatusBadRequest)
		return
	}
	args := []string{"device", "wifi", "connect", req.SSID}
	if req.Password != "" {
		args = append(args, "password", req.Password)
	}
	if out, err := exec.Command(nmcliBin, args...).CombinedOutput(); err != nil {
		log.Printf("connect failed: %v: %s", err, out)
		http.Error(w, strings.TrimSpace(string(out)), http.StatusBadGateway)
		return
	}
	s, _ := readStatus()
	writeJSON(w, s)
}

func disconnectHandler(w http.ResponseWriter, _ *http.Request) {
	dev, err := wifiDevice()
	if err != nil {
		writeErr(w, err)
		return
	}
	if dev != "" {
		_ = exec.Command(nmcliBin, "device", "disconnect", dev).Run()
	}
	s, _ := readStatus()
	writeJSON(w, s)
}

func readStatus() (Status, error) {
	out, err := nmcli("-t", "-f", "TYPE,STATE,CONNECTION,DEVICE", "device", "status")
	if err != nil {
		return Status{}, err
	}
	for _, line := range nonEmptyLines(out) {
		f := splitTerse(line, 4)
		if len(f) < 4 || f[0] != "wifi" {
			continue
		}
		s := Status{Connected: f[1] == "connected"}
		if s.Connected {
			ssid := f[2]
			s.SSID = &ssid
			if ip, _ := readIP(f[3]); ip != "" {
				s.IP = &ip
			}
		}
		return s, nil
	}
	return Status{}, nil // no wifi device — treat as disconnected
}

func wifiDevice() (string, error) {
	out, err := nmcli("-t", "-f", "TYPE,DEVICE", "device", "status")
	if err != nil {
		return "", err
	}
	for _, line := range nonEmptyLines(out) {
		f := splitTerse(line, 2)
		if len(f) == 2 && f[0] == "wifi" {
			return f[1], nil
		}
	}
	return "", nil
}

func readIP(dev string) (string, error) {
	out, err := nmcli("-t", "-f", "IP4.ADDRESS", "device", "show", dev)
	if err != nil {
		return "", err
	}
	for _, line := range nonEmptyLines(out) {
		f := splitTerse(line, 2)
		if len(f) == 2 && strings.HasPrefix(f[0], "IP4.ADDRESS") && f[1] != "" {
			return strings.SplitN(f[1], "/", 2)[0], nil
		}
	}
	return "", nil
}

func nmcli(args ...string) (string, error) {
	out, err := exec.Command(nmcliBin, args...).Output()
	return string(out), err
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func nonEmptyLines(s string) []string {
	out := []string{}
	for _, l := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}

// nmcli -t escapes ':' in fields as `\:` and `\` as `\\`. Splits at most n
// parts; everything after the (n-1)-th unescaped colon stays in the last cell.
func splitTerse(line string, n int) []string {
	out := make([]string, 0, n)
	var cur strings.Builder
	for i := 0; i < len(line); i++ {
		c := line[i]
		if c == '\\' && i+1 < len(line) {
			cur.WriteByte(line[i+1])
			i++
			continue
		}
		if c == ':' && len(out) < n-1 {
			out = append(out, cur.String())
			cur.Reset()
			continue
		}
		cur.WriteByte(c)
	}
	out = append(out, cur.String())
	return out
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, err error) {
	log.Printf("error: %v", err)
	http.Error(w, err.Error(), http.StatusBadGateway)
}
