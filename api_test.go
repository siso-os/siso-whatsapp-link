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
	reads  int
	status LinkStatus
	mu     sync.Mutex
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
	return "SENT1", nil
}
func (f *fakeLink) RequestHistory(ctx context.Context, oldest Message) error { return nil }
func (f *fakeLink) DisplayName(jid, stored string) string                    { return stored }

const (
	alice = "100000000001@s.whatsapp.net"
	bob   = "100000000002@s.whatsapp.net"
	grp   = "120000000000000001@g.us"
	tok   = "test-token"
)

func setup(t *testing.T, send bool) (*Store, *fakeLink, http.Handler, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := OpenStore(filepath.Join(dir, "messages.db"), bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	fl := &fakeLink{status: LinkStatus{State: "connected"}}
	cfg := Config{Token: tok, SendEnabled: send}
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
