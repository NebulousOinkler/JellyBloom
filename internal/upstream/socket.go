package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// EventType classifies events delivered by Listen.
type EventType string

const (
	// EventConnected fires on every (re)connect. Changes made while disconnected
	// are not replayed, so consumers should resync after a reconnect.
	EventConnected EventType = "Connected"
	// EventDisconnected fires when a connection ends; Err holds the cause.
	EventDisconnected EventType = "Disconnected"
	// EventLibraryChanged carries an upstream LibraryChanged message.
	EventLibraryChanged EventType = "LibraryChanged"
	// EventMessage is any other upstream message, passed through raw.
	EventMessage EventType = "Message"
)

// LibraryChange is the payload of Jellyfin's LibraryChanged message.
type LibraryChange struct {
	ItemsAdded         []string
	ItemsUpdated       []string
	ItemsRemoved       []string
	FoldersAddedTo     []string
	FoldersRemovedFrom []string
	CollectionFolders  []string
}

// Event is one notification from an upstream's /socket.
type Event struct {
	Upstream    string
	Type        EventType
	MessageType string          // Jellyfin MessageType, for EventMessage and EventLibraryChanged
	Library     *LibraryChange  // EventLibraryChanged only
	Data        json.RawMessage // raw message Data
	Err         error           // EventDisconnected only
}

type socketMessage struct {
	MessageType string
	Data        json.RawMessage `json:",omitempty"`
}

// stableConn is how long a connection must last before the reconnect backoff resets.
const stableConn = 30 * time.Second

// Listen connects to the upstream's /socket and delivers events to handle until
// ctx is cancelled, reconnecting with backoff. handle is called from a single
// goroutine and should not block for long. Listen returns ctx.Err().
func (c *Client) Listen(ctx context.Context, handle func(Event)) error {
	for attempt := 0; ; attempt++ {
		connectedFor, err := c.listenOnce(ctx, handle)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		handle(Event{Upstream: c.cfg.Name, Type: EventDisconnected, Err: err})
		if connectedFor >= stableConn {
			attempt = 0
		}
		delay := c.backoff(attempt)
		var ce *CredentialError
		if errors.As(err, &ce) {
			delay = max(delay, time.Minute) // needs an operator; don't hammer
		}
		if err := sleep(ctx, delay); err != nil {
			return err
		}
	}
}

// listenOnce runs a single connection and returns how long it stayed connected.
func (c *Client) listenOnce(ctx context.Context, handle func(Event)) (time.Duration, error) {
	if err := c.ensureLogin(ctx, ""); err != nil {
		return 0, err
	}
	token, _ := c.session()
	conn, resp, err := websocket.Dial(ctx, c.socketURL(token), &websocket.DialOptions{
		HTTPClient: c.hc,
		HTTPHeader: http.Header{"Authorization": {c.authHeader(token)}},
	})
	if err != nil {
		// Jellyfin answers a revoked token with 403 on the socket (401 elsewhere).
		if resp != nil && (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) {
			if lerr := c.ensureLogin(ctx, token); lerr != nil {
				return 0, lerr
			}
		}
		return 0, err
	}
	defer conn.CloseNow()
	conn.SetReadLimit(16 << 20)

	start := time.Now()
	handle(Event{Upstream: c.cfg.Name, Type: EventConnected})

	connCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	keepAlive := make(chan time.Duration, 1)
	go c.keepAlive(connCtx, conn, keepAlive)

	for {
		var msg socketMessage
		if err := wsjson.Read(connCtx, conn, &msg); err != nil {
			return time.Since(start), err
		}
		switch msg.MessageType {
		case "ForceKeepAlive":
			var secs float64
			if json.Unmarshal(msg.Data, &secs) == nil && secs > 0 {
				select {
				case keepAlive <- time.Duration(secs * float64(time.Second)):
				default:
				}
			}
		case "KeepAlive":
			// Upstream's acknowledgement of our keepalive.
		case "LibraryChanged":
			var lc LibraryChange
			if err := json.Unmarshal(msg.Data, &lc); err != nil {
				return time.Since(start), err
			}
			handle(Event{Upstream: c.cfg.Name, Type: EventLibraryChanged, MessageType: msg.MessageType, Library: &lc, Data: msg.Data})
		default:
			handle(Event{Upstream: c.cfg.Name, Type: EventMessage, MessageType: msg.MessageType, Data: msg.Data})
		}
	}
}

// keepAlive sends KeepAlive messages at half the timeout the upstream requests
// via ForceKeepAlive. It is the connection's only writer.
func (c *Client) keepAlive(ctx context.Context, conn *websocket.Conn, timeouts <-chan time.Duration) {
	var tick <-chan time.Time
	var ticker *time.Ticker
	defer func() {
		if ticker != nil {
			ticker.Stop()
		}
	}()
	send := func() bool {
		err := wsjson.Write(ctx, conn, socketMessage{MessageType: "KeepAlive"})
		return err == nil || errors.Is(err, context.Canceled)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case d := <-timeouts:
			if ticker != nil {
				ticker.Stop()
			}
			ticker = time.NewTicker(max(d/2, 100*time.Millisecond))
			tick = ticker.C
			if !send() {
				return
			}
		case <-tick:
			if !send() {
				return
			}
		}
	}
}

func (c *Client) socketURL(token string) string {
	u := *c.base
	if u.Scheme == "https" {
		u.Scheme = "wss"
	} else {
		u.Scheme = "ws"
	}
	u.Path = c.base.Path + "/socket"
	u.RawQuery = url.Values{"api_key": {token}, "deviceId": {c.deviceID}}.Encode()
	return u.String()
}
