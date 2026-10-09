package main

// siso-whatsapp-link: Shaan's WhatsApp, linked as one companion device on an always-on box (the Mac mini), served
// to Agent Base over the tailnet. See AGENTS.md and docs/decision/memo.html for why it is shaped this way.

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"syscall"
	"time"
)

const version = "0.2.0"

type Config struct {
	DataDir      string
	Listen       []string
	Token        string
	SendEnabled  bool
	ReadReceipts bool
	MemLimitMB   int
	MaxRSSMB     int
	MaxDownFor   time.Duration
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// secret returns a 32-byte hex secret: from the macOS Keychain when available (service name given), otherwise from a
// 0600 file in the data dir. Created on first run. The value is never printed.
func secret(dataDir, service, file string) (string, error) {
	if service != "" && runtime.GOOS == "darwin" && os.Getenv("WA_NO_KEYCHAIN") == "" {
		out, err := exec.Command("security", "find-generic-password", "-s", service, "-a", "siso", "-w").Output()
		if err == nil && len(strings.TrimSpace(string(out))) == 64 {
			return strings.TrimSpace(string(out)), nil
		}
		v := newHex()
		if err := exec.Command("security", "add-generic-password", "-U", "-s", service, "-a", "siso", "-w", v).Run(); err == nil {
			if chk, err := exec.Command("security", "find-generic-password", "-s", service, "-a", "siso", "-w").Output(); err == nil && strings.TrimSpace(string(chk)) == v {
				return v, nil
			}
		}
		log.Printf("secret %s: keychain unavailable, using a 0600 file", service)
	}
	p := filepath.Join(dataDir, file)
	if b, err := os.ReadFile(p); err == nil && len(strings.TrimSpace(string(b))) == 64 {
		return strings.TrimSpace(string(b)), nil
	}
	v := newHex()
	return v, os.WriteFile(p, []byte(v+"\n"), 0o600)
}

func newHex() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

type Event struct {
	Type string `json:"type"`
	Chat string `json:"chat,omitempty"`
}

type Hub struct {
	mu   sync.Mutex
	subs map[chan Event]struct{}
}

func NewHub() *Hub { return &Hub{subs: map[chan Event]struct{}{}} }

func (h *Hub) Subscribe() chan Event {
	ch := make(chan Event, 64)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	return ch
}

func (h *Hub) Unsubscribe(ch chan Event) {
	h.mu.Lock()
	delete(h.subs, ch)
	h.mu.Unlock()
}

func (h *Hub) Publish(e Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- e:
		default: // a slow reader misses a ping and refetches on the next one
		}
	}
}

func main() {
	home, _ := os.UserHomeDir()
	dataDir := flag.String("data", env("WA_DATA_DIR", filepath.Join(home, "Library/Application Support/siso-whatsapp-link")), "data directory")
	listen := flag.String("listen", env("WA_LISTEN", "127.0.0.1:5480"), "comma-separated listen addresses")
	printToken := flag.Bool("print-token-path", false, "print where the API token lives, then exit")
	flag.Parse()

	cfg := Config{DataDir: *dataDir, Listen: strings.Split(*listen, ","),
		SendEnabled: env("WA_SEND", "0") == "1", ReadReceipts: env("WA_READ_RECEIPTS", "0") == "1",
		MemLimitMB: 200, MaxRSSMB: 600, MaxDownFor: 15 * time.Minute}
	syscall.Umask(0o077) // the session and the store are his account: owner-only, always
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		log.Fatal(err)
	}
	os.Chmod(cfg.DataDir, 0o700)
	if *printToken {
		log.Printf("token: %s/api-token (0600)", cfg.DataDir)
		return
	}
	debug.SetMemoryLimit(int64(cfg.MemLimitMB) << 20)

	// The API token is a network credential the laptop must copy over ssh, which cannot read the login Keychain, so it
	// lives in a 0600 file. The body key (what protects the store at rest) stays in the Keychain.
	tok, err := secret(cfg.DataDir, "", "api-token")
	if err != nil {
		log.Fatal("token: ", err)
	}
	cfg.Token = tok
	keyHex, err := secret(cfg.DataDir, "siso-whatsapp-link-body-key", "body-key")
	if err != nil {
		log.Fatal("body key: ", err)
	}
	key, _ := hex.DecodeString(keyHex)

	st, err := OpenStore(filepath.Join(cfg.DataDir, "messages.db"), key)
	if err != nil {
		log.Fatal("store: ", err)
	}
	hub := NewHub()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	link, err := NewLink(ctx, cfg, st, hub)
	if err != nil {
		log.Fatal("link: ", err)
	}
	soak := NewSoak(cfg.DataDir, time.Now())
	link.soak = soak
	if err := link.Start(); err != nil {
		log.Printf("connect: %v (auto-reconnect continues)", err)
	}

	api := NewAPI(cfg, st, link, hub)
	api.soak = soak
	soakDone := make(chan struct{})
	go func() { soak.Run(ctx, link, st, hub, time.Minute); close(soakDone) }()
	srv := &http.Server{Handler: api.Handler(), ReadHeaderTimeout: 10 * time.Second}
	for _, addr := range cfg.Listen {
		addr = strings.TrimSpace(addr)
		go func() {
			for {
				ln, err := net.Listen("tcp", addr)
				if err != nil {
					// The tailnet address may come up after us at boot; keep trying rather than die.
					log.Printf("listen %s: %v (retrying)", addr, err)
					time.Sleep(15 * time.Second)
					continue
				}
				log.Printf("listening on %s", addr)
				if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
					log.Printf("serve %s: %v", addr, err)
				}
				return
			}
		}()
	}
	go watchdog(ctx, cfg, link, stop)
	log.Printf("siso-whatsapp-link %s up (send=%v receipts=%v)", version, cfg.SendEnabled, cfg.ReadReceipts)
	<-ctx.Done()
	log.Printf("shutting down")
	select { // let the recorder write its stop row before the store closes
	case <-soakDone:
	case <-time.After(3 * time.Second):
	}
	sctx, c := context.WithTimeout(context.Background(), 5*time.Second)
	srv.Shutdown(sctx)
	c()
	link.Close()
	st.Close()
	if exitCode != 0 {
		os.Exit(exitCode)
	}
}

var exitCode int

// watchdog exits (launchd restarts us) when memory runs away or a linked session stays down too long: a fresh
// process is the most reliable reconnect there is.
func watchdog(ctx context.Context, cfg Config, link *Link, stop context.CancelFunc) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	var downSince time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if mb := rssMB(); mb > cfg.MaxRSSMB {
			log.Printf("watchdog: memory %d MB over %d, restarting", mb, cfg.MaxRSSMB)
			exitCode = 3
			stop()
			return
		}
		s := link.Status()
		if s.LoggedIn && !s.Connected && s.State != "logged_out" && s.State != "banned" {
			if downSince.IsZero() {
				downSince = time.Now()
			} else if time.Since(downSince) > cfg.MaxDownFor {
				log.Printf("watchdog: down for %s, restarting", time.Since(downSince).Round(time.Second))
				exitCode = 4
				stop()
				return
			}
		} else {
			downSince = time.Time{}
		}
	}
}
