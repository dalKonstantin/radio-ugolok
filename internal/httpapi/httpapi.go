// Package httpapi — HTTP-роутер: SPA, WebSocket, снимок эфира и раздача
// эфирных файлов через http.ServeContent.
package httpapi

import (
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"

	"radio-ugolok/internal/station"
)

// trackID — sha256 содержимого в hex. Проверка заодно закрывает обход путей.
var trackID = regexp.MustCompile(`^[0-9a-f]{64}$`)

type Server struct {
	Station   *station.Station
	WS        http.HandlerFunc
	Web       fs.FS
	TracksDir string
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /ws", s.WS)
	mux.HandleFunc("GET /api/state", s.state)
	mux.HandleFunc("GET /api/tracks/{id}/audio", s.audio)

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
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(st)
}

func (s *Server) audio(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !trackID.MatchString(id) {
		writeError(w, http.StatusNotFound, "not_found", "Трек не найден")
		return
	}
	f, err := os.Open(filepath.Join(s.TracksDir, id+".mp3"))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			writeError(w, http.StatusNotFound, "not_found", "Трек не найден")
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
	// Имя файла — хеш содержимого, значит по этому URL содержимое не меняется.
	h := w.Header()
	h.Set("Content-Type", "audio/mpeg")
	h.Set("Cache-Control", "public, max-age=31536000, immutable")
	h.Set("ETag", `"`+id+`"`)
	http.ServeContent(w, r, "", fi.ModTime(), f)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{"code": code, "message": message},
	})
}
