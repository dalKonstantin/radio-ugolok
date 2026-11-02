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

socket.on('hello', async () => {
  // Первая оценка сдвига готова после трёх обменов, остальные её уточнят.
  await clock.run();
  socket.send('join', { nick: nickInput.value.trim() });
});
socket.on('close', () => clock.cancel());
socket.on('pong', (d) => clock.onPong(d));
socket.on('state', (s) => {
  ui.renderNowPlaying(s.nowPlaying);
  ui.renderListeners(s.listeners);
  player.setNowPlaying(s.nowPlaying);
});
socket.on('nowPlaying', (np) => {
  ui.renderNowPlaying(np);
  player.setNowPlaying(np);
});
socket.on('listeners', (l) => ui.renderListeners(l));
socket.on('error', (e) => console.warn('сервер:', e.code, e.message));
socket.on('trackAdded', () => loadLibrary());

// Библиотека: поиск уходит на сервер с задержкой, устаревшие ответы
// отбрасываются.
const searchInput = document.getElementById('search');
let libraryReq = 0;
async function loadLibrary() {
  const req = ++libraryReq;
  const params = new URLSearchParams({ q: searchInput.value.trim(), limit: LIBRARY_PAGE });
  try {
    const res = await fetch(`/api/library?${params}`);
    const body = await res.json();
    if (req !== libraryReq) return;
    if (!res.ok) throw new Error(body?.error?.message ?? res.status);
    ui.renderLibrary(body.items, body.total);
  } catch (err) {
    console.warn('библиотека:', err);
  }
}
let searchTimer;
searchInput.addEventListener('input', () => {
  clearTimeout(searchTimer);
  searchTimer = setTimeout(loadLibrary, SEARCH_DEBOUNCE_MS);
});

// Загрузка.
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
      ui.renderUploadStatus(`${file.name}: ${result.message}`);
    }
  });
  loadLibrary();
});

setInterval(() => {
  if (socket.status === 'open' && clock.ready) clock.run();
}, RESYNC_EVERY_MS);

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
  ui.renderProgress(player.targetPos(), player.np?.track?.durationMs);
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
