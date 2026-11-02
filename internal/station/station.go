// Package station — автомат эфира, единственный владелец его состояния.
// Всё состояние живёт в одной горутине (Run); снаружи туда попадают только
// команды через канал, наружу уходят готовые сообщения для рассылки.
//
// M1: в эфире один захардкоженный трек, который крутится по кругу.
package station

import (
	"context"
	"sort"
	"time"

	"radio-ugolok/internal/clock"
)

// tailMargin — запас после конца трека, чтобы клиенты успели доиграть хвост.
const tailMargin = 500 * time.Millisecond

// Message — конверт протокола {"type": "...", "data": {...}}.
type Message struct {
	Type string `json:"type"`
	Data any    `json:"data"`
}

// Outgoing — сообщение для хаба. Пустой To означает рассылку всем.
type Outgoing struct {
	To  string
	Msg Message
}

type Track struct {
	ID         string `json:"id"`
	Title      string `json:"title"`
	Artist     string `json:"artist"`
	DurationMs int64  `json:"durationMs"`
	HasCover   bool   `json:"hasCover"`
}

type NowPlaying struct {
	Track     *Track `json:"track"`
	StartedAt int64  `json:"startedAt,omitempty"`
	Duration  int64  `json:"duration,omitempty"`
	AddedBy   string `json:"addedBy,omitempty"`
}

type Listeners struct {
	Count int      `json:"count"`
	Nicks []string `json:"nicks"`
}

type State struct {
	NowPlaying NowPlaying `json:"nowPlaying"`
	Queue      []any      `json:"queue"`
	Listeners  Listeners  `json:"listeners"`
	Skip       any        `json:"skip"`
	Chat       []any      `json:"chat"`
}

type Station struct {
	clock *clock.Clock
	cmds  chan func()
	out   chan<- Outgoing

	// Поля ниже трогает только горутина Run.
	track     Track
	startedAt int64
	listeners map[string]string // clientId -> nick
}

func New(c *clock.Clock, track Track, out chan<- Outgoing) *Station {
	return &Station{
		clock:     c,
		cmds:      make(chan func(), 64),
		out:       out,
		track:     track,
		listeners: make(map[string]string),
	}
}

func (s *Station) Run(ctx context.Context) {
	timer := time.NewTimer(0)
	s.start(timer)
	for {
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case cmd := <-s.cmds:
			cmd()
		case <-timer.C:
			s.start(timer)
			s.out <- Outgoing{Msg: Message{Type: "nowPlaying", Data: s.nowPlaying()}}
		}
	}
}

// start ставит трек в эфир с текущего момента и заводит таймер на его конец.
func (s *Station) start(timer *time.Timer) {
	s.startedAt = s.clock.NowMs()
	timer.Reset(time.Duration(s.track.DurationMs)*time.Millisecond + tailMargin)
}

// do выполняет f в горутине автомата. Ошибка только если контекст закончился.
func (s *Station) do(ctx context.Context, f func()) error {
	select {
	case s.cmds <- f:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Station) Join(ctx context.Context, clientID, nick string) error {
	return s.do(ctx, func() {
		s.listeners[clientID] = nick
		s.out <- Outgoing{To: clientID, Msg: Message{Type: "state", Data: s.state()}}
		s.out <- Outgoing{Msg: Message{Type: "listeners", Data: s.listenersMsg()}}
	})
}

func (s *Station) Leave(ctx context.Context, clientID string) error {
	return s.do(ctx, func() {
		if _, ok := s.listeners[clientID]; !ok {
			return
		}
		delete(s.listeners, clientID)
		s.out <- Outgoing{Msg: Message{Type: "listeners", Data: s.listenersMsg()}}
	})
}

func (s *Station) Resync(ctx context.Context, clientID string) error {
	return s.do(ctx, func() {
		s.out <- Outgoing{To: clientID, Msg: Message{Type: "state", Data: s.state()}}
	})
}

// Snapshot возвращает текущее состояние эфира (для GET /api/state).
func (s *Station) Snapshot(ctx context.Context) (State, error) {
	reply := make(chan State, 1)
	if err := s.do(ctx, func() { reply <- s.state() }); err != nil {
		return State{}, err
	}
	select {
	case st := <-reply:
		return st, nil
	case <-ctx.Done():
		return State{}, ctx.Err()
	}
}

func (s *Station) nowPlaying() NowPlaying {
	t := s.track
	return NowPlaying{Track: &t, StartedAt: s.startedAt, Duration: t.DurationMs}
}

func (s *Station) listenersMsg() Listeners {
	nicks := make([]string, 0, len(s.listeners))
	for _, n := range s.listeners {
		nicks = append(nicks, n)
	}
	sort.Strings(nicks)
	return Listeners{Count: len(nicks), Nicks: nicks}
}

func (s *Station) state() State {
	return State{
		NowPlaying: s.nowPlaying(),
		Queue:      []any{},
		Listeners:  s.listenersMsg(),
		Skip:       map[string]int{"votes": 0, "required": 0},
		Chat:       []any{},
	}
}
