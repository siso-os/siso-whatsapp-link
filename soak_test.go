package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The recorder writes start, sample, state and stop rows with numbers only, and /soak serves them behind the token.
func TestSoakRecorder(t *testing.T) {
	st, fl, _, dir := setup(t, false)
	seed(t, st)
	hub := NewHub()
	s := NewSoak(dir, time.Now().Add(-time.Minute))
	s.Live(time.Now().Add(-2*time.Second), false)
	s.Live(time.Now().Add(-90*time.Second), true)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx, fl, st, hub, 50*time.Millisecond); close(done) }()
	time.Sleep(120 * time.Millisecond)
	fl.setStatus(LinkStatus{State: "disconnected", LoggedIn: true})
	hub.Publish(Event{Type: "state"})
	time.Sleep(30 * time.Millisecond)
	cancel()
	<-done

	rows := s.Rows(0)
	kinds := map[string]int{}
	for _, r := range rows {
		kinds[r.Kind]++
	}
	if kinds["start"] != 1 || kinds["stop"] != 1 || kinds["state"] != 1 || kinds["sample"] < 1 {
		t.Fatalf("kinds %v", kinds)
	}
	var first SoakRow
	for _, r := range rows {
		if r.Kind == "sample" {
			first = r
			break
		}
	}
	if first.Live != 2 || first.Self != 1 || first.Late != 1 || first.LagMaxSec < 89 || first.Chats != 2 || first.Messages != 4 || first.RSSMB <= 0 || first.PID != os.Getpid() {
		t.Fatalf("sample %+v", first)
	}
	if rows[len(rows)-1].State != "disconnected" {
		t.Fatalf("last state %q", rows[len(rows)-1].State)
	}

	raw, _ := os.ReadFile(filepath.Join(dir, "soak.csv"))
	if fi, _ := os.Stat(filepath.Join(dir, "soak.csv")); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode().Perm())
	}
	for _, secret := range []string{alice, grp, "100000000", "fixture", "Fixture", "Banana", "Bob"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("soak.csv holds %q", secret)
		}
	}

	api := NewAPI(Config{Token: tok}, st, fl, hub)
	api.soak = s
	h := api.Handler()
	if w := do(h, "GET", "/soak", "", ""); w.Code != 401 {
		t.Fatalf("no token: %d", w.Code)
	}
	w := do(h, "GET", "/soak?since=0", tok, "")
	var out struct{ Rows []SoakRow }
	if json.Unmarshal(w.Body.Bytes(), &out); w.Code != 200 || len(out.Rows) != len(rows) {
		t.Fatalf("/soak %d rows=%d", w.Code, len(out.Rows))
	}
	if w := do(h, "GET", "/soak?since=9999999999", tok, ""); !strings.Contains(w.Body.String(), `"rows":[]`) {
		t.Fatalf("future since: %s", w.Body.String())
	}
}

func TestSoakRotates(t *testing.T) {
	dir := t.TempDir()
	s := NewSoak(dir, time.Now())
	big := strings.Repeat("x", 9<<20)
	os.WriteFile(s.path, []byte(soakHeader+"\n1,sample,1,1,connected,true,true,1,1,1,0,0,0,0,0,0,0,0,0\n"+big), 0o600)
	s.write(SoakRow{TS: 2, Kind: "sample", State: "connected"})
	if _, err := os.Stat(s.path + ".1"); err != nil {
		t.Fatal("not rotated")
	}
	if rows := s.Rows(0); len(rows) != 2 || rows[0].TS != 1 || rows[1].TS != 2 {
		t.Fatalf("rows %+v", rows)
	}
}
