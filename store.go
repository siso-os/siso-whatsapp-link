package main

// The message store: chats and messages for the WhatsApp space, in one SQLite file on the box that holds the link.
// Message text and the raw message proto are sealed with AES-256-GCM (the mini has no FileVault); everything else
// (chat ids, names, timestamps, counts) is metadata and stays plain so the list and the Rolodex feed are cheap.

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct {
	db   *sql.DB
	aead cipher.AEAD
	mu   sync.Mutex // serialises writes; SQLite is happiest with one writer
}

type Chat struct {
	JID          string `json:"jid"`
	Name         string `json:"name"`
	IsGroup      bool   `json:"isGroup"`
	LastTS       int64  `json:"lastTs"`
	Unread       int    `json:"unread"`
	MarkedUnread bool   `json:"markedUnread,omitempty"`
	Archived     bool   `json:"archived,omitempty"`
	Pinned       bool   `json:"pinned,omitempty"`
	MutedUntil   int64  `json:"mutedUntil,omitempty"`
	Preview      string `json:"preview,omitempty"`
	PreviewFrom  bool   `json:"previewFromMe,omitempty"`
	PreviewKind  string `json:"previewKind,omitempty"`
}

type Message struct {
	ChatJID    string `json:"chat"`
	ID         string `json:"id"`
	Sender     string `json:"sender,omitempty"`
	SenderName string `json:"senderName,omitempty"`
	FromMe     bool   `json:"fromMe"`
	TS         int64  `json:"ts"`
	Kind       string `json:"kind"` // text image video audio document sticker location contact other
	Text       string `json:"text,omitempty"`
	Mime       string `json:"mime,omitempty"`
	FileName   string `json:"fileName,omitempty"`
	HasThumb   bool   `json:"hasThumb,omitempty"`
	HasMedia   bool   `json:"hasMedia,omitempty"`
	QuotedID   string `json:"quotedId,omitempty"`
	Deleted    bool   `json:"deleted,omitempty"`
	Edited     bool   `json:"edited,omitempty"`
	Raw        []byte `json:"-"`
}

const schema = `
PRAGMA journal_mode=WAL;
PRAGMA busy_timeout=5000;
CREATE TABLE IF NOT EXISTS chats (
  jid TEXT PRIMARY KEY, name TEXT NOT NULL DEFAULT '', is_group INTEGER NOT NULL DEFAULT 0,
  last_ts INTEGER NOT NULL DEFAULT 0, unread INTEGER NOT NULL DEFAULT 0, marked_unread INTEGER NOT NULL DEFAULT 0,
  archived INTEGER NOT NULL DEFAULT 0, pinned INTEGER NOT NULL DEFAULT 0, muted_until INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS messages (
  chat TEXT NOT NULL, id TEXT NOT NULL, sender TEXT NOT NULL DEFAULT '', sender_name TEXT NOT NULL DEFAULT '',
  from_me INTEGER NOT NULL DEFAULT 0, ts INTEGER NOT NULL, kind TEXT NOT NULL, text_enc BLOB, raw_enc BLOB,
  mime TEXT NOT NULL DEFAULT '', file_name TEXT NOT NULL DEFAULT '', has_thumb INTEGER NOT NULL DEFAULT 0,
  has_media INTEGER NOT NULL DEFAULT 0, quoted_id TEXT NOT NULL DEFAULT '', deleted INTEGER NOT NULL DEFAULT 0,
  edited INTEGER NOT NULL DEFAULT 0, PRIMARY KEY (chat, id)
);
CREATE INDEX IF NOT EXISTS messages_chat_ts ON messages(chat, ts);
CREATE TABLE IF NOT EXISTS sends (ts INTEGER NOT NULL, chat TEXT NOT NULL, id TEXT NOT NULL, ok INTEGER NOT NULL);
`

func OpenStore(path string, key []byte) (*Store, error) {
	if len(key) != 32 {
		return nil, errors.New("body key must be 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(4)
	if _, err := db.Exec(schema); err != nil {
		return nil, fmt.Errorf("schema: %w", err)
	}
	return &Store{db: db, aead: aead}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) seal(b []byte) []byte {
	if b == nil {
		return nil
	}
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		panic(err)
	}
	return s.aead.Seal(nonce, nonce, b, nil)
}

func (s *Store) open(b []byte) []byte {
	if len(b) < s.aead.NonceSize() {
		return nil
	}
	n := s.aead.NonceSize()
	out, err := s.aead.Open(nil, b[:n], b[n:], nil)
	if err != nil {
		return nil
	}
	return out
}

// UpsertChat merges what is known about a chat. Empty name never overwrites a real one; a later lastTs wins.
func (s *Store) UpsertChat(c Chat) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`INSERT INTO chats (jid,name,is_group,last_ts,unread,archived,pinned,muted_until) VALUES (?,?,?,?,?,?,?,?)
ON CONFLICT(jid) DO UPDATE SET name=CASE WHEN excluded.name<>'' THEN excluded.name ELSE chats.name END,
 is_group=excluded.is_group, last_ts=MAX(chats.last_ts, excluded.last_ts)`,
		c.JID, c.Name, b2i(c.IsGroup), c.LastTS, c.Unread, b2i(c.Archived), b2i(c.Pinned), c.MutedUntil)
	return err
}

// SetChatState writes the state WhatsApp itself reports for a chat (history sync, app state). Unread is absolute.
func (s *Store) SetChatState(jid string, unread *int, archived, pinned *bool, mutedUntil *int64, markedUnread *bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	sets, args := []string{}, []any{}
	if unread != nil {
		sets, args = append(sets, "unread=?"), append(args, *unread)
	}
	if archived != nil {
		sets, args = append(sets, "archived=?"), append(args, b2i(*archived))
	}
	if pinned != nil {
		sets, args = append(sets, "pinned=?"), append(args, b2i(*pinned))
	}
	if mutedUntil != nil {
		sets, args = append(sets, "muted_until=?"), append(args, *mutedUntil)
	}
	if markedUnread != nil {
		sets, args = append(sets, "marked_unread=?"), append(args, b2i(*markedUnread))
	}
	if len(sets) == 0 {
		return nil
	}
	if _, err := s.db.Exec(`INSERT OR IGNORE INTO chats (jid) VALUES (?)`, jid); err != nil {
		return err
	}
	_, err := s.db.Exec(`UPDATE chats SET `+strings.Join(sets, ",")+` WHERE jid=?`, append(args, jid)...)
	return err
}

// PutMessage stores a message. live=true means it arrived now (not from history): it moves the chat's unread count
// the way the phone does (incoming +1, anything sent from his own devices clears it).
func (s *Store) PutMessage(m Message, live bool) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var textEnc []byte
	if m.Text != "" {
		textEnc = s.seal([]byte(m.Text))
	}
	res, err := s.db.Exec(`INSERT INTO messages (chat,id,sender,sender_name,from_me,ts,kind,text_enc,raw_enc,mime,file_name,has_thumb,has_media,quoted_id)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(chat,id) DO NOTHING`,
		m.ChatJID, m.ID, m.Sender, m.SenderName, b2i(m.FromMe), m.TS, m.Kind, textEnc, s.seal(m.Raw), m.Mime, m.FileName,
		b2i(m.HasThumb), b2i(m.HasMedia), m.QuotedID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return false, nil
	}
	if _, err := s.db.Exec(`INSERT OR IGNORE INTO chats (jid,is_group) VALUES (?,?)`, m.ChatJID, b2i(strings.HasSuffix(m.ChatJID, "@g.us"))); err != nil {
		return true, err
	}
	if _, err := s.db.Exec(`UPDATE chats SET last_ts=MAX(last_ts,?) WHERE jid=?`, m.TS, m.ChatJID); err != nil {
		return true, err
	}
	if live {
		if m.FromMe {
			_, err = s.db.Exec(`UPDATE chats SET unread=0, marked_unread=0 WHERE jid=?`, m.ChatJID)
		} else {
			_, err = s.db.Exec(`UPDATE chats SET unread=unread+1 WHERE jid=?`, m.ChatJID)
		}
	}
	return true, err
}

func (s *Store) EditMessage(chat, id, text string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`UPDATE messages SET text_enc=?, edited=1 WHERE chat=? AND id=?`, s.seal([]byte(text)), chat, id)
	return err
}

func (s *Store) DeleteMessage(chat, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`UPDATE messages SET text_enc=NULL, raw_enc=NULL, has_thumb=0, has_media=0, deleted=1 WHERE chat=? AND id=?`, chat, id)
	return err
}

// Chats lists chats newest first with the last message as a preview. archived: include archived chats.
func (s *Store) Chats(limit, offset int, archived bool) ([]Chat, error) {
	where := "WHERE c.archived=0"
	if archived {
		where = ""
	}
	rows, err := s.db.Query(`SELECT c.jid,c.name,c.is_group,c.last_ts,c.unread,c.marked_unread,c.archived,c.pinned,c.muted_until,
 m.text_enc,m.from_me,m.kind,m.deleted
FROM chats c LEFT JOIN messages m ON m.rowid = (SELECT rowid FROM messages WHERE chat=c.jid ORDER BY ts DESC LIMIT 1)
`+where+` ORDER BY c.pinned DESC, c.last_ts DESC LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Chat{}
	for rows.Next() {
		var c Chat
		var isGroup, mu, arch, pin int
		var textEnc []byte
		var fromMe, del sql.NullInt64
		var kind sql.NullString
		if err := rows.Scan(&c.JID, &c.Name, &isGroup, &c.LastTS, &c.Unread, &mu, &arch, &pin, &c.MutedUntil, &textEnc, &fromMe, &kind, &del); err != nil {
			return nil, err
		}
		c.IsGroup, c.MarkedUnread, c.Archived, c.Pinned = isGroup == 1, mu == 1, arch == 1, pin == 1
		c.Preview, c.PreviewFrom, c.PreviewKind = string(s.open(textEnc)), fromMe.Int64 == 1, kind.String
		if del.Int64 == 1 {
			c.PreviewKind = "deleted"
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) Chat(jid string) (Chat, bool) {
	var c Chat
	var isGroup int
	err := s.db.QueryRow(`SELECT jid,name,is_group,last_ts,unread FROM chats WHERE jid=?`, jid).Scan(&c.JID, &c.Name, &isGroup, &c.LastTS, &c.Unread)
	c.IsGroup = isGroup == 1
	return c, err == nil
}

const msgCols = `chat,id,sender,sender_name,from_me,ts,kind,text_enc,mime,file_name,has_thumb,has_media,quoted_id,deleted,edited`

func (s *Store) scanMessages(rows *sql.Rows) ([]Message, error) {
	defer rows.Close()
	out := []Message{}
	for rows.Next() {
		var m Message
		var fromMe, thumb, media, del, ed int
		var textEnc []byte
		if err := rows.Scan(&m.ChatJID, &m.ID, &m.Sender, &m.SenderName, &fromMe, &m.TS, &m.Kind, &textEnc, &m.Mime, &m.FileName, &thumb, &media, &m.QuotedID, &del, &ed); err != nil {
			return nil, err
		}
		m.FromMe, m.HasThumb, m.HasMedia, m.Deleted, m.Edited = fromMe == 1, thumb == 1, media == 1, del == 1, ed == 1
		m.Text = string(s.open(textEnc))
		out = append(out, m)
	}
	return out, rows.Err()
}

// Messages returns up to limit messages older than before (0 = newest), oldest first.
func (s *Store) Messages(chat string, before int64, limit int) ([]Message, error) {
	if before <= 0 {
		before = 1 << 62
	}
	rows, err := s.db.Query(`SELECT `+msgCols+` FROM messages WHERE chat=? AND ts<? ORDER BY ts DESC LIMIT ?`, chat, before, limit)
	if err != nil {
		return nil, err
	}
	ms, err := s.scanMessages(rows)
	for i, j := 0, len(ms)-1; i < j; i, j = i+1, j-1 {
		ms[i], ms[j] = ms[j], ms[i]
	}
	return ms, err
}

func (s *Store) Oldest(chat string) (Message, bool) {
	rows, err := s.db.Query(`SELECT `+msgCols+` FROM messages WHERE chat=? ORDER BY ts ASC LIMIT 1`, chat)
	if err != nil {
		return Message{}, false
	}
	ms, _ := s.scanMessages(rows)
	if len(ms) == 0 {
		return Message{}, false
	}
	return ms[0], true
}

func (s *Store) Raw(chat, id string) []byte {
	var enc []byte
	if err := s.db.QueryRow(`SELECT raw_enc FROM messages WHERE chat=? AND id=?`, chat, id).Scan(&enc); err != nil {
		return nil
	}
	return s.open(enc)
}

// Search scans decrypted text newest first (bodies are sealed, so there is no SQL index over them).
func (s *Store) Search(q, chat string, limit int) ([]Message, error) {
	q = strings.ToLower(strings.TrimSpace(q))
	if q == "" {
		return []Message{}, nil
	}
	where, args := "WHERE text_enc IS NOT NULL", []any{}
	if chat != "" {
		where, args = where+" AND chat=?", append(args, chat)
	}
	rows, err := s.db.Query(`SELECT `+msgCols+` FROM messages `+where+` ORDER BY ts DESC`, args...)
	if err != nil {
		return nil, err
	}
	all, err := s.scanMessages(rows)
	if err != nil {
		return nil, err
	}
	out := []Message{}
	for _, m := range all {
		if strings.Contains(strings.ToLower(m.Text), q) || strings.Contains(strings.ToLower(m.FileName), q) {
			out = append(out, m)
			if len(out) >= limit {
				break
			}
		}
	}
	return out, nil
}

type RolodexRow struct {
	JID      string `json:"jid"`
	Phone    string `json:"phone,omitempty"`
	Name     string `json:"name"`
	IsGroup  bool   `json:"isGroup"`
	LastTS   int64  `json:"lastTs"`
	LastIn   int64  `json:"lastIn"`
	LastOut  int64  `json:"lastOut"`
	Messages int    `json:"messages"`
	Sent     int    `json:"sent"`
}

// Rolodex is the people feed: counts and dates only, never bodies.
func (s *Store) Rolodex() ([]RolodexRow, error) {
	rows, err := s.db.Query(`SELECT c.jid,c.name,c.is_group,c.last_ts,
 COALESCE(MAX(CASE WHEN m.from_me=0 THEN m.ts END),0), COALESCE(MAX(CASE WHEN m.from_me=1 THEN m.ts END),0),
 COUNT(m.id), COALESCE(SUM(m.from_me),0)
FROM chats c LEFT JOIN messages m ON m.chat=c.jid GROUP BY c.jid ORDER BY c.last_ts DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RolodexRow{}
	for rows.Next() {
		var r RolodexRow
		var g int
		if err := rows.Scan(&r.JID, &r.Name, &g, &r.LastTS, &r.LastIn, &r.LastOut, &r.Messages, &r.Sent); err != nil {
			return nil, err
		}
		r.IsGroup = g == 1
		if strings.HasSuffix(r.JID, "@s.whatsapp.net") {
			r.Phone = "+" + strings.TrimSuffix(r.JID, "@s.whatsapp.net")
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

type Counts struct {
	Chats    int `json:"chats"`
	Groups   int `json:"groups"`
	Messages int `json:"messages"`
	Unread   int `json:"unreadChats"`
}

func (s *Store) Counts() Counts {
	var c Counts
	s.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(is_group),0), COALESCE(SUM(CASE WHEN (unread>0 OR marked_unread=1) AND archived=0 THEN 1 ELSE 0 END),0) FROM chats`).Scan(&c.Chats, &c.Groups, &c.Unread)
	s.db.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&c.Messages)
	return c
}

// Send ledger: the rate limiter's memory and the audit trail (chat id and message id only).
func (s *Store) LogSend(chat, id string, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.db.Exec(`INSERT INTO sends (ts,chat,id,ok) VALUES (?,?,?,?)`, time.Now().Unix(), chat, id, b2i(ok))
}

func (s *Store) SendsSince(t time.Time) (n int, last int64) {
	s.db.QueryRow(`SELECT COUNT(*), COALESCE(MAX(ts),0) FROM sends WHERE ts>=?`, t.Unix()).Scan(&n, &last)
	return
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}
