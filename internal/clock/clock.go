// Package clock даёт серверное время эфира: Unix-миллисекунды, отсчитываемые
// по монотонным часам от точки старта процесса. Перевод системного времени
// после старта на эфир не влияет.
package clock

import "time"

type Clock struct {
	start     time.Time // несёт монотонное показание
	startUnix int64
}

func New() *Clock {
	now := time.Now()
	return &Clock{start: now, startUnix: now.UnixMilli()}
}

// NowMs — текущее серверное время в Unix-миллисекундах.
func (c *Clock) NowMs() int64 {
	return c.startUnix + time.Since(c.start).Milliseconds()
}
