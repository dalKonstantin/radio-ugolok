// <audio>-элемент: выход в позицию эфира и удержание в ней.

import { driftAction, SEEK_AHEAD } from './sync.js';

const LOOKAHEAD = 0.3;          // с, запас на буферизацию и декод
const BACKUP_CHECK_MS = 2000;   // таймеры в фоне душатся, timeupdate — нет
const STALE_GRACE = 1.5;        // с за концом трека, после которых состояние устарело
const MAX_ENTER_ATTEMPTS = 3;

const once = (el, event) =>
  new Promise((resolve) => el.addEventListener(event, resolve, { once: true }));
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

const trackUrl = (track) => `/api/tracks/${track.id}/audio`;

export class Player {
  // clock — объект с методом now() (серверное время, мс).
  // onStale — вызывается, когда по часам трек давно должен был закончиться.
  constructor(audio, clock, { onStale } = {}) {
    this.audio = audio;
    this.clock = clock;
    this.onStale = onStale ?? (() => {});
    this.np = null;            // последний nowPlaying
    this.connected = false;    // пользователь хочет слышать эфир
    this.holding = false;      // проверки дрейфа активны
    this.gen = 0;              // поколение выхода в позицию, для отмены
    this.drift = null;         // с, последний измеренный дрейф

    audio.preload = 'auto';
    audio.preservesPitch = true;
    audio.webkitPreservesPitch = true;

    audio.addEventListener('timeupdate', () => this.check());
    setInterval(() => this.check(), BACKUP_CHECK_MS);
  }

  // Позиция эфира по серверным часам, с.
  targetPos() {
    if (!this.np?.track) return null;
    return (this.clock.now() - this.np.startedAt) / 1000;
  }

  setNowPlaying(np) {
    const same = this.np?.track && np?.track &&
      this.np.track.id === np.track.id && this.np.startedAt === np.startedAt;
    this.np = np;
    // state после переподключения повторяет тот же эфир: если уже держим
    // позицию, новый выход в неё дал бы слышимый скачок — дрейф поправит сам.
    if (same && this.holding) return;
    if (this.connected) this.enter();
  }

  // Вызывать синхронно из обработчика клика: Safari разрешает play() только
  // элементу, который уже запускали внутри жеста пользователя.
  unlock() {
    if (this.np?.track && !this.audio.src) this.audio.src = trackUrl(this.np.track);
    this.audio.play().catch(() => {});
    this.audio.pause();
  }

  connect() {
    this.connected = true;
    this.enter();
  }

  disconnect() {
    this.connected = false;
    this.gen++;
    this.holding = false;
    this.audio.pause();
  }

  // Выход в текущую точку эфира.
  async enter() {
    const gen = ++this.gen;
    const audio = this.audio;
    this.holding = false;
    this.drift = null;

    const track = this.np?.track;
    if (!track) {
      audio.pause();
      return;
    }

    const url = trackUrl(track);
    if (!audio.src.endsWith(url)) {
      audio.src = url;
    }
    if (audio.readyState < HTMLMediaElement.HAVE_METADATA) {
      await once(audio, 'loadedmetadata');
      if (gen !== this.gen) return;
    }
    audio.playbackRate = 1;

    for (let attempt = 0; attempt < MAX_ENTER_ATTEMPTS; attempt++) {
      const pos = this.targetPos();
      if (pos > track.durationMs / 1000 + STALE_GRACE) {
        audio.pause();
        this.onStale();
        return;
      }
      if (pos > track.durationMs / 1000) {
        // Трек доигран, следующий nowPlaying уже в пути.
        audio.pause();
        return;
      }

      const seekTo = Math.max(0, pos + LOOKAHEAD);
      if (Math.abs(audio.currentTime - seekTo) > 0.01) {
        audio.currentTime = seekTo;
        await once(audio, 'seeked');
        if (gen !== this.gen) return;
      }
      if (audio.readyState < HTMLMediaElement.HAVE_FUTURE_DATA) {
        await once(audio, 'canplay');
        if (gen !== this.gen) return;
      }

      // Позиция выставлена с запасом: ждём, пока эфир её догонит.
      const ahead = audio.currentTime - this.targetPos();
      if (ahead < -LOOKAHEAD) continue; // буферизация съела весь запас — ещё раз
      if (ahead > 0) {
        await sleep(ahead * 1000);
        if (gen !== this.gen) return;
      }
      break;
    }

    try {
      await audio.play();
    } catch (err) {
      console.warn('play() отклонён', err);
      return;
    }
    if (gen !== this.gen) return;
    this.holding = true;
    this.check();
  }

  // Проверка дрейфа и коррекция темпом или перемоткой.
  check() {
    const audio = this.audio;
    if (!this.connected || !this.holding || audio.paused || audio.seeking) return;
    const pos = this.targetPos();
    if (pos === null) return;

    const drift = pos - audio.currentTime;
    this.drift = drift;
    const action = driftAction(drift);

    if (action.seek) {
      // Пока перемотка не закончилась, проверки стоят — иначе алгоритм
      // начнёт гоняться за собственным отставанием.
      const gen = this.gen;
      this.holding = false;
      audio.playbackRate = 1;
      audio.currentTime = pos + SEEK_AHEAD;
      once(audio, 'seeked').then(() => {
        if (gen === this.gen) this.holding = true;
      });
      return;
    }
    if (audio.playbackRate !== action.rate) audio.playbackRate = action.rate;
  }
}
