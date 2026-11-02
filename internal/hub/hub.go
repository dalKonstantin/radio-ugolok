// Package hub держит WebSocket-соединения: принимает команды клиентов,
// передаёт их автомату эфира и рассылает его сообщения.
//
// Реестр клиентов принадлежит горутине Run. pong отправляется прямо из
// читающей горутины, мимо очереди рассылки, чтобы её задержка не портила
// оценку сдвига часов.
package hub

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/coder/websocket"

	"radio-ugolok/internal/clock"
	"radio-ugolok/internal/station"
)

const (
	// readTimeout — сколько ждём любого сообщения от клиента. Клиент шлёт
	// keepalive-ping каждые 20 с.
	readTimeout  = 60 * time.Second
	writeTimeout = 10 * time.Second
	sendBuffer   = 64
	maxNickLen   = 32
)

type client struct {
	id   string
	send chan []byte
}

type Hub struct {
	clock      *clock.Clock
	station    *station.Station
	in         <-chan station.Outgoing
	register   chan *client
	unregister chan *client
}

func New(c *clock.Clock, st *station.Station, in <-chan station.Outgoing) *Hub {
	return &Hub{
		clock:      c,
		station:    st,
		in:         in,
		register:   make(chan *client),
		unregister: make(chan *client),
	}
}

func (h *Hub) Run(ctx context.Context) {
	clients := make(map[string]*client)
	drop := func(c *client) {
		if clients[c.id] == c {
			delete(clients, c.id)
			close(c.send)
		}
	}
	deliver := func(c *client, b []byte) {
		select {
		case c.send <- b:
		default:
			log.Printf("hub: клиент %s не успевает читать, отключаем", c.id)
			drop(c)
		}
	}

	for {
		select {
		case <-ctx.Done():
			for _, c := range clients {
				drop(c)
			}
			return
		case c := <-h.register:
			clients[c.id] = c
		case c := <-h.unregister:
			drop(c)
		case out := <-h.in:
			b, err := json.Marshal(out.Msg)
			if err != nil {
				log.Printf("hub: marshal %s: %v", out.Msg.Type, err)
				continue
			}
			if out.To != "" {
				if c, ok := clients[out.To]; ok {
					deliver(c, b)
				}
				continue
			}
			for _, c := range clients {
				deliver(c, b)
			}
		}
	}
}

type inbound struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

func (h *Hub) ServeWS(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer conn.CloseNow()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	c := &client{id: newID(), send: make(chan []byte, sendBuffer)}

	if err := writeJSON(ctx, conn, station.Message{Type: "hello", Data: map[string]any{
		"clientId": c.id, "serverTime": h.clock.NowMs(),
	}}); err != nil {
		return
	}

	select {
	case h.register <- c:
	case <-ctx.Done():
		return
	}
	defer func() {
		// Run к этому моменту может уже завершиться, поэтому не блокируемся.
		select {
		case h.unregister <- c:
		case <-time.After(time.Second):
		}
		leaveCtx, leaveCancel := context.WithTimeout(context.Background(), time.Second)
		defer leaveCancel()
		_ = h.station.Leave(leaveCtx, c.id)
	}()

	go h.writeLoop(ctx, cancel, conn, c)

	for {
		readCtx, readCancel := context.WithTimeout(ctx, readTimeout)
		_, data, err := conn.Read(readCtx)
		readCancel()
		if err != nil {
			if !isNormalClose(err) {
				log.Printf("hub: %s: чтение: %v", c.id, err)
			}
			return
		}
		var msg inbound
		if err := json.Unmarshal(data, &msg); err != nil {
			h.sendError(ctx, conn, "bad_message", "Не удалось разобрать сообщение")
			continue
		}
		if err := h.handle(ctx, conn, c, msg); err != nil {
			return
		}
	}
}

func (h *Hub) handle(ctx context.Context, conn *websocket.Conn, c *client, msg inbound) error {
	switch msg.Type {
	case "ping":
		var d struct {
			T0 int64 `json:"t0"`
		}
		_ = json.Unmarshal(msg.Data, &d)
		return writeJSON(ctx, conn, station.Message{Type: "pong", Data: map[string]int64{
			"t0": d.T0, "tServer": h.clock.NowMs(),
		}})
	case "join":
		var d struct {
			Nick string `json:"nick"`
		}
		_ = json.Unmarshal(msg.Data, &d)
		return h.station.Join(ctx, c.id, cleanNick(d.Nick))
	case "resync":
		return h.station.Resync(ctx, c.id)
	default:
		h.sendError(ctx, conn, "unknown_command", "Неизвестная команда: "+msg.Type)
		return nil
	}
}

func (h *Hub) writeLoop(ctx context.Context, cancel context.CancelFunc, conn *websocket.Conn, c *client) {
	defer cancel()
	for {
		select {
		case <-ctx.Done():
			return
		case b, ok := <-c.send:
			if !ok {
				conn.Close(websocket.StatusPolicyViolation, "слишком медленное чтение")
				return
			}
			wctx, wcancel := context.WithTimeout(ctx, writeTimeout)
			err := conn.Write(wctx, websocket.MessageText, b)
			wcancel()
			if err != nil {
				return
			}
		}
	}
}

func (h *Hub) sendError(ctx context.Context, conn *websocket.Conn, code, message string) {
	_ = writeJSON(ctx, conn, station.Message{Type: "error", Data: map[string]string{
		"code": code, "message": message,
	}})
}

// writeJSON пишет сообщение напрямую в соединение. Conn.Write в
// coder/websocket безопасен для конкурентных вызовов.
func writeJSON(ctx context.Context, conn *websocket.Conn, m station.Message) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	return conn.Write(wctx, websocket.MessageText, b)
}

func cleanNick(nick string) string {
	nick = strings.TrimSpace(nick)
	if utf8.RuneCountInString(nick) > maxNickLen {
		nick = string([]rune(nick)[:maxNickLen])
	}
	if nick == "" {
		nick = "гость"
	}
	return nick
}

func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func isNormalClose(err error) bool {
	s := websocket.CloseStatus(err)
	return s == websocket.StatusNormalClosure || s == websocket.StatusGoingAway ||
		errors.Is(err, context.Canceled) || errors.Is(err, io.EOF)
}
