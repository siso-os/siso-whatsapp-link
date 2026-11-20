package main

// Tests run against a fake link and fixture chats only: no WhatsApp, no real names or numbers.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeLink struct {
	sent   []string
	texts  []string
	images []fakeImage
	reads  int
	status LinkStatus
	mu     sync.Mutex
}

type fakeImage struct {
	chat, caption string
	img           OutImage
}

func (f *fakeLink) Status() LinkStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.status
}

func (f *fakeLink) setStatus(s LinkStatus) {
	f.mu.Lock()
	f.status = s
	f.mu.Unlock()
}
func (f *fakeLink) QR() (string, bool)  { return "", false }
func (f *fakeLink) StartPairing() error { return nil }
func (f *fakeLink) Download(ctx context.Context, raw []byte) ([]byte, string, error) {
	return nil, "", errors.New("no")
}
func (f *fakeLink) Thumbnail(raw []byte) []byte { return nil }
func (f *fakeLink) MarkRead(ctx context.Context, chat string, msgs []Message) error {
	f.reads++
	return nil
}
func (f *fakeLink) Send(ctx context.Context, chat, text string) (string, error) {
	f.sent = append(f.sent, chat)
	f.texts = append(f.texts, text)
	return "SENT1", nil
}
func (f *fakeLink) SendImage(ctx context.Context, chat string, img OutImage, caption string) (string, error) {
	f.sent = append(f.sent, chat)
	f.images = append(f.images, fakeImage{chat, caption, img})
	return "IMG1", nil
}
func (f *fakeLink) RequestHistory(ctx context.Context, oldest Message) error { return nil }
func (f *fakeLink) DisplayName(jid, stored string) string                    { return stored }

const (
	alice = "100000000001@s.whatsapp.net"
	bob   = "100000000002@s.whatsapp.net"
	grp   = "120000000000000001@g.us"
	grp2  = "120000000000000002@g.us"
	tok   = "test-token"
	atok  = "test-agent-token"
)

func setup(t *testing.T, send bool) (*Store, *fakeLink, http.Handler, string) {
	t.Helper()
	return setupCfg(t, Config{Token: tok, SendEnabled: send})
}

func setupCfg(t *testing.T, cfg Config) (*Store, *fakeLink, http.Handler, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := OpenStore(filepath.Join(dir, "messages.db"), bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	fl := &fakeLink{status: LinkStatus{State: "connected"}}
	return st, fl, NewAPI(cfg, st, fl, NewHub()).Handler(), dir
}

func seed(t *testing.T, st *Store) {
	t.Helper()
	st.UpsertChat(Chat{JID: alice, Name: "Fixture Alice"})
	st.UpsertChat(Chat{JID: grp, Name: "Fixture Group", IsGroup: true})
	put := func(m Message, live bool) {
		if _, err := st.PutMessage(m, live); err != nil {
			t.Fatal(err)
		}
	}
	put(Message{ChatJID: alice, ID: "a1", Sender: alice, TS: 100, Kind: "text", Text: "hello from the fixture"}, false)
	put(Message{ChatJID: alice, ID: "a2", FromMe: true, TS: 200, Kind: "text", Text: "a reply"}, false)
	put(Message{ChatJID: grp, ID: "g1", Sender: bob, SenderName: "Bob", TS: 300, Kind: "image", Text: "a caption", HasThumb: true}, true)
	put(Message{ChatJID: grp, ID: "g2", Sender: bob, TS: 400, Kind: "text", Text: "Banana bread recipe"}, true)
}

func do(h http.Handler, method, path, auth string, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if auth != "" {
		r.Header.Set("Authorization", "Bearer "+auth)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestAuth(t *testing.T) {
	_, _, h, _ := setup(t, false)
	for _, p := range []string{"/health", "/chats", "/rolodex", "/search?q=x", "/chats/" + alice + "/messages"} {
		if w := do(h, "GET", p, "", ""); w.Code != 401 {
			t.Errorf("%s without token: %d", p, w.Code)
		}
		if w := do(h, "GET", p, "wrong", ""); w.Code != 401 {
			t.Errorf("%s wrong token: %d", p, w.Code)
		}
	}
	if w := do(h, "GET", "/healthz", "", ""); w.Code != 200 {
		t.Errorf("healthz: %d", w.Code)
	}
}

func TestChatsUnreadAndPreview(t *testing.T) {
	st, _, h, _ := setup(t, false)
	seed(t, st)
	w := do(h, "GET", "/chats", tok, "")
	var out struct {
		Chats  []Chat
		Counts Counts
	}
	json.Unmarshal(w.Body.Bytes(), &out)
	if len(out.Chats) != 2 || out.Chats[0].JID != grp {
		t.Fatalf("want group first (newest): %+v", out.Chats)
	}
	if out.Chats[0].Unread != 2 || out.Chats[1].Unread != 0 {
		t.Errorf("unread: live incoming counts, history does not: %d %d", out.Chats[0].Unread, out.Chats[1].Unread)
	}
	if out.Chats[0].Preview != "Banana bread recipe" || out.Chats[1].Preview != "a reply" || !out.Chats[1].PreviewFrom {
		t.Errorf("preview: %+v", out.Chats)
	}
	if out.Counts.Chats != 2 || out.Counts.Groups != 1 || out.Counts.Messages != 4 || out.Counts.Unread != 1 {
		t.Errorf("counts: %+v", out.Counts)
	}
	// A message he sends from his phone clears the count, as WhatsApp does.
	st.PutMessage(Message{ChatJID: grp, ID: "g3", FromMe: true, TS: 500, Kind: "text", Text: "ok"}, true)
	c, _ := st.Chat(grp)
	if c.Unread != 0 {
		t.Errorf("own message should clear unread, got %d", c.Unread)
	}
	// Duplicate delivery does not double count.
	if ok, _ := st.PutMessage(Message{ChatJID: grp, ID: "g3", FromMe: true, TS: 500, Kind: "text"}, true); ok {
		t.Error("duplicate stored twice")
	}
}

func TestMessagesPaging(t *testing.T) {
	st, _, h, _ := setup(t, false)
	for i := 0; i < 10; i++ {
		st.PutMessage(Message{ChatJID: alice, ID: string(rune('a' + i)), TS: int64(100 + i), Kind: "text", Text: "m"}, false)
	}
	w := do(h, "GET", "/chats/"+alice+"/messages?limit=4", tok, "")
	var out struct{ Messages []Message }
	json.Unmarshal(w.Body.Bytes(), &out)
	if len(out.Messages) != 4 || out.Messages[0].TS != 106 || out.Messages[3].TS != 109 {
		t.Fatalf("newest page oldest-first: %+v", out.Messages)
	}
	w = do(h, "GET", "/chats/"+alice+"/messages?limit=4&before=106", tok, "")
	json.Unmarshal(w.Body.Bytes(), &out)
	if len(out.Messages) != 4 || out.Messages[3].TS != 105 {
		t.Fatalf("older page: %+v", out.Messages)
	}
}

func TestSearchFindsTextAndNames(t *testing.T) {
	st, _, h, _ := setup(t, false)
	seed(t, st)
	w := do(h, "GET", "/search?q=banana", tok, "")
	var out struct {
		Chats    []Chat
		Messages []Message
	}
	json.Unmarshal(w.Body.Bytes(), &out)
	if len(out.Messages) != 1 || out.Messages[0].ID != "g2" {
		t.Errorf("text search: %+v", out.Messages)
	}
	w = do(h, "GET", "/search?q=alice", tok, "")
	json.Unmarshal(w.Body.Bytes(), &out)
	if len(out.Chats) != 1 || out.Chats[0].JID != alice {
		t.Errorf("name search: %+v", out.Chats)
	}
}

func TestRolodexHasNoBodies(t *testing.T) {
	st, _, h, _ := setup(t, false)
	seed(t, st)
	w := do(h, "GET", "/rolodex", tok, "")
	body := w.Body.String()
	for _, s := range []string{"hello from the fixture", "a reply", "Banana", "caption"} {
		if strings.Contains(body, s) {
			t.Errorf("rolodex leaked message text %q", s)
		}
	}
	var out struct{ People []RolodexRow }
	json.Unmarshal(w.Body.Bytes(), &out)
	var a RolodexRow
	for _, p := range out.People {
		if p.JID == alice {
			a = p
		}
	}
	if a.Messages != 2 || a.Sent != 1 || a.LastIn != 100 || a.LastOut != 200 || a.Phone != "+100000000001" {
		t.Errorf("rolodex row: %+v", a)
	}
}

func TestBodiesSealedAtRest(t *testing.T) {
	st, _, _, dir := setup(t, false)
	seed(t, st)
	st.db.Exec("PRAGMA wal_checkpoint(TRUNCATE)")
	for _, f := range []string{"messages.db", "messages.db-wal"} {
		b, _ := os.ReadFile(filepath.Join(dir, f))
		if bytes.Contains(b, []byte("hello from the fixture")) || bytes.Contains(b, []byte("Banana bread")) {
			t.Errorf("%s holds plaintext message text", f)
		}
	}
	// And a wrong key reads nothing back.
	st2, _ := OpenStore(filepath.Join(dir, "messages.db"), bytes.Repeat([]byte{9}, 32))
	ms, _ := st2.Messages(alice, 0, 10)
	if len(ms) != 2 || ms[0].Text != "" {
		t.Errorf("wrong key should open rows but not text: %+v", ms)
	}
	st2.Close()
}

func TestSendDisabledByDefault(t *testing.T) {
	st, fl, h, _ := setup(t, false)
	seed(t, st)
	w := do(h, "POST", "/chats/"+alice+"/send", tok, `{"text":"hi"}`)
	if w.Code != 403 || len(fl.sent) != 0 {
		t.Fatalf("send must be off by default: %d sent=%d", w.Code, len(fl.sent))
	}
}

func TestSendGuards(t *testing.T) {
	st, fl, h, _ := setup(t, true)
	seed(t, st)
	if w := do(h, "POST", "/chats/"+bob+"/send", tok, `{"text":"hi"}`); w.Code != 404 {
		t.Errorf("unknown chat: %d", w.Code)
	}
	if w := do(h, "POST", "/chats/"+alice+"/send", tok, `{"text":"  "}`); w.Code != 400 {
		t.Errorf("empty text: %d", w.Code)
	}
	if w := do(h, "POST", "/chats/"+alice+"/send", tok, `{"text":"hi"}`); w.Code != 200 {
		t.Fatalf("first send: %d %s", w.Code, w.Body)
	}
	if w := do(h, "POST", "/chats/"+alice+"/send", tok, `{"text":"again"}`); w.Code != 429 {
		t.Errorf("second send inside the gap: %d", w.Code)
	}
	if len(fl.sent) != 1 {
		t.Errorf("sent %d, want 1", len(fl.sent))
	}
	// Hourly ceiling.
	for i := 0; i < sendPerHour; i++ {
		st.db.Exec(`INSERT INTO sends (ts,chat,id,ok) VALUES (?,?,?,1)`, time.Now().Add(-10*time.Minute).Unix(), alice, "x")
	}
	st.db.Exec(`UPDATE sends SET ts=ts-60`)
	if w := do(h, "POST", "/chats/"+alice+"/send", tok, `{"text":"hi"}`); w.Code != 429 {
		t.Errorf("hour ceiling: %d", w.Code)
	}
}

func TestReadClearsLocally(t *testing.T) {
	st, fl, h, _ := setup(t, false)
	seed(t, st)
	if w := do(h, "POST", "/chats/"+grp+"/read", tok, ""); w.Code != 200 {
		t.Fatalf("read: %d", w.Code)
	}
	if fl.reads != 1 {
		t.Errorf("link MarkRead not called")
	}
}

func TestLogShapeHidesIDs(t *testing.T) {
	if got := pathShape("/chats/" + alice + "/messages"); strings.Contains(got, "1000") {
		t.Errorf("log path leaks the number: %s", got)
	}
	if got := pathShape("/media/" + grp + "/ABCDEF/thumb"); strings.Contains(got, "ABCDEF") || strings.Contains(got, "1200") {
		t.Errorf("media path leaks ids: %s", got)
	}
}

// The agent token: /health, and read + send in WA_AGENT_CHATS only. Everything else is 403, sends carry the label,
// and it has its own 20-a-day ceiling. The API token keeps every route.

func setupAgent(t *testing.T, send bool, chats ...string) (*Store, *fakeLink, http.Handler) {
	t.Helper()
	st, fl, h, _ := setupCfg(t, Config{Token: tok, AgentToken: atok, AgentChats: chats, SendEnabled: send})
	seed(t, st)
	st.UpsertChat(Chat{JID: grp2, Name: "Fixture Agent Group", IsGroup: true})
	st.PutMessage(Message{ChatJID: grp2, ID: "h1", Sender: bob, TS: 600, Kind: "text", Text: "a reply from the fixture"}, true)
	return st, fl, h
}

func TestAgentTokenRefusedOutsideItsChat(t *testing.T) {
	_, fl, h := setupAgent(t, true, grp2)
	for _, c := range []struct{ method, path, body string }{
		{"GET", "/chats", ""},
		{"GET", "/chats?archived=1", ""},
		{"GET", "/search?q=banana", ""},
		{"GET", "/search?q=x&chat=" + grp2, ""},
		{"GET", "/rolodex", ""},
		{"GET", "/media/" + grp + "/g1", ""},
		{"GET", "/media/" + grp2 + "/h1", ""},
		{"GET", "/media/" + grp2 + "/h1/thumb", ""},
		{"GET", "/events", ""},
		{"GET", "/soak", ""},
		{"GET", "/qr", ""},
		{"POST", "/pair", ""},
		{"GET", "/chats/" + alice + "/messages", ""},
		{"GET", "/chats/" + grp + "/messages", ""},
		{"POST", "/chats/" + alice + "/send", `{"text":"hi"}`},
		{"POST", "/chats/" + grp + "/send", `{"text":"hi"}`},
		{"POST", "/chats/0@s.whatsapp.net/send", `{"text":"x"}`},
		{"POST", "/chats/120000000000000002%2540g.us.x/send", `{"text":"x"}`},
		{"POST", "/chats/" + grp2 + "/read", ""},
		{"GET", "/chats/" + grp2 + "/send", ""}, // wrong method
		{"POST", "/chats/" + grp2 + "/messages", ""},
		{"GET", "/chats/" + grp2 + "/messages/", ""}, // would redirect
		{"GET", "/chats//" + grp2 + "/messages", ""},
		{"GET", "/health/", ""},
		{"GET", "/nope", ""},
	} {
		if w := do(h, c.method, c.path, atok, c.body); w.Code != 403 {
			t.Errorf("agent %s %s: %d, want 403", c.method, c.path, w.Code)
		} else if strings.Contains(w.Body.String(), "fixture") || strings.Contains(w.Body.String(), "Banana") {
			t.Errorf("agent %s %s: 403 body leaked data: %s", c.method, c.path, w.Body)
		}
	}
	if len(fl.sent) != 0 {
		t.Errorf("agent token sent %d messages outside its chat", len(fl.sent))
	}
}

func TestAgentTokenNoChatsBound(t *testing.T) {
	_, fl, h := setupAgent(t, true) // WA_AGENT_CHATS empty: no chat is the agent's
	if w := do(h, "POST", "/chats/"+grp2+"/send", atok, `{"text":"x"}`); w.Code != 403 {
		t.Errorf("send with no agent chat bound: %d", w.Code)
	}
	if w := do(h, "GET", "/chats/"+grp2+"/messages", atok, ""); w.Code != 403 {
		t.Errorf("read with no agent chat bound: %d", w.Code)
	}
	if len(fl.sent) != 0 {
		t.Error("sent with no agent chat bound")
	}
	w := do(h, "GET", "/health", atok, "")
	var out struct{ AgentChats []string }
	json.Unmarshal(w.Body.Bytes(), &out)
	if w.Code != 200 || out.AgentChats == nil || len(out.AgentChats) != 0 {
		t.Errorf("health shows no agent chat as []: %d %s", w.Code, w.Body)
	}
}

func TestAgentTokenInItsChat(t *testing.T) {
	st, fl, h := setupAgent(t, true, grp2)
	w := do(h, "GET", "/health", atok, "")
	var hl struct {
		AgentChats      []string
		AgentSendsDay   int
		AgentSendPerDay int
	}
	json.Unmarshal(w.Body.Bytes(), &hl)
	if w.Code != 200 || len(hl.AgentChats) != 1 || hl.AgentChats[0] != grp2 || hl.AgentSendPerDay != 20 {
		t.Fatalf("agent health: %d %s", w.Code, w.Body)
	}
	w = do(h, "GET", "/chats/"+grp2+"/messages?limit=10", atok, "")
	var ms struct{ Messages []Message }
	json.Unmarshal(w.Body.Bytes(), &ms)
	if w.Code != 200 || len(ms.Messages) != 1 || ms.Messages[0].Text != "a reply from the fixture" {
		t.Fatalf("agent reads its chat: %d %+v", w.Code, ms.Messages)
	}
	// An escaped @ is the same chat to the gate and to the handler.
	if w := do(h, "GET", "/chats/120000000000000002%40g.us/messages", atok, ""); w.Code != 200 {
		t.Errorf("escaped jid of the agent chat: %d", w.Code)
	}
	w = do(h, "POST", "/chats/"+grp2+"/send", atok, `{"text":"build is green"}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "SENT1") {
		t.Fatalf("agent send: %d %s", w.Code, w.Body)
	}
	if len(fl.texts) != 1 || fl.texts[0] != "SISO agent: build is green" || fl.sent[0] != grp2 {
		t.Fatalf("agent send must carry the label: %q to %v", fl.texts, fl.sent)
	}
	// Already labelled: not labelled twice.
	st.db.Exec(`UPDATE sends SET ts=ts-10`)
	if w := do(h, "POST", "/chats/"+grp2+"/send", atok, `{"text":"  SISO agent: done"}`); w.Code != 200 {
		t.Fatalf("second agent send: %d %s", w.Code, w.Body)
	}
	if fl.texts[1] != "SISO agent: done" {
		t.Errorf("double label: %q", fl.texts[1])
	}
	// The label alone is not a message.
	st.db.Exec(`UPDATE sends SET ts=ts-10`)
	if w := do(h, "POST", "/chats/"+grp2+"/send", atok, `{"text":"SISO agent:  "}`); w.Code != 400 {
		t.Errorf("label-only text: %d", w.Code)
	}
	// The ledger marks agent sends; the API token's do not count against the agent ceiling.
	if n := st.AgentSendsSince(time.Now().Add(-time.Hour)); n != 2 {
		t.Errorf("agent sends in ledger: %d, want 2", n)
	}
	st.db.Exec(`UPDATE sends SET ts=ts-10`)
	if w := do(h, "POST", "/chats/"+grp2+"/send", tok, `{"text":"from Shaan"}`); w.Code != 200 {
		t.Fatalf("main send to the agent chat: %d", w.Code)
	}
	if fl.texts[2] != "from Shaan" {
		t.Errorf("main token text must stay as written: %q", fl.texts[2])
	}
	if n := st.AgentSendsSince(time.Now().Add(-time.Hour)); n != 2 {
		t.Errorf("main send counted as agent: %d", n)
	}
}

func TestAgentSendNeedsSendOn(t *testing.T) {
	_, fl, h := setupAgent(t, false, grp2)
	if w := do(h, "POST", "/chats/"+grp2+"/send", atok, `{"text":"x"}`); w.Code != 403 || len(fl.sent) != 0 {
		t.Errorf("agent send with WA_SEND=0: %d sent=%d", w.Code, len(fl.sent))
	}
}

func TestAgentDailyCap(t *testing.T) {
	st, fl, h := setupAgent(t, true, grp2)
	// 19 agent sends earlier today (outside the hour window and the 4 s gap): one left.
	for i := 0; i < agentSendPerDay-1; i++ {
		st.db.Exec(`INSERT INTO sends (ts,chat,id,ok,agent) VALUES (?,?,?,1,1)`, time.Now().Add(-3*time.Hour).Unix(), grp2, "x")
	}
	if w := do(h, "POST", "/chats/"+grp2+"/send", atok, `{"text":"twentieth"}`); w.Code != 200 {
		t.Fatalf("20th agent send: %d %s", w.Code, w.Body)
	}
	st.db.Exec(`UPDATE sends SET ts=ts-10 WHERE id='SENT1'`)
	if w := do(h, "POST", "/chats/"+grp2+"/send", atok, `{"text":"twenty-first"}`); w.Code != 429 {
		t.Errorf("21st agent send: %d, want 429", w.Code)
	}
	if len(fl.sent) != 1 {
		t.Errorf("sent %d, want 1", len(fl.sent))
	}
	// The API token is not held by the agent ceiling.
	if w := do(h, "POST", "/chats/"+grp2+"/send", tok, `{"text":"main"}`); w.Code != 200 {
		t.Errorf("main token after agent cap: %d", w.Code)
	}
	// Yesterday's agent sends fall out of the window.
	st.db.Exec(`UPDATE sends SET ts=ts-86400 WHERE agent=1`)
	st.db.Exec(`UPDATE sends SET ts=ts-10`)
	if w := do(h, "POST", "/chats/"+grp2+"/send", atok, `{"text":"next day"}`); w.Code != 200 {
		t.Errorf("agent send after the window: %d", w.Code)
	}
}

func TestMainTokenUnchangedWithAgentToken(t *testing.T) {
	_, fl, h := setupAgent(t, true, grp2)
	for _, p := range []string{"/health", "/chats", "/rolodex", "/search?q=x", "/soak", "/qr", "/chats/" + alice + "/messages", "/chats/" + grp2 + "/messages"} {
		if w := do(h, "GET", p, tok, ""); w.Code != 200 {
			t.Errorf("main %s: %d", p, w.Code)
		}
	}
	if w := do(h, "POST", "/chats/"+alice+"/send", tok, `{"text":"hi"}`); w.Code != 200 || fl.texts[0] != "hi" {
		t.Errorf("main send to any chat he has, unlabelled: %d %q", w.Code, fl.texts)
	}
	if w := do(h, "GET", "/nope", tok, ""); w.Code != 404 {
		t.Errorf("main unknown route stays 404: %d", w.Code)
	}
	// An empty agent token never matches an empty bearer.
	_, _, h2, _ := setupCfg(t, Config{Token: tok})
	r := httptest.NewRequest("GET", "/health", nil)
	r.Header.Set("Authorization", "Bearer ")
	w := httptest.NewRecorder()
	h2.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Errorf("empty bearer with no agent token: %d", w.Code)
	}
}

func TestAgentLedgerColumnMigrates(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "messages.db")
	// A 0.2 store: the sends table without the agent column, holding one send.
	st, err := OpenStore(p, bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	st.db.Exec(`DROP TABLE sends`)
	st.db.Exec(`CREATE TABLE sends (ts INTEGER NOT NULL, chat TEXT NOT NULL, id TEXT NOT NULL, ok INTEGER NOT NULL)`)
	st.db.Exec(`INSERT INTO sends VALUES (?,?,?,1)`, time.Now().Unix(), alice, "old")
	st.Close()
	st, err = OpenStore(p, bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatalf("reopen migrates: %v", err)
	}
	defer st.Close()
	if n, _ := st.SendsSince(time.Now().Add(-time.Hour)); n != 1 {
		t.Errorf("old send kept: %d", n)
	}
	if st.AgentSendsSince(time.Now().Add(-time.Hour)) != 0 {
		t.Error("old send counted as agent")
	}
	st.LogSend(grp2, "new", true, true)
	if st.AgentSendsSince(time.Now().Add(-time.Hour)) != 1 {
		t.Error("agent send not marked")
	}
}
