// Package station — автомат эфира, единственный владелец его состояния:
// текущий трек, очередь, слушатели, голоса за скип, лента чата.
//
// Всё состояние живёт в одной горутине (Run); снаружи туда попадают только
// команды через канал, наружу уходят готовые сообщения для рассылки.
package station

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"radio-ugolok/internal/clock"
	"radio-ugolok/internal/library"
)

const (
	// tailMargin — запас после конца трека, чтобы клиенты успели доиграть хвост.
	tailMargin = 500 * time.Millisecond

	chatHistory   = 200
	chatMaxLen    = 500
	chatFloodN    = 5
	chatFloodSpan = 10 * time.Second

	// ephemeralDelay — через сколько после смены трека стирается отыгравший
	// при -ephemeral.
	ephemeralDelay = 5 * time.Second

	dbTimeout = 5 * time.Second
)

type Track = library.Track

// Library — то, что автомату нужно от библиотеки.
type Library interface {
	Get(ctx context.Context, id string) (Track, error)
	MarkPlayed(ctx context.Context, id string, at int64) error
	Delete(ctx context.Context, id string) error
}

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

type QueueItem struct {
	ItemID  string `json:"itemId"`
	Track   Track  `json:"track"`
	AddedBy string `json:"addedBy"`
}

type NowPlaying struct {
	Track     *Track `json:"track"`
	ItemID    string `json:"itemId,omitempty"`
	StartedAt int64  `json:"startedAt,omitempty"`
	Duration  int64  `json:"duration,omitempty"`
	AddedBy   string `json:"addedBy,omitempty"`
}

type Listeners struct {
	Count int      `json:"count"`
	Nicks []string `json:"nicks"`
}

type Skip struct {
	Votes    int `json:"votes"`
	Required int `json:"required"`
}

type ChatMessage struct {
	ID   string `json:"id"`
	Nick string `json:"nick"`
	Text string `json:"text"`
	At   int64  `json:"at"`
	Kind string `json:"kind"` // user | system
}

type State struct {
	NowPlaying NowPlaying    `json:"nowPlaying"`
	Queue      []QueueItem   `json:"queue"`
	Listeners  Listeners     `json:"listeners"`
	Skip       Skip          `json:"skip"`
	Chat       []ChatMessage `json:"chat"`
}

type Options struct {
	// QueueLimit — сколько позиций в очереди может держать один ник.
	QueueLimit int
	// Ephemeral — стирать трек из библиотеки после проигрывания.
	Ephemeral bool
}

type Station struct {
	clock *clock.Clock
	lib   Library
	opts  Options
	cmds  chan func()
	out   chan<- Outgoing

	// Поля ниже трогает только горутина Run.
	timer     *time.Timer
	current   *QueueItem // nil — эфир молчит (idle)
	startedAt int64
	queue     []QueueItem
	nextItem  int
	listeners map[string]string   // clientId -> nick
	votes     map[string]struct{} // clientId
	chat      []ChatMessage       // последние chatHistory сообщений
	nextChat  int
	chatTimes map[string][]int64 // clientId -> отметки недавних сообщений
}

func New(c *clock.Clock, lib Library, out chan<- Outgoing, opts Options) *Station {
	timer := time.NewTimer(time.Hour)
	timer.Stop()
	return &Station{
		clock:     c,
		lib:       lib,
		opts:      opts,
		cmds:      make(chan func(), 64),
		out:       out,
		timer:     timer,
		listeners: make(map[string]string),
		votes:     make(map[string]struct{}),
		chatTimes: make(map[string][]int64),
	}
}

func (s *Station) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			s.timer.Stop()
			return
		case cmd := <-s.cmds:
			cmd()
		case <-s.timer.C:
			s.advance("")
		}
	}
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

// --- команды -------------------------------------------------------------

func (s *Station) Join(ctx context.Context, clientID, nick string) error {
	return s.do(ctx, func() {
		_, rejoin := s.listeners[clientID]
		s.listeners[clientID] = nick
		s.send(clientID, "state", s.state())
		s.broadcast("listeners", s.listenersMsg())
		s.broadcastSkip()
		if !rejoin {
			s.system(nick + " подключается к эфиру")
		}
	})
}

func (s *Station) Leave(ctx context.Context, clientID string) error {
	return s.do(ctx, func() {
		nick, ok := s.listeners[clientID]
		if !ok {
			return
		}
		delete(s.listeners, clientID)
		delete(s.votes, clientID)
		delete(s.chatTimes, clientID)
		s.broadcast("listeners", s.listenersMsg())
		s.system(nick + " уходит")
		s.broadcastSkip()
		// Порог считается от числа слушателей — с уходом он мог опуститься.
		s.checkSkip()
	})
}

func (s *Station) Resync(ctx context.Context, clientID string) error {
	return s.do(ctx, func() { s.send(clientID, "state", s.state()) })
}

func (s *Station) Enqueue(ctx context.Context, clientID, trackID string) error {
	return s.do(ctx, func() {
		nick := s.nick(clientID)
		mine := 0
		for _, it := range s.queue {
			if it.AddedBy == nick {
				mine++
			}
		}
		if mine >= s.opts.QueueLimit {
			s.fail(clientID, "queue_limit",
				fmt.Sprintf("От одного человека в очереди не больше %d треков — дождитесь, пока ваши сыграют", s.opts.QueueLimit))
			return
		}
		t, err := s.getTrack(trackID)
		if err != nil {
			s.fail(clientID, "unknown_track", "Такого трека нет в библиотеке")
			return
		}
		s.nextItem++
		s.queue = append(s.queue, QueueItem{ItemID: "q-" + strconv.Itoa(s.nextItem), Track: t, AddedBy: nick})
		s.system(fmt.Sprintf("%s ставит «%s»", nick, t.Title))
		if s.current == nil {
			// Из idle эфир выходит сразу, с момента постановки.
			s.advance("")
			return
		}
		s.broadcastQueue()
	})
}

func (s *Station) Dequeue(ctx context.Context, clientID, itemID string) error {
	return s.do(ctx, func() {
		for i, it := range s.queue {
			if it.ItemID == itemID {
				s.queue = append(s.queue[:i], s.queue[i+1:]...)
				s.broadcastQueue()
				return
			}
		}
		s.fail(clientID, "not_found", "Этой позиции уже нет в очереди")
	})
}

func (s *Station) VoteSkip(ctx context.Context, clientID string) error {
	return s.do(ctx, func() {
		if s.current == nil {
			s.fail(clientID, "nothing_playing", "Сейчас ничего не играет")
			return
		}
		if _, ok := s.listeners[clientID]; !ok {
			return
		}
		if _, ok := s.votes[clientID]; ok {
			delete(s.votes, clientID)
		} else {
			s.votes[clientID] = struct{}{}
		}
		s.broadcastSkip()
		s.checkSkip()
	})
}

func (s *Station) Chat(ctx context.Context, clientID, text string) error {
	return s.do(ctx, func() {
		text = strings.TrimSpace(strings.ToValidUTF8(text, ""))
		if text == "" {
			return
		}
		if utf8.RuneCountInString(text) > chatMaxLen {
			s.fail(clientID, "chat_too_long", fmt.Sprintf("Сообщение длиннее %d символов", chatMaxLen))
			return
		}
		now := s.clock.NowMs()
		if !s.allowChat(clientID, now) {
			s.fail(clientID, "chat_flood", "Слишком часто — подождите несколько секунд")
			return
		}
		s.postChat(s.nick(clientID), text, "user", now)
	})
}

func (s *Station) DeleteTrack(ctx context.Context, clientID, trackID string) error {
	return s.do(ctx, func() {
		if s.current != nil && s.current.Track.ID == trackID {
			s.fail(clientID, "track_playing", "Этот трек сейчас в эфире — удалить его нельзя")
			return
		}
		t, err := s.getTrack(trackID)
		if err != nil {
			s.fail(clientID, "unknown_track", "Такого трека нет в библиотеке")
			return
		}
		if t.PlayedAt == nil {
			s.fail(clientID, "not_played", "Удалить можно только трек, который уже звучал")
			return
		}
		if err := s.removeTrack(trackID); err != nil {
			log.Printf("station: удаление %s: %v", trackID[:12], err)
			s.fail(clientID, "internal", "Не удалось удалить трек")
		}
	})
}

// TrackAdded рассылает всем новый трек библиотеки.
func (s *Station) TrackAdded(ctx context.Context, t Track) error {
	return s.do(ctx, func() { s.broadcast("trackAdded", map[string]Track{"track": t}) })
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

// --- переключение треков ---------------------------------------------------

// advance снимает текущий трек и ставит следующий из очереди, а если она
// пуста — переводит эфир в idle. reason уходит в чат системным событием.
func (s *Station) advance(reason string) {
	s.timer.Stop()
	prev := s.current
	s.current = nil
	clear(s.votes)
	if reason != "" {
		s.system(reason)
	}

	if len(s.queue) > 0 {
		next := s.queue[0]
		s.queue = s.queue[1:]
		s.current = &next
		s.startedAt = s.clock.NowMs()
		s.timer.Reset(time.Duration(next.Track.DurationMs)*time.Millisecond + tailMargin)
		s.markPlayed(next.Track.ID)
	}

	s.broadcast("nowPlaying", s.nowPlaying())
	s.broadcastQueue()
	s.broadcastSkip()

	if prev != nil && s.opts.Ephemeral {
		id := prev.Track.ID
		time.AfterFunc(ephemeralDelay, func() {
			_ = s.do(context.Background(), func() { s.expire(id) })
		})
	}
}

func (s *Station) checkSkip() {
	if s.current == nil || len(s.votes) == 0 {
		return
	}
	if len(s.votes) >= skipRequired(len(s.listeners)) {
		s.advance(fmt.Sprintf("«%s» пропущен голосованием", s.current.Track.Title))
	}
}

// skipRequired — сколько голосов нужно для скипа: больше половины
// слушателей, минимум два; одинокий слушатель скипает сам.
func skipRequired(listeners int) int {
	if listeners <= 1 {
		return 1
	}
	return max(2, listeners/2+1)
}

// expire стирает отыгравший трек в режиме -ephemeral, если он за это время
// снова не попал в эфир или в очередь.
func (s *Station) expire(trackID string) {
	if s.current != nil && s.current.Track.ID == trackID {
		return
	}
	for _, it := range s.queue {
		if it.Track.ID == trackID {
			return
		}
	}
	if err := s.removeTrack(trackID); err != nil && !errors.Is(err, library.ErrNotFound) {
		log.Printf("station: ephemeral %s: %v", trackID[:12], err)
	}
}

// removeTrack убирает трек из библиотеки и все его позиции из очереди.
func (s *Station) removeTrack(trackID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), dbTimeout)
	defer cancel()
	if err := s.lib.Delete(ctx, trackID); err != nil {
		return err
	}
	kept := make([]QueueItem, 0, len(s.queue))
	for _, it := range s.queue {
		if it.Track.ID != trackID {
			kept = append(kept, it)
		}
	}
	changed := len(kept) != len(s.queue)
	s.queue = kept
	s.broadcast("trackRemoved", map[string]string{"trackId": trackID})
	if changed {
		s.broadcastQueue()
	}
	return nil
}

func (s *Station) getTrack(id string) (Track, error) {
	ctx, cancel := context.WithTimeout(context.Background(), dbTimeout)
	defer cancel()
	return s.lib.Get(ctx, id)
}

func (s *Station) markPlayed(id string) {
	ctx, cancel := context.WithTimeout(context.Background(), dbTimeout)
	defer cancel()
	if err := s.lib.MarkPlayed(ctx, id, time.Now().UnixMilli()); err != nil {
		log.Printf("station: played_at %s: %v", id, err)
	}
}

// --- чат -----------------------------------------------------------------

// allowChat — не больше chatFloodN сообщений за chatFloodSpan от клиента.
func (s *Station) allowChat(clientID string, now int64) bool {
	cutoff := now - chatFloodSpan.Milliseconds()
	recent := s.chatTimes[clientID][:0]
	for _, t := range s.chatTimes[clientID] {
		if t > cutoff {
			recent = append(recent, t)
		}
	}
	if len(recent) >= chatFloodN {
		s.chatTimes[clientID] = recent
		return false
	}
	s.chatTimes[clientID] = append(recent, now)
	return true
}

func (s *Station) system(text string) {
	s.postChat("", text, "system", s.clock.NowMs())
}

func (s *Station) postChat(nick, text, kind string, at int64) {
	s.nextChat++
	m := ChatMessage{ID: "m-" + strconv.Itoa(s.nextChat), Nick: nick, Text: text, At: at, Kind: kind}
	if len(s.chat) >= chatHistory {
		s.chat = append(s.chat[:0], s.chat[len(s.chat)-chatHistory+1:]...)
	}
	s.chat = append(s.chat, m)
	s.broadcast("chat", m)
}

// --- сообщения -----------------------------------------------------------

func (s *Station) send(clientID, typ string, data any) {
	s.out <- Outgoing{To: clientID, Msg: Message{Type: typ, Data: data}}
}

func (s *Station) broadcast(typ string, data any) {
	s.out <- Outgoing{Msg: Message{Type: typ, Data: data}}
}

func (s *Station) fail(clientID, code, message string) {
	s.send(clientID, "error", map[string]string{"code": code, "message": message})
}

func (s *Station) broadcastQueue() {
	s.broadcast("queue", map[string][]QueueItem{"items": s.queueItems()})
}

func (s *Station) broadcastSkip() { s.broadcast("skip", s.skipMsg()) }

func (s *Station) nick(clientID string) string {
	if n, ok := s.listeners[clientID]; ok {
		return n
	}
	return "гость"
}

func (s *Station) nowPlaying() NowPlaying {
	if s.current == nil {
		return NowPlaying{}
	}
	t := s.current.Track
	return NowPlaying{
		Track:     &t,
		ItemID:    s.current.ItemID,
		StartedAt: s.startedAt,
		Duration:  t.DurationMs,
		AddedBy:   s.current.AddedBy,
	}
}

// Копии срезов: сообщения уходят в другую горутину, а автомат продолжает
// менять свои.
func (s *Station) queueItems() []QueueItem { return append([]QueueItem{}, s.queue...) }

func (s *Station) listenersMsg() Listeners {
	nicks := make([]string, 0, len(s.listeners))
	for _, n := range s.listeners {
		nicks = append(nicks, n)
	}
	sort.Strings(nicks)
	return Listeners{Count: len(nicks), Nicks: nicks}
}

func (s *Station) skipMsg() Skip {
	return Skip{Votes: len(s.votes), Required: skipRequired(len(s.listeners))}
}

func (s *Station) state() State {
	return State{
		NowPlaying: s.nowPlaying(),
		Queue:      s.queueItems(),
		Listeners:  s.listenersMsg(),
		Skip:       s.skipMsg(),
		Chat:       append([]ChatMessage{}, s.chat...),
	}
}
