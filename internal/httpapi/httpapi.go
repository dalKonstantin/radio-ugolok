// Package httpapi — HTTP-роутер: SPA, WebSocket, снимок эфира, библиотека,
// загрузка и раздача файлов через http.ServeContent.
package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"radio-ugolok/internal/library"
	"radio-ugolok/internal/station"
)

// trackID — sha256 содержимого в hex. Проверка заодно закрывает обход путей.
var trackID = regexp.MustCompile(`^[0-9a-f]{64}$`)

const (
	defaultLimit = 50
	maxLimit     = 200
	maxNickLen   = 32
)

type Server struct {
	Station   *station.Station
	Library   *library.Library
	WS        http.HandlerFunc
	Web       fs.FS
	MaxUpload int64 // байт
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /ws", s.WS)
	mux.HandleFunc("GET /api/state", s.state)
	mux.HandleFunc("GET /api/library", s.list)
	mux.HandleFunc("POST /api/upload", s.upload)
	mux.HandleFunc("GET /api/tracks/{id}/audio", s.audio)
	mux.HandleFunc("GET /api/tracks/{id}/cover", s.cover)
	mux.HandleFunc("GET /api/tracks/{id}/peaks", s.peaks)

	static := http.FileServerFS(s.Web)
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		// Фронтенд меняется вместе с бинарником, кешировать его надолго нельзя.
		w.Header().Set("Cache-Control", "no-cache")
		static.ServeHTTP(w, r)
	})
	return mux
}

func (s *Server) state(w http.ResponseWriter, r *http.Request) {
	st, err := s.Station.Snapshot(r.Context())
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "Эфир недоступен")
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) list(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := intParam(q.Get("limit"), defaultLimit, 1, maxLimit)
	offset := intParam(q.Get("offset"), 0, 0, 1<<31-1)
	items, total, err := s.Library.List(r.Context(), q.Get("q"), limit, offset)
	if err != nil {
		log.Printf("httpapi: library: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "Не удалось прочитать библиотеку")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "total": total})
}

// upload принимает multipart с полем file (и необязательным nick), складывает
// файл во временный и прогоняет через конвейер библиотеки.
func (s *Server) upload(w http.ResponseWriter, r *http.Request) {
	// Небольшой запас сверху на заголовки multipart.
	r.Body = http.MaxBytesReader(w, r.Body, s.MaxUpload+64<<10)
	mr, err := r.MultipartReader()
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "Ожидается multipart/form-data с полем file")
		return
	}

	var (
		tmpPath  string
		origName string
		nick     string
	)
	defer func() {
		if tmpPath != "" {
			os.Remove(tmpPath)
		}
	}()

	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			s.uploadReadError(w, err)
			return
		}
		switch part.FormName() {
		case "nick":
			b, _ := io.ReadAll(io.LimitReader(part, 256))
			nick = cleanNick(string(b))
		case "file":
			if tmpPath != "" {
				writeError(w, http.StatusBadRequest, "bad_request", "Загружайте по одному файлу")
				return
			}
			origName = part.FileName()
			tmpPath, err = s.saveUpload(part)
			if err != nil {
				s.uploadReadError(w, err)
				return
			}
		}
		part.Close()
	}
	if tmpPath == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "Нет поля file")
		return
	}

	t, created, err := s.Library.Import(r.Context(), tmpPath, origName, nick)
	if err != nil {
		var le *library.Error
		if errors.As(err, &le) {
			if le.Err != nil {
				log.Printf("httpapi: upload %q: %v", origName, err)
			}
			writeError(w, importStatus(le.Code), le.Code, le.Message)
			return
		}
		log.Printf("httpapi: upload %q: %v", origName, err)
		writeError(w, http.StatusInternalServerError, "internal", "Не удалось сохранить трек")
		return
	}

	status := http.StatusOK
	if created {
		status = http.StatusCreated
		log.Printf("httpapi: загружен %q — %q (%s) от %q", t.Artist, t.Title, t.ID[:12], nick)
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_ = s.Station.TrackAdded(ctx, t)
		cancel()
	}
	writeJSON(w, status, t)
}

func (s *Server) saveUpload(part io.Reader) (string, error) {
	f, err := os.CreateTemp(s.Library.TmpDir(), "upload-*")
	if err != nil {
		return "", err
	}
	n, err := io.Copy(f, io.LimitReader(part, s.MaxUpload+1))
	if err == nil && n > s.MaxUpload {
		err = &http.MaxBytesError{Limit: s.MaxUpload}
	}
	if err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", err
	}
	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

func (s *Server) uploadReadError(w http.ResponseWriter, err error) {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		writeError(w, http.StatusRequestEntityTooLarge, "too_large",
			"Файл больше "+strconv.FormatInt(s.MaxUpload>>20, 10)+" МБ")
		return
	}
	writeError(w, http.StatusBadRequest, "bad_request", "Не удалось принять файл")
}

func importStatus(code string) int {
	switch code {
	case "unsupported_format":
		return http.StatusUnsupportedMediaType
	case "transcode_failed":
		return http.StatusUnprocessableEntity
	default:
		return http.StatusInternalServerError
	}
}

func (s *Server) audio(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	serveFile(w, r, s.Library.AudioPath(id), id, "audio/mpeg")
}

func (s *Server) cover(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	serveFile(w, r, s.Library.CoverPath(id), id, "image/jpeg")
}

func (s *Server) peaks(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	peaks, uploadedAt, err := s.Library.Peaks(r.Context(), id)
	if err != nil {
		if errors.Is(err, library.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "Трек не найден")
		} else {
			writeError(w, http.StatusInternalServerError, "internal", "Не удалось прочитать пики")
		}
		return
	}
	setImmutable(w, id, "application/octet-stream")
	http.ServeContent(w, r, "", time.UnixMilli(uploadedAt), bytes.NewReader(peaks))
}

func pathID(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := r.PathValue("id")
	if !trackID.MatchString(id) {
		writeError(w, http.StatusNotFound, "not_found", "Трек не найден")
		return "", false
	}
	return id, true
}

// serveFile отдаёт файл через ServeContent: Range, If-Range, Last-Modified.
func serveFile(w http.ResponseWriter, r *http.Request, path, id, contentType string) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			writeError(w, http.StatusNotFound, "not_found", "Файл не найден")
		} else {
			writeError(w, http.StatusInternalServerError, "internal", "Не удалось открыть файл")
		}
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "Не удалось открыть файл")
		return
	}
	setImmutable(w, id, contentType)
	http.ServeContent(w, r, "", fi.ModTime(), f)
}

// Имя файла — хеш содержимого исходника, значит по URL содержимое не меняется.
func setImmutable(w http.ResponseWriter, id, contentType string) {
	h := w.Header()
	h.Set("Content-Type", contentType)
	h.Set("Cache-Control", "public, max-age=31536000, immutable")
	h.Set("ETag", `"`+id+`"`)
}

func intParam(s string, def, lo, hi int) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return min(max(n, lo), hi)
}

func cleanNick(nick string) string {
	nick = strings.TrimSpace(strings.ToValidUTF8(nick, ""))
	if utf8.RuneCountInString(nick) > maxNickLen {
		nick = string([]rune(nick)[:maxNickLen])
	}
	return nick
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]string{"code": code, "message": message},
	})
}
