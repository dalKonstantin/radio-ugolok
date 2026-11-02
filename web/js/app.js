// Точка входа: связывает соединение, часы, проигрыватель и рендер.

import { Socket } from './ws.js';
import { ClockSync, RESYNC_EVERY_MS } from './sync.js';
import { Player } from './player.js';
import * as ui from './ui.js';

const NICK_KEY = 'radio.nick';
const RESYNC_MIN_GAP_MS = 2000;
const DIAG_EVERY_MS = 250;

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

// Для отладки из консоли: radio.audio.currentTime += 3
window.radio = { socket, clock, player, audio };
