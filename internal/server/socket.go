package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// keepAliveSeconds is the ForceKeepAlive interval sent to clients, as Jellyfin does.
const keepAliveSeconds = 60

type socketMessage struct {
	MessageType string
	Data        any `json:",omitempty"`
}

// hub serves the client /socket and broadcasts server messages.
type hub struct {
	log   *slog.Logger
	mu    sync.Mutex
	conns map[*client]struct{}
}

type client struct {
	userID string
	out    chan socketMessage
}

func newHub(log *slog.Logger) *hub { return &hub{log: log, conns: map[*client]struct{}{}} }

// serve runs one authenticated client connection until it closes.
func (h *hub) serve(w http.ResponseWriter, r *http.Request, userID string) {
	// Token auth, not cookies, protects this socket, so any origin may connect
	// (native apps send none; jellyfin-web is same-origin).
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}
	defer conn.CloseNow()
	conn.SetReadLimit(1 << 20)

	c := &client{userID: userID, out: make(chan socketMessage, 64)}
	h.mu.Lock()
	h.conns[c] = struct{}{}
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		delete(h.conns, c)
		h.mu.Unlock()
	}()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	go func() { // single writer
		defer cancel()
		for {
			select {
			case <-ctx.Done():
				return
			case m := <-c.out:
				wctx, wcancel := context.WithTimeout(ctx, 10*time.Second)
				err := wsjson.Write(wctx, conn, m)
				wcancel()
				if err != nil {
					return
				}
			}
		}
	}()

	c.send(socketMessage{MessageType: "ForceKeepAlive", Data: keepAliveSeconds})
	for {
		var msg struct {
			MessageType string
			Data        json.RawMessage
		}
		// Clients send KeepAlive every keepAliveSeconds/2; allow some slack.
		rctx, rcancel := context.WithTimeout(ctx, 2*keepAliveSeconds*time.Second)
		err := wsjson.Read(rctx, conn, &msg)
		rcancel()
		if err != nil {
			return
		}
		if msg.MessageType == "KeepAlive" {
			c.send(socketMessage{MessageType: "KeepAlive"})
		}
	}
}

// send queues a message, dropping it if the client is too slow to keep up.
func (c *client) send(m socketMessage) {
	select {
	case c.out <- m:
	default:
	}
}

// broadcast sends a message to every connected client.
func (h *hub) broadcast(messageType string, data any) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.conns {
		c.send(socketMessage{MessageType: messageType, Data: data})
	}
}

func (h *hub) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.conns)
}
