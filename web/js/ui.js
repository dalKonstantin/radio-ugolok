// Рендер. Весь пользовательский текст — только через textContent.

const $ = (id) => document.getElementById(id);

const TOAST_MS = 4000;
const CHAT_STICK_PX = 40; // насколько близко к низу считается «внизу»

const fmtTime = (sec) => {
  if (sec == null || !Number.isFinite(sec)) return '–:––';
  const s = Math.max(0, Math.floor(sec));
  return `${Math.floor(s / 60)}:${String(s % 60).padStart(2, '0')}`;
};

const fmtMs = (v, digits = 0) => (v == null ? '—' : `${v.toFixed(digits)} мс`);

const fmtClock = (ms) =>
  new Date(ms).toLocaleTimeString('ru-RU', { hour: '2-digit', minute: '2-digit' });

function el(tag, className, text) {
  const node = document.createElement(tag);
  if (className) node.className = className;
  if (text != null) node.textContent = text;
  return node;
}

function button(className, text, title, onClick) {
  const b = el('button', className, text);
  b.type = 'button';
  if (title) b.title = title;
  b.addEventListener('click', onClick);
  return b;
}

function plural(n, one, few, many) {
  const m10 = n % 10, m100 = n % 100;
  if (m10 === 1 && m100 !== 11) return one;
  if (m10 >= 2 && m10 <= 4 && (m100 < 12 || m100 > 14)) return few;
  return many;
}

// --- эфир ------------------------------------------------------------------

export function renderNowPlaying(np) {
  const t = np?.track;
  document.body.classList.toggle('idle', !t);
  $('title').textContent = t ? t.title : 'Эфир пуст — поставьте трек в очередь';
  $('artist').textContent = t ? t.artist : '';
  $('added-by').textContent = t && np.addedBy ? `от ${np.addedBy}` : '';
}

export function renderListeners({ count, nicks }) {
  $('listeners').textContent = `${count} ${plural(count, 'слушатель', 'слушателя', 'слушателей')}`;
  $('listeners').title = nicks.join(', ');
}

export function renderConnectButton(connected) {
  $('connect').textContent = connected ? 'Отключиться' : 'Подключиться к эфиру';
  $('connect').classList.toggle('on', connected);
}

export function renderProgress(pos, durationMs) {
  const dur = durationMs ? durationMs / 1000 : null;
  $('time').textContent = dur == null
    ? ''
    : `${fmtTime(pos == null ? null : Math.min(pos, dur))} / ${fmtTime(dur)}`;
}

export function renderSkip({ votes, required }, voted, playing) {
  const b = $('skip');
  b.disabled = !playing;
  b.classList.toggle('on', voted);
  b.textContent = votes > 0 ? `Скип ${votes}/${required}` : 'Скип';
  b.title = voted ? 'Снять голос' : `Голос за пропуск трека, нужно ${required}`;
}

export function renderDiag({ socket, offset, rtt, drift, rate }) {
  $('d-socket').textContent = socket;
  $('d-offset').textContent = fmtMs(offset);
  $('d-rtt').textContent = fmtMs(rtt);
  $('d-drift').textContent = drift == null ? '—' : `${(drift * 1000).toFixed(0)} мс`;
  $('d-rate').textContent = rate.toFixed(3);
}

let toastTimer;
export function toast(text) {
  const t = $('toast');
  t.textContent = text;
  t.hidden = false;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => { t.hidden = true; }, TOAST_MS);
}

export function renderUploadStatus(text) {
  $('upload-status').textContent = text;
}

// --- очередь и библиотека --------------------------------------------------

function trackText(t) {
  const span = el('span', 'track');
  span.append(el('b', null, t.title), ' ', el('span', 'muted', t.artist));
  return span;
}

// onRemove(itemId)
export function renderQueue(items, onRemove) {
  const list = $('queue');
  list.replaceChildren(...items.map((it, i) => {
    const li = el('li');
    li.append(
      el('span', 'muted num', String(i + 1)),
      trackText(it.track),
      el('span', 'muted', it.addedBy),
      button('icon', '×', 'Убрать из очереди', () => onRemove(it.itemId)),
    );
    return li;
  }));
  $('queue-empty').hidden = items.length > 0;
}

// actions: {enqueue(trackId), remove(track)}; playingId — что сейчас в эфире.
export function renderLibrary(items, total, playingId, actions) {
  $('library').replaceChildren(...items.map((t) => libraryRow(t, playingId, actions)));
  $('library-total').textContent = items.length < total
    ? `показано ${items.length} из ${total}`
    : `${total} ${plural(total, 'трек', 'трека', 'треков')}`;
}

function libraryRow(t, playingId, actions) {
  const li = el('li');

  let cover;
  if (t.hasCover) {
    cover = el('img', 'cover');
    cover.src = `/api/tracks/${t.id}/cover`;
    cover.alt = '';
    cover.loading = 'lazy';
  } else {
    cover = el('span', 'cover');
  }

  li.append(
    cover,
    trackText(t),
    el('span', 'muted', fmtTime(t.durationMs / 1000)),
    button('icon', '+', 'Поставить в очередь', () => actions.enqueue(t.id)),
  );

  // Крестик — только у трека, который уже звучал; у играющего он неактивен.
  if (t.playedAt != null) {
    const del = button('icon', '×', 'Удалить из библиотеки', () => actions.remove(t));
    if (t.id === playingId) {
      del.disabled = true;
      del.title = 'Трек сейчас в эфире';
    }
    li.append(del);
  } else {
    li.append(el('span', 'icon'));
  }
  return li;
}

// --- чат -------------------------------------------------------------------

let lastChatNick = null;

export function resetChat(messages) {
  $('chat').replaceChildren();
  lastChatNick = null;
  for (const m of messages) appendChat(m);
  scrollChat(true);
}

export function appendChat(m) {
  const list = $('chat');
  const stick = list.scrollHeight - list.scrollTop - list.clientHeight < CHAT_STICK_PX;

  const li = el('li', m.kind === 'system' ? 'system' : null);
  // Подряд идущие сообщения одного человека — ник один раз.
  const grouped = m.kind === 'user' && m.nick === lastChatNick;
  if (m.kind === 'user' && !grouped) li.append(el('span', 'nick', m.nick));
  li.append(el('span', 'text', m.text), el('time', null, fmtClock(m.at)));
  list.append(li);
  lastChatNick = m.kind === 'user' ? m.nick : null;

  if (stick) scrollChat(true);
  else $('chat-new').hidden = false;
}

export function scrollChat(force) {
  const list = $('chat');
  if (force) list.scrollTop = list.scrollHeight;
  $('chat-new').hidden = true;
}
