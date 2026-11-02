// Рендер. Весь пользовательский текст — только через textContent.

const $ = (id) => document.getElementById(id);

const fmtTime = (sec) => {
  if (sec == null || !Number.isFinite(sec)) return '–:––';
  const s = Math.max(0, Math.floor(sec));
  return `${Math.floor(s / 60)}:${String(s % 60).padStart(2, '0')}`;
};

const fmtMs = (v, digits = 0) => (v == null ? '—' : `${v.toFixed(digits)} мс`);

export function renderNowPlaying(np) {
  const t = np?.track;
  $('title').textContent = t ? t.title : 'Эфир пуст — поставьте трек в очередь';
  $('artist').textContent = t ? t.artist : '';
}

export function renderListeners({ count }) {
  $('listeners').textContent = `${count} ${plural(count, 'слушатель', 'слушателя', 'слушателей')}`;
}

export function renderConnectButton(connected) {
  $('connect').textContent = connected ? 'Отключиться' : 'Подключиться к эфиру';
  $('connect').classList.toggle('on', connected);
}

export function renderProgress(pos, durationMs) {
  const dur = durationMs ? durationMs / 1000 : null;
  $('time').textContent = `${fmtTime(pos == null ? null : Math.min(pos, dur ?? pos))} / ${fmtTime(dur)}`;
}

export function renderDiag({ socket, offset, rtt, drift, rate }) {
  $('d-socket').textContent = socket;
  $('d-offset').textContent = fmtMs(offset);
  $('d-rtt').textContent = fmtMs(rtt);
  $('d-drift').textContent = drift == null ? '—' : `${(drift * 1000).toFixed(0)} мс`;
  $('d-rate').textContent = rate.toFixed(3);
}

function plural(n, one, few, many) {
  const m10 = n % 10, m100 = n % 100;
  if (m10 === 1 && m100 !== 11) return one;
  if (m10 >= 2 && m10 <= 4 && (m100 < 12 || m100 > 14)) return few;
  return many;
}
