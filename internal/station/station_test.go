package station

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"radio-ugolok/internal/clock"
	"radio-ugolok/internal/library"
)

// fakeLib — библиотека в памяти. Трогается только из горутины автомата.
type fakeLib struct {
	tracks  map[string]Track
	deleted []string
}

func (f *fakeLib) Get(_ context.Context, id string) (Track, error) {
	t, ok := f.tracks[id]
	if !ok {
		return Track{}, library.ErrNotFound
	}
	return t, nil
}

func (f *fakeLib) MarkPlayed(_ context.Context, id string, at int64) error {
	t := f.tracks[id]
	t.PlayedAt = &at
	f.tracks[id] = t
	return nil
}

func (f *fakeLib) Delete(_ context.Context, id string) error {
	if _, ok := f.tracks[id]; !ok {
		return library.ErrNotFound
	}
	delete(f.tracks, id)
	f.deleted = append(f.deleted, id)
	return nil
}

type harness struct {
	t   *testing.T
	st  *Station
	lib *fakeLib
	out chan Outgoing
	ctx context.Context
}

func newHarness(t *testing.T, opts Options, tracks ...Track) *harness {
	t.Helper()
	lib := &fakeLib{tracks: map[string]Track{}}
	for _, tr := range tracks {
		lib.tracks[tr.ID] = tr
	}
	out := make(chan Outgoing, 1024)
	if opts.QueueLimit == 0 {
		opts.QueueLimit = 3
	}
	st := New(clock.New(), lib, out, opts)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go st.Run(ctx)
	return &harness{t: t, st: st, lib: lib, out: out, ctx: ctx}
}

func track(id string, ms int64) Track {
	return Track{ID: id, Title: "T-" + id, Artist: "A", DurationMs: ms}
}

// expect пропускает сообщения, пока не встретит нужный type (адресованный
// всем или клиенту to), и возвращает его data.
func (h *harness) expect(typ, to string) any {
	h.t.Helper()
	deadline := time.After(8 * time.Second)
	for {
		select {
		case o := <-h.out:
			if o.Msg.Type == typ && (o.To == "" || o.To == to) {
				return o.Msg.Data
			}
		case <-deadline:
			h.t.Fatalf("не дождались %q", typ)
			return nil
		}
	}
}

// drain выбрасывает всё, что уже лежит в канале.
func (h *harness) drain() {
	time.Sleep(20 * time.Millisecond)
	for {
		select {
		case <-h.out:
		default:
			return
		}
	}
}

func (h *harness) snapshot() State {
	h.t.Helper()
	st, err := h.st.Snapshot(h.ctx)
	if err != nil {
		h.t.Fatal(err)
	}
	return st
}

// inStation выполняет f в горутине автомата — так тест читает фейковую
// библиотеку без гонки с таймером.
func (h *harness) inStation(f func()) {
	h.t.Helper()
	done := make(chan struct{})
	if err := h.st.do(h.ctx, func() { f(); close(done) }); err != nil {
		h.t.Fatal(err)
	}
	<-done
}

func (h *harness) expectError(to, code string) {
	h.t.Helper()
	e := h.expect("error", to).(map[string]string)
	if e["code"] != code {
		h.t.Fatalf("ошибка %q, ожидалась %q", e["code"], code)
	}
}

func playingID(np NowPlaying) string {
	if np.Track == nil {
		return ""
	}
	return np.Track.ID
}

func TestSkipRequired(t *testing.T) {
	for n, want := range map[int]int{0: 1, 1: 1, 2: 2, 3: 2, 4: 3, 5: 3, 6: 4, 10: 6, 30: 16} {
		if got := skipRequired(n); got != want {
			t.Errorf("skipRequired(%d) = %d, want %d", n, got, want)
		}
	}
}

func TestNextTrackFromQueueThenIdle(t *testing.T) {
	h := newHarness(t, Options{}, track("a", 100), track("b", 100))
	h.st.Join(h.ctx, "c1", "kostya")
	h.drain()

	if np := h.snapshot().NowPlaying; np.Track != nil {
		t.Fatalf("после старта эфир должен молчать, играет %v", np.Track.ID)
	}

	// Из idle первый трек стартует сразу.
	h.st.Enqueue(h.ctx, "c1", "a")
	if id := playingID(h.expect("nowPlaying", "").(NowPlaying)); id != "a" {
		t.Fatalf("играет %q, ожидался a", id)
	}
	h.st.Enqueue(h.ctx, "c1", "b")
	h.st.Enqueue(h.ctx, "c1", "a") // тот же трек может стоять повторно
	st := h.snapshot()
	if len(st.Queue) != 2 || st.Queue[0].Track.ID != "b" || st.Queue[1].Track.ID != "a" {
		t.Fatalf("очередь %+v", st.Queue)
	}
	var played *int64
	h.inStation(func() { played = h.lib.tracks["a"].PlayedAt })
	if played == nil {
		t.Error("played_at не отмечен")
	}

	// Дальше — по таймеру, первая позиция очереди.
	first := h.expect("nowPlaying", "").(NowPlaying)
	if playingID(first) != "b" || first.AddedBy != "kostya" {
		t.Fatalf("после a играет %q", playingID(first))
	}
	second := h.expect("nowPlaying", "").(NowPlaying)
	if playingID(second) != "a" {
		t.Fatalf("после b играет %q", playingID(second))
	}
	if gap := second.StartedAt - first.StartedAt; gap < 550 || gap > 800 {
		t.Errorf("между стартами %d мс, ожидалось ~600 (длительность + 500)", gap)
	}
	if np := h.expect("nowPlaying", "").(NowPlaying); np.Track != nil {
		t.Fatalf("очередь кончилась, а играет %q", np.Track.ID)
	}
}

func TestQueueLimitPerNick(t *testing.T) {
	h := newHarness(t, Options{QueueLimit: 2}, track("a", 60_000), track("b", 60_000))
	h.st.Join(h.ctx, "c1", "kostya")
	h.st.Join(h.ctx, "c2", "kostya") // та же персона из второй вкладки
	h.st.Join(h.ctx, "c3", "vasya")

	h.st.Enqueue(h.ctx, "c1", "a") // сразу в эфир, в очереди не числится
	h.st.Enqueue(h.ctx, "c1", "b")
	h.st.Enqueue(h.ctx, "c2", "a")
	h.drain()
	h.st.Enqueue(h.ctx, "c2", "b")
	h.expectError("c2", "queue_limit")
	h.st.Enqueue(h.ctx, "c3", "b")
	h.st.Enqueue(h.ctx, "c3", "zzz")
	h.expectError("c3", "unknown_track")
	if n := len(h.snapshot().Queue); n != 3 {
		t.Fatalf("в очереди %d позиций, ожидалось 3", n)
	}

	// Убрать позицию может любой.
	item := h.snapshot().Queue[0].ItemID
	h.st.Dequeue(h.ctx, "c3", item)
	h.st.Dequeue(h.ctx, "c3", item)
	h.expectError("c3", "not_found")
	if n := len(h.snapshot().Queue); n != 2 {
		t.Fatalf("после dequeue %d позиций", n)
	}
}

func TestVoteSkip(t *testing.T) {
	h := newHarness(t, Options{}, track("a", 60_000), track("b", 60_000))
	for _, c := range []string{"c1", "c2", "c3", "c4"} {
		h.st.Join(h.ctx, c, c)
	}
	h.st.Enqueue(h.ctx, "c1", "a")
	h.st.Enqueue(h.ctx, "c1", "b")
	h.drain()

	vote := func(c string) Skip {
		h.st.VoteSkip(h.ctx, c)
		return h.snapshot().Skip
	}
	if s := vote("c1"); s != (Skip{Votes: 1, Required: 3}) {
		t.Fatalf("после первого голоса %+v", s)
	}
	if s := vote("c1"); s.Votes != 0 {
		t.Fatalf("повторный голос должен сниматься, голосов %d", s.Votes)
	}
	vote("c1")
	vote("c2")
	if id := playingID(h.snapshot().NowPlaying); id != "a" {
		t.Fatalf("2 из 4 — не больше половины, а трек сменился на %q", id)
	}

	// Ушёл не голосовавший: 2 из 3 — порог взят.
	h.st.Leave(h.ctx, "c4")
	st := h.snapshot()
	if playingID(st.NowPlaying) != "b" {
		t.Fatalf("после ухода слушателя должен был пройти скип, играет %q", playingID(st.NowPlaying))
	}
	if st.Skip.Votes != 0 {
		t.Errorf("голоса не сброшены при смене трека: %d", st.Skip.Votes)
	}
}

func TestSkipAloneIsImmediate(t *testing.T) {
	h := newHarness(t, Options{}, track("a", 60_000))
	h.st.Join(h.ctx, "c1", "kostya")
	h.st.VoteSkip(h.ctx, "c1")
	h.expectError("c1", "nothing_playing")
	h.st.Enqueue(h.ctx, "c1", "a")
	h.st.VoteSkip(h.ctx, "c1")
	if np := h.snapshot().NowPlaying; np.Track != nil {
		t.Fatal("одинокий слушатель должен скипать сразу")
	}
}

func TestDeleteTrack(t *testing.T) {
	h := newHarness(t, Options{}, track("a", 60_000), track("b", 60_000), track("c", 60_000))
	h.st.Join(h.ctx, "c1", "kostya")
	h.st.Join(h.ctx, "c2", "vasya")
	h.st.Enqueue(h.ctx, "c1", "a")
	h.drain()

	h.st.DeleteTrack(h.ctx, "c1", "a")
	h.expectError("c1", "track_playing")
	h.st.DeleteTrack(h.ctx, "c1", "b")
	h.expectError("c1", "not_played")

	// a отыграл (скип голосами обоих) и снова стоит в очереди дважды.
	h.st.Enqueue(h.ctx, "c1", "c")
	h.st.Enqueue(h.ctx, "c1", "a")
	h.st.Enqueue(h.ctx, "c2", "a")
	h.st.VoteSkip(h.ctx, "c1")
	h.st.VoteSkip(h.ctx, "c2")
	h.drain()

	h.st.DeleteTrack(h.ctx, "c2", "a")
	if got := h.expect("trackRemoved", "").(map[string]string)["trackId"]; got != "a" {
		t.Fatalf("trackRemoved %q", got)
	}
	st := h.snapshot()
	if len(st.Queue) != 0 || playingID(st.NowPlaying) != "c" {
		t.Fatalf("позиции удалённого трека остались: %+v", st.Queue)
	}
}

func TestEphemeral(t *testing.T) {
	if testing.Short() {
		t.Skip("ждёт 5 с")
	}
	h := newHarness(t, Options{Ephemeral: true}, track("a", 50), track("b", 50))
	h.st.Join(h.ctx, "c1", "kostya")
	h.st.Enqueue(h.ctx, "c1", "a")
	h.st.Enqueue(h.ctx, "c1", "b")
	h.st.Enqueue(h.ctx, "c1", "b")
	if got := h.expect("trackRemoved", "").(map[string]string)["trackId"]; got != "a" {
		t.Fatalf("первым должен стереться a, стёрт %q", got)
	}
}

func TestChat(t *testing.T) {
	h := newHarness(t, Options{})
	h.st.Join(h.ctx, "c1", "kostya")
	h.drain()

	h.st.Chat(h.ctx, "c1", "  <b>привет</b>  ")
	m := h.expect("chat", "").(ChatMessage)
	if m.Nick != "kostya" || m.Text != "<b>привет</b>" || m.Kind != "user" {
		t.Fatalf("сообщение %+v", m)
	}
	h.st.Chat(h.ctx, "c1", strings.Repeat("я", chatMaxLen+1))
	h.expectError("c1", "chat_too_long")

	for range chatFloodN - 1 {
		h.st.Chat(h.ctx, "c1", "ещё")
	}
	h.st.Chat(h.ctx, "c1", "лишнее")
	h.expectError("c1", "chat_flood")

	// Новый клиент видит историю, ограниченную chatHistory.
	h.st.do(h.ctx, func() {
		for i := range chatHistory + 10 {
			h.st.system(fmt.Sprintf("событие %d", i))
		}
	})
	h.st.Join(h.ctx, "c2", "vasya")
	st := h.expect("state", "c2").(State)
	if len(st.Chat) != chatHistory {
		t.Fatalf("в истории %d сообщений, ожидалось %d", len(st.Chat), chatHistory)
	}
	if first, last := st.Chat[0].Text, st.Chat[len(st.Chat)-1].Text; first != "событие 10" || last != fmt.Sprintf("событие %d", chatHistory+9) {
		t.Errorf("история %q … %q, ожидались последние %d", first, last, chatHistory)
	}
}
