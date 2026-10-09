package main

// The WhatsApp link: one whatsmeow client holding Shaan's number as a companion device (like WhatsApp Desktop).
// It turns WhatsApp's events into store rows and keeps a small state machine the API and the watchdog read.
// Nothing here logs a message body or a phone number.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
)

// Linker is what the API needs from WhatsApp; the tests use a fake.
type Linker interface {
	Status() LinkStatus
	QR() (code string, active bool)
	StartPairing() error
	Download(ctx context.Context, raw []byte) ([]byte, string, error)
	Thumbnail(raw []byte) []byte
	MarkRead(ctx context.Context, chat string, msgs []Message) error
	Send(ctx context.Context, chat, text string) (string, error)
	RequestHistory(ctx context.Context, oldest Message) error
	DisplayName(jid string, stored string) string
}

type LinkStatus struct {
	State          string `json:"state"` // starting needs_qr pairing connecting connected disconnected logged_out banned outdated
	LoggedIn       bool   `json:"loggedIn"`
	Connected      bool   `json:"connected"`
	Since          int64  `json:"since"`
	LastEvent      int64  `json:"lastEvent"`
	Reconnects     int    `json:"reconnects"`
	HistoryChunks  int    `json:"historyChunks"`
	HistoryPercent int    `json:"historyPercent"`
	Detail         string `json:"detail,omitempty"`
}

type Link struct {
	cli     *whatsmeow.Client
	st      *Store
	hub     *Hub
	cfg     Config
	mu      sync.Mutex
	stat    LinkStatus
	qr      string
	qrOn    bool
	names   map[string]string
	namesAt time.Time
	soak    *Soak // the flight recorder's live-message counter (nil in tests)
}

func NewLink(ctx context.Context, cfg Config, st *Store, hub *Hub) (*Link, error) {
	store.SetOSInfo("SISO Agent Base", [3]uint32{1, 0, 0})
	container, err := sqlstore.New(ctx, "sqlite", "file:"+cfg.DataDir+"/session.db?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)", waLog.Stdout("store", "WARN", false))
	if err != nil {
		return nil, err
	}
	dev, err := container.GetFirstDevice(ctx)
	if err != nil {
		return nil, err
	}
	cli := whatsmeow.NewClient(dev, waLog.Stdout("wa", "WARN", false))
	cli.EnableAutoReconnect = true
	cli.AutomaticMessageRerequestFromPhone = true
	l := &Link{cli: cli, st: st, hub: hub, cfg: cfg, stat: LinkStatus{State: "starting", Since: time.Now().Unix()}}
	cli.AddEventHandler(l.handle)
	return l, nil
}

// Start connects when a device is already linked, otherwise waits for StartPairing (the QR is his one action).
func (l *Link) Start() error {
	if l.cli.Store.ID == nil {
		l.set("needs_qr", "")
		return nil
	}
	l.set("connecting", "")
	return l.cli.Connect()
}

func (l *Link) StartPairing() error {
	l.mu.Lock()
	if l.qrOn || l.cli.Store.ID != nil {
		l.mu.Unlock()
		return nil
	}
	l.qrOn = true
	l.mu.Unlock()
	ch, err := l.cli.GetQRChannel(context.Background())
	if err != nil {
		l.mu.Lock()
		l.qrOn = false
		l.mu.Unlock()
		return err
	}
	if err := l.cli.Connect(); err != nil {
		l.mu.Lock()
		l.qrOn = false
		l.mu.Unlock()
		return err
	}
	l.set("pairing", "")
	go func() {
		for item := range ch {
			l.mu.Lock()
			switch item.Event {
			case "code":
				l.qr = item.Code
			default:
				l.qr, l.qrOn = "", false
			}
			l.mu.Unlock()
			l.hub.Publish(Event{Type: "state"})
			if item.Event != "code" {
				log.Printf("pairing: %s", item.Event)
				if item.Event != "success" {
					l.cli.Disconnect()
					l.set("needs_qr", "pairing "+item.Event)
				}
			}
		}
	}()
	return nil
}

func (l *Link) QR() (string, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.qr, l.qrOn
}

func (l *Link) Status() LinkStatus {
	l.mu.Lock()
	s := l.stat
	l.mu.Unlock()
	s.LoggedIn, s.Connected = l.cli.IsLoggedIn(), l.cli.IsConnected()
	return s
}

func (l *Link) set(state, detail string) {
	l.mu.Lock()
	if l.stat.State != state {
		l.stat.Since = time.Now().Unix()
	}
	l.stat.State, l.stat.Detail = state, detail
	l.mu.Unlock()
	log.Printf("state: %s %s", state, detail)
	l.hub.Publish(Event{Type: "state"})
}

func (l *Link) touch() {
	l.mu.Lock()
	l.stat.LastEvent = time.Now().Unix()
	l.mu.Unlock()
}

func (l *Link) handle(evt any) {
	l.touch()
	ctx := context.Background()
	switch e := evt.(type) {
	case *events.Connected:
		l.set("connected", "")
		go l.refreshGroups()
	case *events.Disconnected:
		l.mu.Lock()
		l.stat.Reconnects++
		l.mu.Unlock()
		l.set("disconnected", "")
	case *events.KeepAliveTimeout:
		l.set("connecting", fmt.Sprintf("keepalive timeout x%d", e.ErrorCount))
	case *events.KeepAliveRestored:
		l.set("connected", "")
	case *events.StreamReplaced:
		l.set("disconnected", "stream replaced by another login of this device")
	case *events.LoggedOut:
		l.set("logged_out", "unlinked from the phone: "+e.Reason.String())
	case *events.TemporaryBan:
		l.set("banned", e.String())
	case *events.ClientOutdated:
		l.set("outdated", "whatsmeow needs an update")
	case *events.ConnectFailure:
		l.set("disconnected", "connect failure: "+e.Reason.String())
	case *events.PairSuccess:
		l.set("connecting", "paired")
	case *events.HistorySync:
		l.history(ctx, e.Data)
	case *events.Message:
		if l.put(ctx, e, true) {
			chat := l.canon(ctx, e.Info.Chat)
			l.soak.Live(e.Info.Timestamp, l.cli.Store.ID != nil && chat == l.cli.Store.ID.ToNonAD().String())
			l.hub.Publish(Event{Type: "message", Chat: chat})
		}
	case *events.Receipt:
		// His own other device read the chat (phone or Desktop): clear the count, as WhatsApp does.
		if e.IsFromMe && (e.Type == types.ReceiptTypeRead || e.Type == types.ReceiptTypeReadSelf) {
			zero := 0
			l.st.SetChatState(l.canon(ctx, e.Chat), &zero, nil, nil, nil, nil)
			l.hub.Publish(Event{Type: "chat", Chat: l.canon(ctx, e.Chat)})
		}
	case *events.MarkChatAsRead:
		jid := l.canon(ctx, e.JID)
		if e.Action.GetRead() {
			zero, f := 0, false
			l.st.SetChatState(jid, &zero, nil, nil, nil, &f)
		} else {
			t := true
			l.st.SetChatState(jid, nil, nil, nil, nil, &t)
		}
		l.hub.Publish(Event{Type: "chat", Chat: jid})
	case *events.Archive:
		a := e.Action.GetArchived()
		l.st.SetChatState(l.canon(ctx, e.JID), nil, &a, nil, nil, nil)
	case *events.Pin:
		p := e.Action.GetPinned()
		l.st.SetChatState(l.canon(ctx, e.JID), nil, nil, &p, nil, nil)
	case *events.Mute:
		m := int64(0)
		if e.Action.GetMuted() {
			m = e.Action.GetMuteEndTimestamp()
			if m <= 0 {
				m = -1 // muted forever
			}
		}
		l.st.SetChatState(l.canon(ctx, e.JID), nil, nil, nil, &m, nil)
	case *events.GroupInfo:
		if e.Name != nil {
			l.st.UpsertChat(Chat{JID: e.JID.String(), Name: e.Name.Name, IsGroup: true})
		}
	case *events.JoinedGroup:
		l.st.UpsertChat(Chat{JID: e.JID.String(), Name: e.GroupInfo.Name, IsGroup: true})
	}
}

// canon maps a hidden-user (LID) chat to the phone-number JID when WhatsApp has told us the pairing, so one
// person is one chat whichever address a message arrives on.
func (l *Link) canon(ctx context.Context, j types.JID) string {
	j = j.ToNonAD()
	if j.Server == types.HiddenUserServer && l.cli.Store.LIDs != nil {
		if pn, err := l.cli.Store.LIDs.GetPNForLID(ctx, j); err == nil && !pn.IsEmpty() {
			return pn.ToNonAD().String()
		}
	}
	return j.String()
}

func skipChat(j types.JID) bool {
	// Status updates are not his chats, and reading them is outward-visible; newsletters and broadcasts are not chats.
	return j.Server == types.BroadcastServer || j.Server == types.NewsletterServer || j.IsEmpty()
}

func (l *Link) history(ctx context.Context, data *waHistorySync.HistorySync) {
	if data == nil {
		return
	}
	n := 0
	for _, conv := range data.GetConversations() {
		j, err := types.ParseJID(conv.GetID())
		if err != nil || skipChat(j) {
			continue
		}
		jid := l.canon(ctx, j)
		name := conv.GetName()
		if name == "" {
			name = conv.GetDisplayName()
		}
		ts := int64(conv.GetConversationTimestamp())
		if lt := int64(conv.GetLastMsgTimestamp()); lt > ts {
			ts = lt
		}
		l.st.UpsertChat(Chat{JID: jid, Name: name, IsGroup: j.Server == types.GroupServer, LastTS: ts})
		unread, arch, pin := int(conv.GetUnreadCount()), conv.GetArchived(), conv.GetPinned() > 0
		mute := int64(conv.GetMuteEndTime())
		l.st.SetChatState(jid, &unread, &arch, &pin, &mute, nil)
		for _, hm := range conv.GetMessages() {
			wm := hm.GetMessage()
			if wm == nil {
				continue
			}
			ev, err := l.cli.ParseWebMessage(j, wm)
			if err != nil {
				continue
			}
			if l.put(ctx, ev, false) {
				n++
			}
		}
	}
	l.mu.Lock()
	l.stat.HistoryChunks++
	l.stat.HistoryPercent = int(data.GetProgress())
	l.mu.Unlock()
	log.Printf("history: type=%s chunk=%d convs=%d new_msgs=%d progress=%d", data.GetSyncType(), data.GetChunkOrder(), len(data.GetConversations()), n, data.GetProgress())
	l.hub.Publish(Event{Type: "history"})
}

// put turns one WhatsApp message into a store row; edits and deletions update the original. Returns true if stored.
func (l *Link) put(ctx context.Context, e *events.Message, live bool) bool {
	if skipChat(e.Info.Chat) || e.Message == nil {
		return false
	}
	chat := l.canon(ctx, e.Info.Chat)
	msg := e.Message
	if pm := msg.GetProtocolMessage(); pm != nil {
		switch pm.GetType() {
		case waE2E.ProtocolMessage_REVOKE:
			l.st.DeleteMessage(chat, pm.GetKey().GetID())
			l.hub.Publish(Event{Type: "chat", Chat: chat})
		case waE2E.ProtocolMessage_MESSAGE_EDIT:
			if t, _, _, _ := describe(pm.GetEditedMessage()); t != "" {
				l.st.EditMessage(chat, pm.GetKey().GetID(), t)
				l.hub.Publish(Event{Type: "chat", Chat: chat})
			}
		}
		return false
	}
	if e.IsEdit {
		if t, _, _, _ := describe(msg); t != "" {
			l.st.EditMessage(chat, e.Info.ID, t)
		}
		return false
	}
	text, kind, mime, file := describe(msg)
	if kind == "" {
		return false
	}
	raw, _ := proto.Marshal(msg)
	m := Message{ChatJID: chat, ID: e.Info.ID, Sender: l.canon(ctx, e.Info.Sender), SenderName: e.Info.PushName,
		FromMe: e.Info.IsFromMe, TS: e.Info.Timestamp.Unix(), Kind: kind, Text: text, Mime: mime, FileName: file,
		HasThumb: len(thumbOf(msg)) > 0, HasMedia: hasMedia(msg), QuotedID: quotedID(msg), Raw: raw}
	ok, err := l.st.PutMessage(m, live)
	if err != nil {
		log.Printf("store: put failed: %v", err)
	}
	return ok
}

func hasMedia(m *waE2E.Message) bool {
	return m.GetImageMessage() != nil || m.GetVideoMessage() != nil || m.GetAudioMessage() != nil ||
		m.GetDocumentMessage() != nil || m.GetStickerMessage() != nil
}

// describe returns (text, kind, mime, fileName); kind "" means "not a message the space shows".
func describe(m *waE2E.Message) (string, string, string, string) {
	if m == nil {
		return "", "", "", ""
	}
	switch {
	case m.GetConversation() != "":
		return m.GetConversation(), "text", "", ""
	case m.GetExtendedTextMessage() != nil:
		return m.GetExtendedTextMessage().GetText(), "text", "", ""
	case m.GetImageMessage() != nil:
		x := m.GetImageMessage()
		return x.GetCaption(), "image", x.GetMimetype(), ""
	case m.GetVideoMessage() != nil:
		x := m.GetVideoMessage()
		return x.GetCaption(), "video", x.GetMimetype(), ""
	case m.GetAudioMessage() != nil:
		return "", "audio", m.GetAudioMessage().GetMimetype(), ""
	case m.GetDocumentMessage() != nil:
		x := m.GetDocumentMessage()
		return x.GetCaption(), "document", x.GetMimetype(), x.GetFileName()
	case m.GetDocumentWithCaptionMessage() != nil:
		return describe(m.GetDocumentWithCaptionMessage().GetMessage())
	case m.GetStickerMessage() != nil:
		return "", "sticker", m.GetStickerMessage().GetMimetype(), ""
	case m.GetLocationMessage() != nil:
		return m.GetLocationMessage().GetName(), "location", "", ""
	case m.GetLiveLocationMessage() != nil:
		return "", "location", "", ""
	case m.GetContactMessage() != nil:
		return m.GetContactMessage().GetDisplayName(), "contact", "", ""
	case m.GetContactsArrayMessage() != nil:
		return m.GetContactsArrayMessage().GetDisplayName(), "contact", "", ""
	case m.GetPollCreationMessage() != nil:
		return m.GetPollCreationMessage().GetName(), "poll", "", ""
	case m.GetPollCreationMessageV3() != nil:
		return m.GetPollCreationMessageV3().GetName(), "poll", "", ""
	}
	return "", "", "", ""
}

func thumbOf(m *waE2E.Message) []byte {
	switch {
	case m.GetImageMessage() != nil:
		return m.GetImageMessage().GetJPEGThumbnail()
	case m.GetVideoMessage() != nil:
		return m.GetVideoMessage().GetJPEGThumbnail()
	case m.GetDocumentMessage() != nil:
		return m.GetDocumentMessage().GetJPEGThumbnail()
	case m.GetExtendedTextMessage() != nil:
		return m.GetExtendedTextMessage().GetJPEGThumbnail()
	case m.GetLocationMessage() != nil:
		return m.GetLocationMessage().GetJPEGThumbnail()
	}
	return nil
}

func quotedID(m *waE2E.Message) string {
	if x := m.GetExtendedTextMessage(); x != nil {
		return x.GetContextInfo().GetStanzaID()
	}
	return ""
}

func (l *Link) Thumbnail(raw []byte) []byte {
	var m waE2E.Message
	if proto.Unmarshal(raw, &m) != nil {
		return nil
	}
	return thumbOf(&m)
}

func (l *Link) Download(ctx context.Context, raw []byte) ([]byte, string, error) {
	var m waE2E.Message
	if err := proto.Unmarshal(raw, &m); err != nil {
		return nil, "", err
	}
	var d whatsmeow.DownloadableMessage
	mime := ""
	switch {
	case m.GetImageMessage() != nil:
		d, mime = m.GetImageMessage(), m.GetImageMessage().GetMimetype()
	case m.GetVideoMessage() != nil:
		d, mime = m.GetVideoMessage(), m.GetVideoMessage().GetMimetype()
	case m.GetAudioMessage() != nil:
		d, mime = m.GetAudioMessage(), m.GetAudioMessage().GetMimetype()
	case m.GetDocumentMessage() != nil:
		d, mime = m.GetDocumentMessage(), m.GetDocumentMessage().GetMimetype()
	case m.GetStickerMessage() != nil:
		d, mime = m.GetStickerMessage(), m.GetStickerMessage().GetMimetype()
	default:
		return nil, "", errors.New("no media")
	}
	b, err := l.cli.Download(ctx, d)
	return b, mime, err
}

// MarkRead clears the count locally. It sends read receipts (blue ticks) only when the operator has turned them on:
// receipts are visible to the other person, so they are off until Shaan says otherwise.
func (l *Link) MarkRead(ctx context.Context, chat string, msgs []Message) error {
	zero, f := 0, false
	if err := l.st.SetChatState(chat, &zero, nil, nil, nil, &f); err != nil {
		return err
	}
	if !l.cfg.ReadReceipts || !l.cli.IsConnected() {
		return nil
	}
	cj, err := types.ParseJID(chat)
	if err != nil {
		return err
	}
	bySender := map[string][]types.MessageID{}
	var last int64
	for _, m := range msgs {
		if m.FromMe {
			continue
		}
		bySender[m.Sender] = append(bySender[m.Sender], m.ID)
		if m.TS > last {
			last = m.TS
		}
	}
	for sender, ids := range bySender {
		sj := types.EmptyJID
		if cj.Server == types.GroupServer {
			sj, _ = types.ParseJID(sender)
		}
		if err := l.cli.MarkRead(ctx, ids, time.Unix(last, 0), cj, sj); err != nil {
			return err
		}
	}
	return nil
}

var ErrSendDisabled = errors.New("sending is turned off on this gateway")

func (l *Link) Send(ctx context.Context, chat, text string) (string, error) {
	if !l.cfg.SendEnabled {
		return "", ErrSendDisabled
	}
	j, err := types.ParseJID(chat)
	if err != nil {
		return "", err
	}
	resp, err := l.cli.SendMessage(ctx, j, &waE2E.Message{Conversation: proto.String(text)})
	if err != nil {
		return "", err
	}
	return resp.ID, nil
}

// RequestHistory asks his phone for older messages in one chat (WhatsApp's on-demand history sync).
func (l *Link) RequestHistory(ctx context.Context, oldest Message) error {
	if !l.cli.IsConnected() {
		return errors.New("not connected")
	}
	cj, err := types.ParseJID(oldest.ChatJID)
	if err != nil {
		return err
	}
	info := &types.MessageInfo{MessageSource: types.MessageSource{Chat: cj, IsFromMe: oldest.FromMe, IsGroup: cj.Server == types.GroupServer},
		ID: oldest.ID, Timestamp: time.Unix(oldest.TS, 0)}
	_, err = l.cli.SendPeerMessage(ctx, l.cli.BuildHistorySyncRequest(info, 50))
	return err
}

// DisplayName prefers his address-book name, then the name WhatsApp gave the chat, then their push name.
func (l *Link) DisplayName(jid, stored string) string {
	l.mu.Lock()
	if time.Since(l.namesAt) > time.Minute || l.names == nil {
		l.names = map[string]string{}
		if all, err := l.cli.Store.Contacts.GetAllContacts(context.Background()); err == nil {
			for j, c := range all {
				n := c.FullName
				if n == "" {
					n = c.FirstName
				}
				if n == "" {
					n = c.BusinessName
				}
				if n == "" && c.PushName != "" {
					n = "~" + c.PushName
				}
				l.names[j.String()] = n
			}
		}
		l.namesAt = time.Now()
	}
	n := l.names[jid]
	l.mu.Unlock()
	if n != "" && !strings.HasPrefix(n, "~") {
		return n
	}
	if stored != "" {
		return stored
	}
	return strings.TrimPrefix(n, "~")
}

func (l *Link) refreshGroups() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	groups, err := l.cli.GetJoinedGroups(ctx)
	if err != nil {
		log.Printf("groups: %v", err)
		return
	}
	for _, g := range groups {
		l.st.UpsertChat(Chat{JID: g.JID.String(), Name: g.Name, IsGroup: true})
	}
	log.Printf("groups: %d joined", len(groups))
}

func (l *Link) Close() { l.cli.Disconnect() }
