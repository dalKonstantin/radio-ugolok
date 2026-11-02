package library

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"radio-ugolok/internal/media"
)

func TestCheckFormat(t *testing.T) {
	audio := func(codec string) media.Stream { return media.Stream{CodecType: "audio", CodecName: codec} }
	probe := func(format string, streams ...media.Stream) media.Probe {
		return media.Probe{FormatName: format, Streams: streams}
	}
	cover := media.Stream{CodecType: "video", CodecName: "mjpeg"}
	cases := []struct {
		name string
		p    media.Probe
		want bool
	}{
		{"mp3", probe("mp3", audio("mp3")), true},
		{"mp3 с обложкой", probe("mp3", audio("mp3"), cover), true},
		{"flac", probe("flac", audio("flac")), true},
		{"wav", probe("wav", audio("pcm_s24le")), true},
		{"aiff", probe("aiff", audio("pcm_s16be")), true},
		{"ogg vorbis", probe("ogg", audio("vorbis")), true},
		{"opus", probe("ogg", audio("opus")), true},
		{"m4a aac", probe("mov,mp4,m4a,3gp,3g2,mj2", audio("aac")), true},
		{"m4a alac", probe("mov,mp4,m4a,3gp,3g2,mj2", audio("alac")), true},
		{"wma", probe("asf", audio("wmav2")), true},
		{"flac в ogg", probe("ogg", audio("flac")), false},
		{"два аудиопотока", probe("flac", audio("flac"), audio("flac")), false},
		{"без аудио", probe("tty", media.Stream{CodecType: "video", CodecName: "ansi"}), false},
		{"matroska", probe("matroska,webm", audio("opus")), false},
	}
	for _, c := range cases {
		if got := checkFormat(c.p); got != c.want {
			t.Errorf("%s: checkFormat = %v, want %v", c.name, got, c.want)
		}
	}
}

func openTest(t *testing.T) *Library {
	t.Helper()
	if err := media.CheckTools(); err != nil {
		t.Skip(err)
	}
	l, err := Open(Options{DataDir: t.TempDir(), BitrateKbps: 192, MaxJobs: 2})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	return l
}

func TestImportDedupAndReject(t *testing.T) {
	l := openTest(t)
	ctx := context.Background()

	src := filepath.Join(t.TempDir(), "click.wav")
	if err := media.GenerateTestTrack(ctx, src, 3); err != nil {
		t.Fatal(err)
	}

	first, created, err := l.Import(ctx, src, "Щелчки.wav", "kostya")
	if err != nil || !created {
		t.Fatalf("первая загрузка: created=%v err=%v", created, err)
	}
	if first.Title != "Щелчки" || first.Artist != "Unknown" {
		t.Errorf("теги по умолчанию: %q / %q", first.Title, first.Artist)
	}
	if first.DurationMs < 2900 || first.DurationMs > 3100 {
		t.Errorf("длительность %d мс, ожидалось ~3000", first.DurationMs)
	}
	if peaks, _, err := l.Peaks(ctx, first.ID); err != nil || len(peaks) != media.PeaksCount {
		t.Errorf("пики: len=%d err=%v", len(peaks), err)
	}

	second, created, err := l.Import(ctx, src, "другое имя.wav", "vasya")
	if err != nil || created || second.ID != first.ID || second.UploadedBy != "kostya" {
		t.Fatalf("повторная загрузка: created=%v id=%v by=%q err=%v", created, second.ID == first.ID, second.UploadedBy, err)
	}
	if _, total, _ := l.List(ctx, "", 10, 0); total != 1 {
		t.Errorf("в библиотеке %d записей, ожидалась одна", total)
	}

	fake := filepath.Join(t.TempDir(), "song.mp3")
	if err := os.WriteFile(fake, []byte("это текст, а не музыка"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, err = l.Import(ctx, fake, "song.mp3", "")
	var le *Error
	if !errors.As(err, &le) || le.Code != "unsupported_format" {
		t.Fatalf("текст под видом mp3: err=%v", err)
	}
}

func TestListSearchCyrillic(t *testing.T) {
	l := openTest(t)
	ctx := context.Background()
	for _, row := range [][2]string{{"a", "Ночной Город"}, {"b", "Day_Light 100%"}} {
		_, err := l.db.Exec(`INSERT INTO tracks (id, title, artist, duration_ms, size_bytes, uploaded_at)
			VALUES (?, ?, 'X', 1, 1, 1)`, row[0], row[1])
		if err != nil {
			t.Fatal(err)
		}
	}
	for q, want := range map[string]int{"ночной": 1, "ГОРОД": 1, "100%": 1, "y_l": 1, "%": 1, "zzz": 0, "": 2} {
		if _, total, err := l.List(ctx, q, 10, 0); err != nil || total != want {
			t.Errorf("q=%q: total=%d err=%v, want %d", q, total, err, want)
		}
	}
}
