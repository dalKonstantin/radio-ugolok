// WebSocket-соединение с переподключением и keepalive.

const BACKOFF_MIN_MS = 1000;
const BACKOFF_MAX_MS = 15_000;
const KEEPALIVE_MS = 20_000;
const SILENCE_LIMIT_MS = 45_000; // без единого сообщения — соединение мёртвое

export class Socket {
  constructor(url) {
    this.url = url;
    this.handlers = new Map();
    this.backoff = BACKOFF_MIN_MS;
    this.ws = null;
    this.lastMessageAt = 0;
    this.status = 'connecting';

    setInterval(() => this.keepalive(), KEEPALIVE_MS);
  }

  // Подписка на сообщение сервера по type. Служебные: 'open', 'close'.
  on(type, fn) {
    if (!this.handlers.has(type)) this.handlers.set(type, []);
    this.handlers.get(type).push(fn);
  }

  emit(type, data) {
    for (const fn of this.handlers.get(type) ?? []) fn(data);
  }

  connect() {
    this.status = 'connecting';
    const ws = new WebSocket(this.url);
    this.ws = ws;

    ws.onopen = () => {
      this.backoff = BACKOFF_MIN_MS;
      this.lastMessageAt = Date.now();
      this.status = 'open';
      this.emit('open');
    };
    ws.onmessage = (ev) => {
      this.lastMessageAt = Date.now();
      let msg;
      try {
        msg = JSON.parse(ev.data);
      } catch {
        return;
      }
      this.emit(msg.type, msg.data);
    };
    ws.onclose = () => {
      if (this.ws !== ws) return;
      this.ws = null;
      this.status = 'closed';
      this.emit('close');
      const delay = this.backoff;
      this.backoff = Math.min(this.backoff * 2, BACKOFF_MAX_MS);
      setTimeout(() => this.connect(), delay);
    };
  }

  send(type, data = {}) {
    if (this.ws?.readyState !== WebSocket.OPEN) return false;
    this.ws.send(JSON.stringify({ type, data }));
    return true;
  }

  keepalive() {
    if (this.ws?.readyState !== WebSocket.OPEN) return;
    if (Date.now() - this.lastMessageAt > SILENCE_LIMIT_MS) {
      this.ws.close();
      return;
    }
    this.send('ping', { t0: Date.now() });
  }
}
