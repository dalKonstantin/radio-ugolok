// Waveform на canvas по пикам трека (1000 байт, 0..255).
//
// Столбики 3 px с зазором 1 px; снизу зеркальная копия в 35 % высоты.
// Проигранная часть белая, непроигранная — полупрозрачная. Перемотки нет:
// canvas не реагирует на клики.

const BAR = 3;
const GAP = 1;
const MIRROR = 0.35;        // высота отражения относительно верхней части
const MIRROR_ALPHA = 0.4;
const PLAYED = 'rgba(255, 255, 255, 1)';
const PENDING = 'rgba(255, 255, 255, 0.35)';

export class Waveform {
  constructor(canvas) {
    this.canvas = canvas;
    this.ctx = canvas.getContext('2d');
    this.peaks = null;      // Uint8Array или null
    this.bars = [];         // высоты столбиков 0..1 под текущую ширину
    this.progress = 0;      // 0..1
    this.played = -1;       // сколько столбиков нарисовано проигранными
    this.req = 0;           // номер запроса пиков, для отбрасывания устаревших

    new ResizeObserver(() => this.resize()).observe(canvas);
  }

  // Загружает пики трека; null — очистить (эфир пуст).
  async load(trackId) {
    const req = ++this.req;
    if (!trackId) {
      this.setPeaks(null);
      return;
    }
    try {
      const res = await fetch(`/api/tracks/${trackId}/peaks`);
      if (!res.ok) throw new Error(res.status);
      const buf = new Uint8Array(await res.arrayBuffer());
      if (req === this.req) this.setPeaks(buf);
    } catch (err) {
      console.warn('пики:', err);
      if (req === this.req) this.setPeaks(null);
    }
  }

  setPeaks(peaks) {
    this.peaks = peaks;
    this.resize();
  }

  // Двигает границу проигранного; перерисовывает, только если она
  // перешла на другой столбик.
  setProgress(frac) {
    this.progress = Number.isFinite(frac) ? Math.min(1, Math.max(0, frac)) : 0;
    const played = Math.floor(this.progress * this.bars.length);
    if (played !== this.played) this.draw();
  }

  resize() {
    const { canvas } = this;
    const dpr = window.devicePixelRatio || 1;
    const w = canvas.clientWidth;
    const h = canvas.clientHeight;
    canvas.width = Math.round(w * dpr);
    canvas.height = Math.round(h * dpr);
    this.ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    this.bars = this.resample(Math.floor((w + GAP) / (BAR + GAP)));
    this.draw();
  }

  // Сводит пики к n столбикам, беря максимум по корзине.
  resample(n) {
    const p = this.peaks;
    if (!p || p.length === 0 || n <= 0) return [];
    const out = new Array(n);
    for (let i = 0; i < n; i++) {
      const from = Math.floor((i * p.length) / n);
      const to = Math.max(from + 1, Math.floor(((i + 1) * p.length) / n));
      let m = 0;
      for (let j = from; j < to; j++) if (p[j] > m) m = p[j];
      out[i] = m / 255;
    }
    return out;
  }

  draw() {
    const { ctx, canvas, bars } = this;
    const w = canvas.clientWidth;
    const h = canvas.clientHeight;
    ctx.clearRect(0, 0, w, h);

    const top = h / (1 + MIRROR);   // ось между столбиком и отражением
    const played = Math.floor(this.progress * bars.length);
    this.played = played;

    if (bars.length === 0) {
      // Эфир пуст или пики не пришли — тонкая линия вместо волны.
      ctx.fillStyle = PENDING;
      ctx.fillRect(0, top - 1, w, 1);
      return;
    }

    for (let i = 0; i < bars.length; i++) {
      const x = i * (BAR + GAP);
      const bh = Math.max(1, bars[i] * top);
      ctx.fillStyle = i < played ? PLAYED : PENDING;
      ctx.globalAlpha = 1;
      ctx.fillRect(x, top - bh, BAR, bh);
      ctx.globalAlpha = MIRROR_ALPHA;
      ctx.fillRect(x, top + 1, BAR, Math.max(1, bh * MIRROR - 1));
    }
    ctx.globalAlpha = 1;
  }
}
