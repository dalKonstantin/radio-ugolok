package library

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/dhowden/tag"

	"radio-ugolok/internal/media"
)

// Error — отказ конвейера загрузки с кодом для клиента.
type Error struct {
	Code    string
	Message string
	Err     error
}

func (e *Error) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s: %v", e.Code, e.Err)
	}
	return e.Code
}

func (e *Error) Unwrap() error { return e.Err }

const maxTagLen = 200

// Принимаемые контейнеры (имя из format_name ffprobe) и кодеки в них.
var acceptedFormats = []struct {
	containers []string
	codecs     func(string) bool
}{
	{[]string{"mp3"}, is("mp3")},
	{[]string{"flac"}, is("flac")},
	{[]string{"wav", "aiff"}, func(c string) bool { return strings.HasPrefix(c, "pcm_") }},
	{[]string{"ogg"}, is("vorbis", "opus")},
	{[]string{"mov", "mp4", "m4a"}, is("aac", "alac")},
	{[]string{"asf"}, is("wmav1", "wmav2", "wmapro")},
}

func is(codecs ...string) func(string) bool {
	return func(c string) bool {
		for _, x := range codecs {
			if c == x {
				return true
			}
		}
		return false
	}
}

// checkFormat проверяет, что в файле ровно один аудиопоток в принимаемом
// контейнере. Расширение не смотрится вообще.
func checkFormat(p media.Probe) bool {
	var audio []media.Stream
	for _, s := range p.Streams {
		if s.CodecType == "audio" {
			audio = append(audio, s)
		}
	}
	if len(audio) != 1 {
		return false
	}
	names := strings.Split(p.FormatName, ",")
	for _, f := range acceptedFormats {
		for _, c := range f.containers {
			for _, n := range names {
				if n == c && f.codecs(audio[0].CodecName) {
					return true
				}
			}
		}
	}
	return false
}

// Import прогоняет файл src через конвейер загрузки и кладёт трек в
// библиотеку. Если такой файл уже загружали, возвращает существующую запись
// и created=false. src не удаляется — это забота вызывающего.
func (l *Library) Import(ctx context.Context, src, origName, uploadedBy string) (t Track, created bool, err error) {
	select {
	case l.jobs <- struct{}{}:
		defer func() { <-l.jobs }()
	case <-ctx.Done():
		return Track{}, false, ctx.Err()
	}

	// 1. Формат — по содержимому.
	probe, err := media.ProbeFile(ctx, src)
	if errors.Is(err, media.ErrNotMedia) || (err == nil && !checkFormat(probe)) {
		return Track{}, false, &Error{Code: "unsupported_format",
			Message: "Неподдерживаемый формат: нужен файл с одним аудиопотоком — mp3, flac, wav, aiff, ogg, opus, m4a или wma"}
	}
	if err != nil {
		return Track{}, false, &Error{Code: "probe_failed", Message: "Не удалось прочитать файл", Err: err}
	}

	// 2. Дедупликация по хешу исходника.
	id, err := media.HashFile(src)
	if err != nil {
		return Track{}, false, err
	}
	if existing, err := l.Get(ctx, id); err == nil {
		return existing, false, nil
	} else if !errors.Is(err, ErrNotFound) {
		return Track{}, false, err
	}

	// 5. Теги — из оригинала, до перекодирования.
	t = Track{ID: id, UploadedBy: uploadedBy, UploadedAt: time.Now().UnixMilli()}
	pic := readTags(src, &t)
	if t.Title == "" {
		t.Title = cleanTag(strings.TrimSuffix(filepath.Base(origName), filepath.Ext(origName)))
	}
	if t.Title == "" {
		t.Title = "Без названия"
	}
	if t.Artist == "" {
		t.Artist = "Unknown"
	}

	// Всё, что успели положить на диск, убираем, если дальше что-то сорвалось.
	audioPath, coverPath := l.AudioPath(id), l.CoverPath(id)
	ok := false
	defer func() {
		if !ok {
			os.Remove(audioPath)
			os.Remove(coverPath)
		}
	}()

	// 3. Перекодирование в эфирный формат.
	if err := l.atomicWrite(audioPath, func(tmp string) error {
		return media.Transcode(ctx, src, tmp, l.bitrate)
	}); err != nil {
		return Track{}, false, &Error{Code: "transcode_failed", Message: "Не удалось перекодировать файл", Err: err}
	}

	// 4. Длительность — по готовому mp3: на ней держится синхронизация.
	if t.DurationMs, err = media.DurationMs(ctx, audioPath); err != nil {
		return Track{}, false, &Error{Code: "probe_failed", Message: "Не удалось определить длительность", Err: err}
	}
	fi, err := os.Stat(audioPath)
	if err != nil {
		return Track{}, false, err
	}
	t.SizeBytes = fi.Size()

	// 6. Обложка. Битая картинка не повод отказывать в загрузке.
	if pic != nil {
		err := l.atomicWrite(coverPath, func(tmp string) error {
			return media.Cover(ctx, pic.Data, tmp)
		})
		if err != nil {
			log.Printf("library: %s: обложка: %v", id[:12], err)
		} else {
			t.HasCover = true
		}
	}

	// 7. Пики — по эфирному mp3, рисуем ровно то, что слышно.
	peaks, err := media.Peaks(ctx, audioPath, t.DurationMs)
	if err != nil {
		return Track{}, false, &Error{Code: "transcode_failed", Message: "Не удалось посчитать waveform", Err: err}
	}

	// Тот же файл могли параллельно загрузить из другой вкладки — тогда
	// запись уже есть и файлы на диске те же.
	res, err := l.db.ExecContext(ctx, `INSERT OR IGNORE INTO tracks
		(id, title, artist, album, duration_ms, size_bytes, has_cover, peaks, uploaded_by, uploaded_at)
		VALUES (?, ?, ?, NULLIF(?, ''), ?, ?, ?, ?, NULLIF(?, ''), ?)`,
		t.ID, t.Title, t.Artist, t.Album, t.DurationMs, t.SizeBytes, t.HasCover, peaks,
		t.UploadedBy, t.UploadedAt)
	if err != nil {
		return Track{}, false, err
	}
	ok = true
	if n, _ := res.RowsAffected(); n == 0 {
		existing, err := l.Get(ctx, id)
		return existing, false, err
	}
	return t, true, nil
}

// atomicWrite даёт write временный путь рядом с dst и переименовывает
// результат в dst, чтобы недописанный файл никогда не лежал под итоговым именем.
func (l *Library) atomicWrite(dst string, write func(tmp string) error) error {
	f, err := os.CreateTemp(l.tmpDir, "out-*"+filepath.Ext(dst))
	if err != nil {
		return err
	}
	tmp := f.Name()
	f.Close()
	if err := write(tmp); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// readTags заполняет title, artist, album и возвращает встроенную обложку.
// Файлы без тегов (wav, aiff) — не ошибка.
func readTags(src string, t *Track) *tag.Picture {
	f, err := os.Open(src)
	if err != nil {
		return nil
	}
	defer f.Close()
	m, err := tag.ReadFrom(f)
	if err != nil {
		return nil
	}
	t.Title = cleanTag(m.Title())
	t.Artist = cleanTag(m.Artist())
	if t.Artist == "" {
		t.Artist = cleanTag(m.AlbumArtist())
	}
	t.Album = cleanTag(m.Album())
	if p := m.Picture(); p != nil && len(p.Data) > 0 {
		return p
	}
	return nil
}

func cleanTag(s string) string {
	s = strings.ToValidUTF8(s, "")
	s = strings.Map(func(r rune) rune {
		if r < 0x20 {
			return -1
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) > maxTagLen {
		s = string([]rune(s)[:maxTagLen])
	}
	return s
}
