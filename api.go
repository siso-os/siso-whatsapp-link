package main

// The HTTP API Agent Base's node reads over the tailnet. Every route needs a bearer token; there is no
// anonymous route but /healthz (a liveness bit, no data). Responses carry message text to the app only;
// the access log records method, path shape, status and time, never a query string or a body.
//
// Two tokens. The API token (api-token) reaches every route. The agent token (agent-token) is for SISO agents and
// reaches only GET /health and, for the chats in WA_AGENT_CHATS, GET /chats/{jid}/messages and
// POST /chats/{jid}/send (text, or an image with a caption); everything else answers 403. Its sends (an image's
// caption too) are labelled "SISO agent: " and capped at 20 a day.
// Shaan, 16 Nov 2026: agents post only in the agent group, labelled, so no one mistakes them for him typing.

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	qrcode "github.com/skip2/go-qrcode"
)

type API struct {
	st      *Store
	link    Linker
	hub     *Hub
	cfg     Config
	started time.Time
	histMu  sync.Mutex
	histAt  map[string]time.Time
	soak    *Soak
}

func NewAPI(cfg Config, st *Store, link Linker, hub *Hub) *API {
	return &API{st: st, link: link, hub: hub, cfg: cfg, started: time.Now(), histAt: map[string]time.Time{}}
}

// agentScope is what the agent token may do on a route. A route not marked is API-token only.
type agentScope int

const (
	agentNone agentScope = iota // API token only
	agentAny                    // the agent token may call it (no chat in the path)
	agentChat                   // the agent token may call it for a chat in WA_AGENT_CHATS only
)

type role int

const (
	roleMain role = iota + 1
	roleAgent
)

type roleKey struct{}

func roleOf(r *http.Request) role { v, _ := r.Context().Value(roleKey{}).(role); return v }

func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	handle := func(pattern string, h http.HandlerFunc, scope agentScope) { mux.Handle(pattern, a.gate(scope, h)) }
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok\n")) })
	handle("GET /health", a.health, agentAny)
	handle("GET /qr", a.qr, agentNone)
	handle("POST /pair", a.pair, agentNone)
	handle("GET /chats", a.chats, agentNone)
	handle("GET /chats/{jid}/messages", a.messages, agentChat)
	handle("POST /chats/{jid}/read", a.read, agentNone)
	handle("POST /chats/{jid}/send", a.send, agentChat)
	handle("GET /media/{jid}/{id}/thumb", a.thumb, agentNone)
	handle("GET /media/{jid}/{id}", a.media, agentNone)
	handle("GET /search", a.search, agentNone)
	handle("GET /rolodex", a.rolodex, agentNone)
	handle("GET /events", a.events, agentNone)
	handle("GET /soak", a.soakRows, agentNone)
	return a.logged(a.auth(mux))
}

var errAgentScope = map[string]string{"error": "forbidden: the agent token reaches only /health and the agent chat"}

// gate runs after routing, so the chat it checks is exactly the one the handler will use (jidParam).
func (a *API) gate(scope agentScope, h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if roleOf(r) == roleAgent && (scope == agentNone || (scope == agentChat && !a.isAgentChat(jidParam(r)))) {
			writeJSON(w, 403, errAgentScope)
			return
		}
		h(w, r)
	})
}

func (a *API) isAgentChat(jid string) bool {
	if jid == "" {
		return false
	}
	for _, c := range a.cfg.AgentChats {
		if c == jid {
			return true
		}
	}
	return false
}

func (a *API) auth(mux *http.ServeMux) http.Handler {
	want := []byte("Bearer " + a.cfg.Token)
	wantAgent := []byte("Bearer " + a.cfg.AgentToken)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			mux.ServeHTTP(w, r)
			return
		}
		got := []byte(r.Header.Get("Authorization"))
		switch {
		case subtle.ConstantTimeCompare(got, want) == 1:
			r = r.WithContext(context.WithValue(r.Context(), roleKey{}, roleMain))
		case a.cfg.AgentToken != "" && subtle.ConstantTimeCompare(got, wantAgent) == 1:
			// No route, a wrong method, or a path the mux would redirect: 403 too, never a 404/405/301 hint.
			if _, pat := mux.Handler(r); pat == "" || path.Clean(r.URL.Path) != r.URL.Path {
				writeJSON(w, 403, errAgentScope)
				return
			}
			r = r.WithContext(context.WithValue(r.Context(), roleKey{}, roleAgent))
		default:
			writeJSON(w, 401, map[string]string{"error": "unauthorized"})
			return
		}
		mux.ServeHTTP(w, r)
	})
}

type statusWriter struct {
	http.ResponseWriter
	code int
}

func (s *statusWriter) WriteHeader(c int) { s.code = c; s.ResponseWriter.WriteHeader(c) }
func (s *statusWriter) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// pathShape hides chat ids in the log: /chats/<jid>/messages, never the number.
func pathShape(p string) string {
	parts := strings.Split(p, "/")
	for i, s := range parts {
		if strings.Contains(s, "@") || (i == 3 && strings.HasPrefix(p, "/media/")) {
			parts[i] = "<id>"
		}
	}
	return strings.Join(parts, "/")
}

func (a *API) logged(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t := time.Now()
		sw := &statusWriter{ResponseWriter: w, code: 200}
		next.ServeHTTP(sw, r)
		if r.URL.Path != "/healthz" && r.URL.Path != "/events" {
			log.Printf("http %s %s %d %dms", r.Method, pathShape(r.URL.Path), sw.code, time.Since(t).Milliseconds())
		}
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func intParam(r *http.Request, k string, def, max int) int {
	n, err := strconv.Atoi(r.URL.Query().Get(k))
	if err != nil || n <= 0 {
		return def
	}
	if n > max {
		return max
	}
	return n
}

func jidParam(r *http.Request) string {
	j, _ := url.PathUnescape(r.PathValue("jid"))
	return j
}

// rssMB is the process's resident memory as the OS sees it (what the watchdog and the soak graph use).
func rssMB() int {
	out, err := exec.Command("ps", "-o", "rss=", "-p", strconv.Itoa(os.Getpid())).Output()
	if kb, e := strconv.Atoi(strings.TrimSpace(string(out))); err == nil && e == nil {
		return kb >> 10
	}
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return int(m.Sys >> 20)
}

func (a *API) health(w http.ResponseWriter, r *http.Request) {
	s := a.link.Status()
	agentChats := a.cfg.AgentChats
	if agentChats == nil {
		agentChats = []string{}
	}
	writeJSON(w, 200, map[string]any{
		"link": s, "counts": a.st.Counts(), "uptimeSec": int(time.Since(a.started).Seconds()), "rssMB": rssMB(),
		"sendEnabled": a.cfg.SendEnabled, "readReceipts": a.cfg.ReadReceipts, "version": version,
		"agentChats": agentChats, "agentSendsDay": a.st.AgentSendsSince(time.Now().Add(-24 * time.Hour)),
		"agentSendPerDay": agentSendPerDay,
	})
}

// soakRows serves the flight recorder (numbers and states only) from ?since=<unix>, default the last 25 hours.
func (a *API) soakRows(w http.ResponseWriter, r *http.Request) {
	since := time.Now().Add(-25 * time.Hour).Unix()
	if v, err := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64); err == nil {
		since = v
	}
	rows := []SoakRow{}
	if a.soak != nil {
		rows = append(rows, a.soak.Rows(since)...)
	}
	writeJSON(w, 200, map[string]any{"rows": rows, "now": time.Now().Unix()})
}

func (a *API) qr(w http.ResponseWriter, r *http.Request) {
	code, active := a.link.QR()
	st := a.link.Status()
	out := map[string]any{"state": st.State, "active": active}
	if code != "" {
		png, err := qrcode.Encode(code, qrcode.Medium, 320)
		if err == nil {
			out["png"] = "data:image/png;base64," + b64(png)
		}
	}
	writeJSON(w, 200, out)
}

func (a *API) pair(w http.ResponseWriter, r *http.Request) {
	if err := a.link.StartPairing(); err != nil {
		writeJSON(w, 502, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 202, map[string]string{"state": a.link.Status().State})
}

func (a *API) chats(w http.ResponseWriter, r *http.Request) {
	cs, err := a.st.Chats(intParam(r, "limit", 200, 2000), intParam(r, "offset", 0, 1<<30), r.URL.Query().Get("archived") == "1")
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	for i := range cs {
		cs[i].Name = a.link.DisplayName(cs[i].JID, cs[i].Name)
	}
	writeJSON(w, 200, map[string]any{"chats": cs, "counts": a.st.Counts()})
}

func (a *API) messages(w http.ResponseWriter, r *http.Request) {
	jid := jidParam(r)
	before, _ := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64)
	limit := intParam(r, "limit", 60, 500)
	ms, err := a.st.Messages(jid, before, limit)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	for i := range ms {
		if !ms[i].FromMe && ms[i].Sender != "" {
			ms[i].SenderName = a.link.DisplayName(ms[i].Sender, ms[i].SenderName)
		}
	}
	// Scrolled to the top of what we hold: ask the phone for older ones (at most once per chat per 10 minutes).
	asked := false
	if before > 0 && len(ms) < limit {
		a.histMu.Lock()
		if time.Since(a.histAt[jid]) > 10*time.Minute {
			a.histAt[jid] = time.Now()
			asked = true
		}
		a.histMu.Unlock()
		if asked {
			if o, ok := a.st.Oldest(jid); ok {
				ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
				if err := a.link.RequestHistory(ctx, o); err != nil {
					asked = false
				}
				cancel()
			}
		}
	}
	c, _ := a.st.Chat(jid)
	c.Name = a.link.DisplayName(jid, c.Name)
	writeJSON(w, 200, map[string]any{"chat": c, "messages": ms, "olderRequested": asked})
}

func (a *API) read(w http.ResponseWriter, r *http.Request) {
	jid := jidParam(r)
	ms, _ := a.st.Messages(jid, 0, 30)
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	if err := a.link.MarkRead(ctx, jid, ms); err != nil {
		writeJSON(w, 502, map[string]string{"error": err.Error()})
		return
	}
	a.hub.Publish(Event{Type: "chat", Chat: jid})
	writeJSON(w, 200, map[string]bool{"ok": true, "receipts": a.cfg.ReadReceipts})
}

// Human pace: one send at a time, at least MinGap apart, and hourly and daily ceilings. No bulk, ever.
// The agent token has its own daily ceiling on top, and its text always carries the label.
const (
	sendMinGap      = 4 * time.Second
	sendPerHour     = 30
	sendPerDay      = 150
	sendMaxChars    = 4000
	agentSendPerDay = 20
	agentPrefix     = "SISO agent: "
	sendBodyMax     = imageMaxBytes*4/3 + 64<<10 // a 5 MB image in base64, plus its caption
)

var sendMu sync.Mutex

// agentLabel makes sure an agent's message says it is an agent's, once.
func agentLabel(text string) string {
	text = strings.TrimLeft(text, " \t\r\n")
	if strings.HasPrefix(text, agentPrefix) {
		return text
	}
	return agentPrefix + text
}

// send posts one message: {"text"} or {"image": base64 PNG/JPEG, "caption"}. Both go through the same gate, label,
// ceilings and ledger; an agent's caption carries the label as its text does.
func (a *API) send(w http.ResponseWriter, r *http.Request) {
	if !a.cfg.SendEnabled {
		writeJSON(w, 403, map[string]string{"error": ErrSendDisabled.Error()})
		return
	}
	agent := roleOf(r) == roleAgent
	var body struct {
		Text    string `json:"text"`
		Image   string `json:"image"`
		Caption string `json:"caption"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, sendBodyMax)).Decode(&body); err != nil {
		code, msg := 400, "body must be JSON: {\"text\"} or {\"image\", \"caption\"}"
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			code, msg = 413, errImageLarge.Error()
		}
		writeJSON(w, code, map[string]string{"error": msg})
		return
	}
	var img *OutImage
	text := body.Text
	if body.Image != "" {
		if body.Text != "" {
			writeJSON(w, 400, map[string]string{"error": "send text or an image with a caption, not both"})
			return
		}
		raw, err := base64.StdEncoding.DecodeString(body.Image)
		if err != nil {
			writeJSON(w, 400, map[string]string{"error": "image must be base64 (standard alphabet)"})
			return
		}
		checked, err := checkImage(raw)
		if err != nil {
			code := 415
			if errors.Is(err, errImageLarge) || errors.Is(err, errImagePix) {
				code = 413
			}
			writeJSON(w, code, map[string]string{"error": err.Error()})
			return
		}
		img, text = &checked, body.Caption
		if agent {
			text = strings.TrimRight(agentLabel(text), " ") // an image with no caption still says who sent it
		}
		if utf8.RuneCountInString(text) > imageCaptionMax {
			writeJSON(w, 400, map[string]string{"error": fmt.Sprintf("caption too long (max %d characters)", imageCaptionMax)})
			return
		}
	} else {
		bare := text
		if agent {
			text = agentLabel(text)
			bare = strings.TrimPrefix(text, agentPrefix) // the label alone is not a message
		}
		if strings.TrimSpace(bare) == "" || len(text) > sendMaxChars {
			writeJSON(w, 400, map[string]string{"error": "text required (max 4000 chars)"})
			return
		}
	}
	jid := jidParam(r)
	if _, ok := a.st.Chat(jid); !ok {
		writeJSON(w, 404, map[string]string{"error": "unknown chat: sends go only to chats he already has"})
		return
	}
	sendMu.Lock()
	defer sendMu.Unlock()
	hour, last := a.st.SendsSince(time.Now().Add(-time.Hour))
	day, _ := a.st.SendsSince(time.Now().Add(-24 * time.Hour))
	if wait := time.Until(time.Unix(last, 0).Add(sendMinGap)); wait > 0 {
		writeJSON(w, 429, map[string]any{"error": "too fast", "retryAfterMs": wait.Milliseconds()})
		return
	}
	if hour >= sendPerHour || day >= sendPerDay {
		writeJSON(w, 429, map[string]string{"error": "send ceiling reached"})
		return
	}
	if agent && a.st.AgentSendsSince(time.Now().Add(-24*time.Hour)) >= agentSendPerDay {
		writeJSON(w, 429, map[string]string{"error": "agent send ceiling reached (20 a day)"})
		return
	}
	var id string
	var err error
	if img != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second) // the upload comes first
		defer cancel()
		id, err = a.link.SendImage(ctx, jid, *img, text)
	} else {
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()
		id, err = a.link.Send(ctx, jid, text)
	}
	a.st.LogSend(jid, id, err == nil, agent)
	if err != nil {
		code := 502
		if errors.Is(err, ErrSendDisabled) {
			code = 403
		}
		writeJSON(w, code, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]string{"id": id})
}

func (a *API) thumb(w http.ResponseWriter, r *http.Request) {
	raw := a.st.Raw(jidParam(r), r.PathValue("id"))
	t := a.link.Thumbnail(raw)
	if len(t) == 0 {
		http.Error(w, "no thumbnail", 404)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "private, max-age=86400")
	w.Write(t)
}

func (a *API) media(w http.ResponseWriter, r *http.Request) {
	raw := a.st.Raw(jidParam(r), r.PathValue("id"))
	if raw == nil {
		http.Error(w, "not found", 404)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	b, mime, err := a.link.Download(ctx, raw)
	if err != nil {
		http.Error(w, "download failed", 502)
		return
	}
	if mime == "" {
		mime = "application/octet-stream"
	}
	w.Header().Set("Content-Type", mime)
	w.Header().Set("Cache-Control", "private, max-age=86400")
	w.Header().Set("Content-Length", fmt.Sprint(len(b)))
	w.Write(b)
}

func (a *API) search(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	ms, err := a.st.Search(q, r.URL.Query().Get("chat"), intParam(r, "limit", 50, 500))
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	// Chat-name matches too, so "search" finds a person before any of their words.
	cs, _ := a.st.Chats(5000, 0, true)
	lq := strings.ToLower(strings.TrimSpace(q))
	names := []Chat{}
	for _, c := range cs {
		c.Name = a.link.DisplayName(c.JID, c.Name)
		if lq != "" && strings.Contains(strings.ToLower(c.Name), lq) {
			names = append(names, c)
			if len(names) >= 20 {
				break
			}
		}
	}
	writeJSON(w, 200, map[string]any{"chats": names, "messages": ms})
}

func (a *API) rolodex(w http.ResponseWriter, r *http.Request) {
	rows, err := a.st.Rolodex()
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	for i := range rows {
		rows[i].Name = a.link.DisplayName(rows[i].JID, rows[i].Name)
	}
	writeJSON(w, 200, map[string]any{"generatedAt": time.Now().Unix(), "people": rows})
}

// events is a server-sent stream of "something changed" pings (chat ids, no content); the app refetches.
func (a *API) events(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "no streaming", 500)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	ch := a.hub.Subscribe()
	defer a.hub.Unsubscribe(ch)
	fmt.Fprintf(w, "event: hello\ndata: {}\n\n")
	fl.Flush()
	tick := time.NewTicker(25 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-tick.C:
			fmt.Fprintf(w, ": ping\n\n")
			fl.Flush()
		case e := <-ch:
			b, _ := json.Marshal(e)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", e.Type, b)
			fl.Flush()
		}
	}
}
