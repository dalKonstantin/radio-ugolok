// Часы эфира и удержание позиции.

export const SYNC_PINGS = 10;
export const SYNC_INTERVAL_MS = 200;
export const RESYNC_EVERY_MS = 60_000;
const BEST_SAMPLES = 3;
const PONG_WAIT_MS = 1000;

const DEADBAND = 0.05;   // с, меньше — ничего не делаем
const HARD_SEEK = 1;     // с, больше — жёсткая перемотка
const RATE_GAIN = 0.1;
const MAX_RATE_DELTA = 0.02;
export const SEEK_AHEAD = 0.1;

const median = (xs) => {
  const s = [...xs].sort((a, b) => a - b);
  const m = s.length >> 1;
  return s.length % 2 ? s[m] : (s[m - 1] + s[m]) / 2;
};

// Из образцов {offset, rtt} берёт три с наименьшим rtt и возвращает медиану
// их offset и rtt. null, если образцов нет.
export function estimateOffset(samples) {
  if (samples.length === 0) return null;
  const best = [...samples].sort((a, b) => a.rtt - b.rtt).slice(0, BEST_SAMPLES);
  return {
    offset: median(best.map((s) => s.offset)),
    rtt: median(best.map((s) => s.rtt)),
  };
}

// Решение по дрейфу (с; положительный — клиент отстаёт).
// Возвращает {rate} или {seek: true}.
export function driftAction(drift) {
  const abs = Math.abs(drift);
  if (abs < DEADBAND) return { rate: 1 };
  if (abs <= HARD_SEEK) {
    const delta = Math.min(MAX_RATE_DELTA, Math.max(-MAX_RATE_DELTA, drift * RATE_GAIN));
    // Округление, чтобы не дёргать playbackRate на каждом timeupdate.
    return { rate: Math.round((1 + delta) * 1000) / 1000 };
  }
  return { seek: true };
}

// Оценка сдвига часов клиента относительно сервера по обменам ping/pong.
export class ClockSync {
  constructor(sendPing) {
    this.sendPing = sendPing;
    this.offset = 0;
    this.rtt = null;
    this.ready = false;
    this.round = 0;
    this.pending = new Set();
    this.samples = [];
  }

  // Серверное время сейчас, Unix мс.
  now() {
    return Date.now() + this.offset;
  }

  // Запускает раунд из SYNC_PINGS обменов. Промис resolve-ится, как только
  // набралось BEST_SAMPLES образцов (первая оценка), уточнение идёт дальше.
  // Отмена — через cancel(): новый раунд или обрыв соединения.
  run() {
    const round = ++this.round;
    this.pending.clear();
    this.samples = [];
    return new Promise((resolve) => {
      this.onEnough = resolve;
      let sent = 0;
      const tick = () => {
        if (round !== this.round) return;
        if (sent < SYNC_PINGS) {
          const t0 = Date.now();
          this.pending.add(t0);
          this.sendPing(t0);
          sent++;
          setTimeout(tick, SYNC_INTERVAL_MS);
        } else {
          // Последние pong могут ещё лететь.
          setTimeout(() => {
            if (round !== this.round) return;
            this.apply();
            this.resolveEnough();
          }, PONG_WAIT_MS);
        }
      };
      tick();
    });
  }

  cancel() {
    this.round++;
    this.pending.clear();
    this.resolveEnough();
  }

  onPong({ t0, tServer }) {
    if (!this.pending.delete(t0)) return; // keepalive или устаревший раунд
    const t1 = Date.now();
    this.samples.push({ offset: tServer - (t0 + t1) / 2, rtt: t1 - t0 });
    if (this.samples.length >= BEST_SAMPLES) {
      this.apply();
      this.resolveEnough();
    }
  }

  apply() {
    const est = estimateOffset(this.samples);
    if (!est) return;
    this.offset = est.offset;
    this.rtt = est.rtt;
    this.ready = true;
  }

  resolveEnough() {
    const r = this.onEnough;
    this.onEnough = null;
    if (r) r();
  }
}
