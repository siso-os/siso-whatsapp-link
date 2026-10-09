package main

// The gateway's flight recorder for the soak: one CSV row a minute plus one on every link state change, start and
// stop, in <data>/soak.csv (0600, rotated at 8 MB). Numbers and states only: no chat, no sender, no text. GET /soak
// serves the rows; soak/report.mjs turns them into the memory and state graph.

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

const soakHeader = "ts,kind,pid,uptimeSec,state,loggedIn,connected,rssMB,heapMB,goroutines,reconnects,chats,messages,unreadChats,live,self,late,lagMaxSec,lagAvgSec"

type SoakRow struct {
	TS         int64   `json:"ts"`
	Kind       string  `json:"kind"` // start sample state stop
	PID        int     `json:"pid"`
	UptimeSec  int     `json:"uptimeSec"`
	State      string  `json:"state"`
	LoggedIn   bool    `json:"loggedIn"`
	Connected  bool    `json:"connected"`
	RSSMB      int     `json:"rssMB"`
	HeapMB     int     `json:"heapMB"`
	Goroutines int     `json:"goroutines"`
	Reconnects int     `json:"reconnects"`
	Chats      int     `json:"chats"`
	Messages   int     `json:"messages"`
	Unread     int     `json:"unreadChats"`
	Live       int     `json:"live"`      // live messages stored since the previous row
	Self       int     `json:"self"`      // of those, in his own "Message yourself" chat
	Late       int     `json:"late"`      // of those, delivered more than a minute after they were sent
	LagMaxSec  float64 `json:"lagMaxSec"` // sent-to-stored delay, worst and mean, over the same messages
	LagAvgSec  float64 `json:"lagAvgSec"`
}

type Soak struct {
	path    string
	started time.Time
	mu      sync.Mutex
	live    int
	self    int
	late    int
	lagMax  float64
	lagSum  float64
}

func NewSoak(dataDir string, started time.Time) *Soak {
	return &Soak{path: filepath.Join(dataDir, "soak.csv"), started: started}
}

// Live counts one live message: when it was sent, and whether it is in his own chat. Safe on a nil Soak.
func (s *Soak) Live(sent time.Time, self bool) {
	if s == nil {
		return
	}
	lag := time.Since(sent).Seconds()
	if lag < 0 {
		lag = 0
	}
	s.mu.Lock()
	s.live++
	if self {
		s.self++
	}
	if lag > 60 {
		s.late++
	}
	if lag > s.lagMax {
		s.lagMax = lag
	}
	s.lagSum += lag
	s.mu.Unlock()
}

// Run writes a start row, a sample every `every`, a state row on each link state change, and a stop row on the way out.
func (s *Soak) Run(ctx context.Context, link Linker, st *Store, hub *Hub, every time.Duration) {
	ch := hub.Subscribe()
	defer hub.Unsubscribe(ch)
	t := time.NewTicker(every)
	defer t.Stop()
	s.write(s.row("start", link, st))
	last := link.Status().State
	for {
		select {
		case <-ctx.Done():
			s.write(s.row("stop", link, st))
			return
		case <-t.C:
			s.write(s.row("sample", link, st))
		case e := <-ch:
			if e.Type != "state" {
				continue
			}
			if now := link.Status().State; now != last {
				last = now
				s.write(s.row("state", link, st))
			}
		}
	}
}

func (s *Soak) row(kind string, link Linker, st *Store) SoakRow {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	ls, c := link.Status(), st.Counts()
	r := SoakRow{TS: time.Now().Unix(), Kind: kind, PID: os.Getpid(), UptimeSec: int(time.Since(s.started).Seconds()),
		State: ls.State, LoggedIn: ls.LoggedIn, Connected: ls.Connected, RSSMB: rssMB(), HeapMB: int(m.HeapAlloc >> 20),
		Goroutines: runtime.NumGoroutine(), Reconnects: ls.Reconnects, Chats: c.Chats, Messages: c.Messages, Unread: c.Unread}
	if kind == "sample" || kind == "stop" {
		s.mu.Lock()
		r.Live, r.Self, r.Late, r.LagMaxSec = s.live, s.self, s.late, round1(s.lagMax)
		if s.live > 0 {
			r.LagAvgSec = round1(s.lagSum / float64(s.live))
		}
		s.live, s.self, s.late, s.lagMax, s.lagSum = 0, 0, 0, 0, 0
		s.mu.Unlock()
	}
	return r
}

func round1(f float64) float64 { return float64(int(f*10+0.5)) / 10 }

func (s *Soak) write(r SoakRow) {
	if fi, err := os.Stat(s.path); err == nil && fi.Size() > 8<<20 {
		os.Rename(s.path, s.path+".1")
	}
	f, err := os.OpenFile(s.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	if fi, _ := f.Stat(); fi != nil && fi.Size() == 0 {
		fmt.Fprintln(f, soakHeader)
	}
	fmt.Fprintf(f, "%d,%s,%d,%d,%s,%t,%t,%d,%d,%d,%d,%d,%d,%d,%d,%d,%d,%g,%g\n", r.TS, r.Kind, r.PID, r.UptimeSec,
		r.State, r.LoggedIn, r.Connected, r.RSSMB, r.HeapMB, r.Goroutines, r.Reconnects, r.Chats, r.Messages, r.Unread,
		r.Live, r.Self, r.Late, r.LagMaxSec, r.LagAvgSec)
}

// Rows reads the recorder back (the rotated file first), keeping rows at or after `since`.
func (s *Soak) Rows(since int64) []SoakRow {
	var out []SoakRow
	for _, p := range []string{s.path + ".1", s.path} {
		f, err := os.Open(p)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			if r, ok := parseSoak(sc.Text()); ok && r.TS >= since {
				out = append(out, r)
			}
		}
		f.Close()
	}
	return out
}

func parseSoak(line string) (SoakRow, bool) {
	p := strings.Split(line, ",")
	if len(p) != 19 || p[0] == "ts" {
		return SoakRow{}, false
	}
	i := func(k int) int { v, _ := strconv.Atoi(p[k]); return v }
	f := func(k int) float64 { v, _ := strconv.ParseFloat(p[k], 64); return v }
	ts, err := strconv.ParseInt(p[0], 10, 64)
	if err != nil {
		return SoakRow{}, false
	}
	return SoakRow{TS: ts, Kind: p[1], PID: i(2), UptimeSec: i(3), State: p[4], LoggedIn: p[5] == "true", Connected: p[6] == "true",
		RSSMB: i(7), HeapMB: i(8), Goroutines: i(9), Reconnects: i(10), Chats: i(11), Messages: i(12), Unread: i(13),
		Live: i(14), Self: i(15), Late: i(16), LagMaxSec: f(17), LagAvgSec: f(18)}, true
}
