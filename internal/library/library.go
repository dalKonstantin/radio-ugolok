// Package library — библиотека треков: метаданные в SQLite, эфирные mp3
// и обложки на диске, конвейер загрузки.
package library

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS tracks (
  id          TEXT PRIMARY KEY,
  title       TEXT NOT NULL,
  artist      TEXT NOT NULL,
  album       TEXT,
  duration_ms INTEGER NOT NULL,
  size_bytes  INTEGER NOT NULL,
  has_cover   INTEGER NOT NULL DEFAULT 0,
  peaks       BLOB,
  uploaded_by TEXT,
  uploaded_at INTEGER NOT NULL,
  played_at   INTEGER
);
CREATE INDEX IF NOT EXISTS idx_tracks_title  ON tracks(title);
CREATE INDEX IF NOT EXISTS idx_tracks_artist ON tracks(artist);
`

// Встроенный lower() в SQLite понимает только ASCII, а названия бывают
// кириллицей — для поиска регистрируем свой.
func init() {
	err := sqlite.RegisterDeterministicScalarFunction("ulower", 1,
		func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
			s, _ := args[0].(string)
			return strings.ToLower(s), nil
		})
	if err != nil {
		panic(err)
	}
}

// ErrNotFound — трека нет в библиотеке.
var ErrNotFound = errors.New("трек не найден")

type Track struct {
	ID         string `json:"id"`
	Title      string `json:"title"`
	Artist     string `json:"artist"`
	Album      string `json:"album,omitempty"`
	DurationMs int64  `json:"durationMs"`
	SizeBytes  int64  `json:"sizeBytes"`
	HasCover   bool   `json:"hasCover"`
	UploadedBy string `json:"uploadedBy,omitempty"`
	UploadedAt int64  `json:"uploadedAt"`
	PlayedAt   *int64 `json:"playedAt"`
}

type Options struct {
	DataDir     string
	BitrateKbps int
	// MaxJobs — сколько загрузок обрабатывается одновременно.
	MaxJobs int
}

type Library struct {
	db        *sql.DB
	tracksDir string
	coversDir string
	tmpDir    string
	bitrate   int
	jobs      chan struct{}
}

func Open(opts Options) (*Library, error) {
	l := &Library{
		tracksDir: filepath.Join(opts.DataDir, "tracks"),
		coversDir: filepath.Join(opts.DataDir, "covers"),
		tmpDir:    filepath.Join(opts.DataDir, "tmp"),
		bitrate:   opts.BitrateKbps,
		jobs:      make(chan struct{}, max(1, opts.MaxJobs)),
	}
	for _, dir := range []string{l.tracksDir, l.coversDir, l.tmpDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}

	dsn := "file:" + filepath.Join(opts.DataDir, "radio.db") +
		"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("миграция: %w", err)
	}
	l.db = db
	return l, nil
}

func (l *Library) Close() error { return l.db.Close() }

// TmpDir — каталог для временных файлов загрузки, на том же диске, что и
// библиотека.
func (l *Library) TmpDir() string { return l.tmpDir }

func (l *Library) AudioPath(id string) string { return filepath.Join(l.tracksDir, id+".mp3") }
func (l *Library) CoverPath(id string) string { return filepath.Join(l.coversDir, id+".jpg") }

const trackColumns = `id, title, artist, album, duration_ms, size_bytes, has_cover,
	uploaded_by, uploaded_at, played_at`

func scanTrack(row interface{ Scan(...any) error }) (Track, error) {
	var (
		t         Track
		album, by sql.NullString
		playedAt  sql.NullInt64
	)
	err := row.Scan(&t.ID, &t.Title, &t.Artist, &album, &t.DurationMs, &t.SizeBytes,
		&t.HasCover, &by, &t.UploadedAt, &playedAt)
	if err != nil {
		return Track{}, err
	}
	t.Album = album.String
	t.UploadedBy = by.String
	if playedAt.Valid {
		t.PlayedAt = &playedAt.Int64
	}
	return t, nil
}

func (l *Library) Get(ctx context.Context, id string) (Track, error) {
	t, err := scanTrack(l.db.QueryRowContext(ctx,
		`SELECT `+trackColumns+` FROM tracks WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Track{}, ErrNotFound
	}
	return t, err
}

// Peaks возвращает сохранённые пики трека.
func (l *Library) Peaks(ctx context.Context, id string) ([]byte, int64, error) {
	var (
		peaks      []byte
		uploadedAt int64
	)
	err := l.db.QueryRowContext(ctx,
		`SELECT peaks, uploaded_at FROM tracks WHERE id = ?`, id).Scan(&peaks, &uploadedAt)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && peaks == nil) {
		return nil, 0, ErrNotFound
	}
	return peaks, uploadedAt, err
}

// List возвращает страницу библиотеки, свежие загрузки первыми, с поиском
// по подстроке в title и artist без учёта регистра.
func (l *Library) List(ctx context.Context, q string, limit, offset int) ([]Track, int, error) {
	where, args := "", []any{}
	if q = strings.TrimSpace(q); q != "" {
		pattern := "%" + likeEscaper.Replace(strings.ToLower(q)) + "%"
		where = `WHERE ulower(title) LIKE ? ESCAPE '\' OR ulower(artist) LIKE ? ESCAPE '\'`
		args = append(args, pattern, pattern)
	}

	var total int
	if err := l.db.QueryRowContext(ctx, `SELECT count(*) FROM tracks `+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	rows, err := l.db.QueryContext(ctx,
		`SELECT `+trackColumns+` FROM tracks `+where+
			` ORDER BY uploaded_at DESC, id LIMIT ? OFFSET ?`,
		append(args, limit, offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	items := []Track{}
	for rows.Next() {
		t, err := scanTrack(rows)
		if err != nil {
			return nil, 0, err
		}
		items = append(items, t)
	}
	return items, total, rows.Err()
}

var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
