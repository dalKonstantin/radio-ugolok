// Точка входа: связывает соединение, часы, проигрыватель и рендер.

import { Socket } from './ws.js';
import { ClockSync, RESYNC_EVERY_MS } from './sync.js';
import { Player } from './player.js';
import { uploadAll } from './upload.js';
import { Waveform } from './waveform.js';
import * as ui from './ui.js';

const NICK_KEY = 'radio.nick';
const VOLUME_KEY = 'radio.volume';
const RESYNC_MIN_GAP_MS = 2000;
const DIAG_EVERY_MS = 250;
const SEARCH_DEBOUNCE_MS = 200;
const LIBRARY_PAGE = 100;
const CHAT_MAX = 500;

const load = (key) => {
  try {
    return localStorage.getItem(key);
  } catch {
    return null;
  }
};
const save = (key, value) => {
  try {
    localStorage.setItem(key, value);
  } catch {}
};

const nickInput = document.getElementById('nick');
nickInput.value = load(NICK_KEY) || `гость-${Math.floor(Math.random() * 9000 + 1000)}`;
save(NICK_KEY, nickInput.value);

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
const waveform = new Waveform(document.getElementById('wave'));

// Состояние эфира, как его видит клиент.
let nowPlaying = null;
let skip = { votes: 0, required: 1 };
let voted = false; // свой голос сервер не возвращает — помним сами

const playingId = () => nowPlaying?.track?.id ?? null;
const renderSkip = () => ui.renderSkip(skip, voted, !!nowPlaying?.track);

function applyNowPlaying(np) {
  if (np.itemId !== nowPlaying?.itemId) voted = false;
  if (np.track?.id !== nowPlaying?.track?.id) waveform.load(np.track?.id ?? null);
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

// --- загрузка: кнопка в шапке и перетаскивание на всю страницу. Прогресс —
// полоса под шапкой, итог — тостами.
let uploading = Promise.resolve();
function upload(files) {
  if (files.length === 0) return;
  // Новая пачка ждёт, пока закончится предыдущая.
  uploading = uploading.then(() => uploadAll(files, nickInput.value.trim(), (ev) => {
    const { file, index, total, progress, result } = ev;
    if (!result) {
      const n = total > 1 ? ` (${index + 1}/${total})` : '';
      if (progress < 1) {
        ui.renderUploadProgress((index + progress) / total, `Загрузка${n}: ${file.name}`);
      } else {
        ui.renderUploadProgress('busy', `Обработка${n}: ${file.name}`);
      }
      return;
    }
    ui.renderUploadProgress(null);
    if (result.ok) {
      const name = `${result.track.artist} — ${result.track.title}`;
      ui.toast(result.created ? `Загружено: ${name}` : `Уже в библиотеке: ${name}`);
    } else {
      ui.toast(`${file.name}: ${result.message}`);
    }
  }));
}

const fileInput = document.getElementById('file');
fileInput.addEventListener('change', () => {
  const files = [...fileInput.files];
  fileInput.value = '';
  upload(files);
});

const hasFiles = (e) => [...(e.dataTransfer?.types ?? [])].includes('Files');
let dragDepth = 0; // dragenter/dragleave приходят от каждого вложенного элемента
window.addEventListener('dragenter', (e) => {
  if (!hasFiles(e)) return;
  e.preventDefault();
  dragDepth++;
  ui.setDropActive(true);
});
window.addEventListener('dragover', (e) => {
  if (!hasFiles(e)) return;
  e.preventDefault();
  e.dataTransfer.dropEffect = 'copy';
});
window.addEventListener('dragleave', (e) => {
  if (!hasFiles(e)) return;
  dragDepth = Math.max(0, dragDepth - 1);
  if (dragDepth === 0) ui.setDropActive(false);
});
window.addEventListener('drop', (e) => {
  if (!hasFiles(e)) return;
  e.preventDefault();
  dragDepth = 0;
  ui.setDropActive(false);
  upload([...e.dataTransfer.files]);
});

// --- чат: от 1280 px колонка справа, уже — выезжающая панель.
const wideChat = window.matchMedia('(min-width: 1280px)');
const chatOpen = () => wideChat.matches || document.body.classList.contains('chat-open');
ui.setChatVisibility(chatOpen);
function toggleChat(open) {
  document.body.classList.toggle('chat-open', open);
  if (open) {
    ui.clearUnread();
    ui.scrollChat(true);
    chatInput.focus();
  }
}
document.getElementById('chat-toggle').addEventListener('click', () =>
  toggleChat(!document.body.classList.contains('chat-open')));
document.getElementById('chat-close').addEventListener('click', () => toggleChat(false));
wideChat.addEventListener('change', () => {
  if (wideChat.matches) ui.clearUnread();
});

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

function connect() {
  player.unlock();
  player.connect();
  ui.renderConnectButton(true);
}

document.getElementById('connect').addEventListener('click', () => {
  if (player.connected) {
    player.disconnect();
    ui.renderConnectButton(false);
  } else {
    connect();
  }
});

// Гейт: первый клик даёт браузеру разрешение на звук и сразу выводит в эфир.
document.getElementById('gate-play').addEventListener('click', () => {
  ui.hideGate();
  connect();
});

// Ник: смена уходит на сервер повторным join.
nickInput.addEventListener('change', () => {
  const nick = nickInput.value.trim();
  if (!nick) return;
  save(NICK_KEY, nick);
  socket.send('join', { nick });
});

// Громкость: локальная, запоминается в браузере.
const volumeInput = document.getElementById('volume');
const setVolume = (v) => {
  audio.volume = v;
  ui.renderVolume(v);
};
const savedVolume = Number.parseFloat(load(VOLUME_KEY));
setVolume(Number.isFinite(savedVolume) ? Math.min(1, Math.max(0, savedVolume)) : 1);
volumeInput.addEventListener('input', () => {
  setVolume(Number(volumeInput.value));
  save(VOLUME_KEY, volumeInput.value);
});

setInterval(() => {
  if (socket.status === 'open' && clock.ready) clock.run();
}, RESYNC_EVERY_MS);

setInterval(() => {
  const pos = player.targetPos();
  const durMs = nowPlaying?.track?.durationMs;
  ui.renderProgress(pos, durMs);
  // Граница волны идёт по эфирному времени, а не по audio.currentTime:
  // так она не дёргается при коррекции темпа.
  waveform.setProgress(durMs && pos != null ? (pos * 1000) / durMs : 0);
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
window.radio = { socket, clock, player, audio, waveform };
