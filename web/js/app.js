// Точка входа: связывает соединение, часы, проигрыватель и рендер.

import { Socket } from './ws.js';
import { ClockSync, RESYNC_EVERY_MS } from './sync.js';
import { Player } from './player.js';
import { uploadAll } from './upload.js';
import * as ui from './ui.js';

const NICK_KEY = 'radio.nick';
const RESYNC_MIN_GAP_MS = 2000;
const DIAG_EVERY_MS = 250;
const SEARCH_DEBOUNCE_MS = 200;
const LIBRARY_PAGE = 100;
const CHAT_MAX = 500;

const loadNick = () => {
  try {
    return localStorage.getItem(NICK_KEY) || '';
  } catch {
    return '';
  }
};
const saveNick = (nick) => {
  try {
    localStorage.setItem(NICK_KEY, nick);
  } catch {}
};

const nickInput = document.getElementById('nick');
nickInput.value = loadNick() || `гость-${Math.floor(Math.random() * 9000 + 1000)}`;
saveNick(nickInput.value);
nickInput.addEventListener('change', () => saveNick(nickInput.value.trim()));

const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
const socket = new Socket(`${proto}//${location.host}/ws`);
const clock = new ClockSync((t0) => socket.send('ping', { t0 }));

let lastResync = 0;
const requestResync = () => {
  const now = Date.now();
  if (now - lastResync < RESYNC_MIN_GAP_MS) return;
  lastResync = now;
  socket.send('resync');
};

const audio = document.getElementById('audio');
const player = new Player(audio, clock, { onStale: requestResync });

// Состояние эфира, как его видит клиент.
let nowPlaying = null;
let skip = { votes: 0, required: 1 };
let voted = false; // свой голос сервер не возвращает — помним сами

const playingId = () => nowPlaying?.track?.id ?? null;
const renderSkip = () => ui.renderSkip(skip, voted, !!nowPlaying?.track);

function applyNowPlaying(np) {
  if (np.itemId !== nowPlaying?.itemId) voted = false;
  nowPlaying = np;
  ui.renderNowPlaying(np);
  renderSkip();
  player.setNowPlaying(np);
  // played_at у треков поменялся, а у играющего крестик неактивен.
  loadLibrary();
}

const removeFromQueue = (itemId) => socket.send('dequeue', { itemId });

socket.on('hello', async () => {
  // Первая оценка сдвига готова после трёх обменов, остальные её уточнят.
  await clock.run();
  socket.send('join', { nick: nickInput.value.trim() });
});
socket.on('close', () => clock.cancel());
socket.on('pong', (d) => clock.onPong(d));
socket.on('state', (s) => {
  applyNowPlaying(s.nowPlaying);
  ui.renderQueue(s.queue, removeFromQueue);
  ui.renderListeners(s.listeners);
  skip = s.skip;
  voted = false; // после переподключения мы для сервера новый клиент
  renderSkip();
  ui.resetChat(s.chat);
});
socket.on('nowPlaying', applyNowPlaying);
socket.on('queue', (q) => ui.renderQueue(q.items, removeFromQueue));
socket.on('listeners', (l) => ui.renderListeners(l));
socket.on('skip', (s) => {
  skip = s;
  if (s.votes === 0) voted = false;
  renderSkip();
});
socket.on('chat', (m) => ui.appendChat(m));
socket.on('trackAdded', () => loadLibrary());
socket.on('trackRemoved', () => loadLibrary());
socket.on('error', (e) => ui.toast(e.message));

// --- библиотека: поиск уходит на сервер с задержкой, устаревшие ответы
// отбрасываются.
const searchInput = document.getElementById('search');
const libraryActions = {
  enqueue: (trackId) => socket.send('enqueue', { trackId }),
  remove: (t) => {
    if (confirm(`Удалить «${t.title}» из библиотеки? Файл будет стёрт.`)) {
      socket.send('deleteTrack', { trackId: t.id });
    }
  },
};
let libraryReq = 0;
async function loadLibrary() {
  const req = ++libraryReq;
  const params = new URLSearchParams({ q: searchInput.value.trim(), limit: LIBRARY_PAGE });
  try {
    const res = await fetch(`/api/library?${params}`);
    const body = await res.json();
    if (req !== libraryReq) return;
    if (!res.ok) throw new Error(body?.error?.message ?? res.status);
    ui.renderLibrary(body.items, body.total, playingId(), libraryActions);
  } catch (err) {
    console.warn('библиотека:', err);
  }
}
let searchTimer;
searchInput.addEventListener('input', () => {
  clearTimeout(searchTimer);
  searchTimer = setTimeout(loadLibrary, SEARCH_DEBOUNCE_MS);
});

// --- загрузка
const fileInput = document.getElementById('file');
fileInput.addEventListener('change', async () => {
  const files = [...fileInput.files];
  fileInput.value = '';
  if (files.length === 0) return;
  await uploadAll(files, nickInput.value.trim(), ({ file, index, total, progress, result }) => {
    if (!result) {
      const n = total > 1 ? ` (${index + 1}/${total})` : '';
      ui.renderUploadStatus(progress < 1
        ? `Загрузка${n}: ${file.name} — ${Math.round(progress * 100)}%`
        : `Обработка${n}: ${file.name}…`);
    } else if (result.ok) {
      ui.renderUploadStatus(result.created
        ? `Загружено: ${result.track.artist} — ${result.track.title}`
        : `Уже в библиотеке: ${result.track.artist} — ${result.track.title}`);
    } else {
      ui.renderUploadStatus('');
      ui.toast(`${file.name}: ${result.message}`);
    }
  });
});

// --- чат
const chatInput = document.getElementById('chat-input');
chatInput.addEventListener('keydown', (e) => {
  if (e.key !== 'Enter' || e.isComposing) return;
  e.preventDefault();
  const text = chatInput.value.trim().slice(0, CHAT_MAX);
  if (!text) return;
  if (socket.send('chat', { text })) chatInput.value = '';
  else ui.toast('Нет соединения с сервером');
});
const chatList = document.getElementById('chat');
chatList.addEventListener('scroll', () => {
  if (chatList.scrollHeight - chatList.scrollTop - chatList.clientHeight < 4) ui.scrollChat(false);
});
document.getElementById('chat-new').addEventListener('click', () => ui.scrollChat(true));

// --- управление
document.getElementById('skip').addEventListener('click', () => {
  if (socket.send('voteSkip')) {
    voted = !voted;
    renderSkip();
  }
});

document.getElementById('connect').addEventListener('click', () => {
  if (player.connected) {
    player.disconnect();
  } else {
    player.unlock();
    player.connect();
  }
  ui.renderConnectButton(player.connected);
});

setInterval(() => {
  if (socket.status === 'open' && clock.ready) clock.run();
}, RESYNC_EVERY_MS);

setInterval(() => {
  ui.renderProgress(player.targetPos(), nowPlaying?.track?.durationMs);
  ui.renderDiag({
    socket: socket.status,
    offset: clock.ready ? clock.offset : null,
    rtt: clock.rtt,
    drift: player.holding ? player.drift : null,
    rate: audio.playbackRate,
  });
}, DIAG_EVERY_MS);

ui.renderConnectButton(false);
socket.connect();
loadLibrary();

// Для отладки из консоли: radio.audio.currentTime += 3
window.radio = { socket, clock, player, audio };
