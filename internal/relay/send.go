package relay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/chainguard-dev/clog"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

// maxSend bounds a message's request.
const maxSend = 64 << 10

// Message is a message to send.
type Message struct {
	// Room is a room ID, like !abc:example.org, or alias, like
	// #home:example.org, of a room it's in.
	Room string `json:"room"`
	// Body is the message's plain text.
	Body string `json:"body"`
	// Notice sends it as an m.notice, which clients show as a bot's.
	Notice bool `json:"notice,omitempty"`
}

// ServeHTTP sends a POST's Message, answering with its event ID. It refuses
// until the relay is ready, and to a room it isn't in, so that it knows
// whether the room is encrypted: it sends encrypted to an encrypted room.
func (r *Relay) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()
	if req.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// Browsers send these on POSTs, and apps don't: refusing them keeps web
	// pages, cross-origin or through DNS rebinding, from sending.
	if req.Header.Get("Origin") != "" || req.Header.Get("Sec-Fetch-Site") != "" {
		http.Error(w, "browsers can't send", http.StatusForbidden)
		return
	}
	select {
	case <-r.ready:
	default:
		http.Error(w, "not synced yet", http.StatusServiceUnavailable)
		return
	}
	var msg Message
	dec := json.NewDecoder(http.MaxBytesReader(w, req.Body, maxSend))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&msg); err != nil {
		http.Error(w, "invalid message: "+err.Error(), http.StatusBadRequest)
		return
	}
	if msg.Body == "" {
		http.Error(w, "empty body", http.StatusBadRequest)
		return
	}

	room := id.RoomID(msg.Room)
	switch {
	case strings.HasPrefix(msg.Room, "#"):
		resp, err := r.cli.ResolveAlias(ctx, id.RoomAlias(msg.Room))
		if err != nil {
			clog.WarnContext(ctx, "resolving a room alias", "room", msg.Room, "error", err)
			http.Error(w, "unknown room", http.StatusNotFound)
			return
		}
		room = resp.RoomID
	case !strings.HasPrefix(msg.Room, "!"):
		http.Error(w, "room is neither a room ID nor an alias", http.StatusBadRequest)
		return
	}
	if !r.cli.StateStore.IsMembership(ctx, room, r.cli.UserID, event.MembershipJoin) {
		http.Error(w, "not in the room", http.StatusForbidden)
		return
	}
	if err := r.loadRoom(ctx, room); err != nil {
		clog.ErrorContext(ctx, "loading a room's state", "room_id", room, "error", err)
		http.Error(w, "loading the room failed", http.StatusBadGateway)
		return
	}

	content := &event.MessageEventContent{MsgType: event.MsgText, Body: msg.Body}
	if msg.Notice {
		content.MsgType = event.MsgNotice
	}
	resp, err := r.cli.SendMessageEvent(ctx, room, event.EventMessage, content)
	if err != nil {
		clog.ErrorContext(ctx, "sending a message", "room_id", room, "error", err)
		status := http.StatusBadGateway
		if errors.Is(err, ctx.Err()) {
			status = http.StatusGatewayTimeout
		}
		http.Error(w, "sending failed", status)
		return
	}
	clog.InfoContext(ctx, "sent a message", "room_id", room, "event_id", resp.EventID)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]id.EventID{"event_id": resp.EventID})
}

// loadRoom makes sure the state store knows whether room is encrypted, and
// if it is, its members, whose devices get the room's keys: joining a room
// tells it the relay is in it before a sync tells it the room's state, and
// it would send in the clear to a room it doesn't know is encrypted.
func (r *Relay) loadRoom(ctx context.Context, room id.RoomID) error {
	store := r.cli.StateStore
	encrypted, err := store.IsEncrypted(ctx, room)
	if err != nil {
		return err
	}
	if !encrypted {
		var content event.EncryptionEventContent
		err := r.cli.StateEvent(ctx, room, event.StateEncryption, "", &content)
		if errors.Is(err, mautrix.MNotFound) {
			return nil
		} else if err != nil {
			return fmt.Errorf("getting the room's encryption: %w", err)
		}
		// StateEvent stores it.
		if encrypted, err = store.IsEncrypted(ctx, room); err != nil {
			return err
		} else if !encrypted {
			return fmt.Errorf("the room is encrypted with %s, which the state store doesn't take", content.Algorithm)
		}
	}
	fetched, err := store.HasFetchedMembers(ctx, room)
	if err != nil || fetched {
		return err
	}
	if _, err := r.cli.Members(ctx, room); err != nil {
		return fmt.Errorf("getting the room's members: %w", err)
	}
	return store.MarkMembersFetched(ctx, room)
}
