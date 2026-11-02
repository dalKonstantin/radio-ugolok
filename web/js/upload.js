// Загрузка файлов на сервер.

// Загружает файл, onProgress получает долю 0..1. Резолвится объектом
// {ok, created, track} или {ok: false, code, message}.
export function uploadFile(file, nick, onProgress = () => {}) {
  return new Promise((resolve) => {
    const form = new FormData();
    form.append('nick', nick);
    form.append('file', file);

    // fetch не умеет прогресс отправки, поэтому XHR.
    const xhr = new XMLHttpRequest();
    xhr.open('POST', '/api/upload');
    xhr.responseType = 'json';
    xhr.upload.onprogress = (e) => {
      if (e.lengthComputable) onProgress(e.loaded / e.total);
    };
    xhr.onload = () => {
      const body = xhr.response;
      if (xhr.status === 200 || xhr.status === 201) {
        resolve({ ok: true, created: xhr.status === 201, track: body });
      } else {
        resolve({
          ok: false,
          code: body?.error?.code ?? 'http_' + xhr.status,
          message: body?.error?.message ?? `Ошибка загрузки (${xhr.status})`,
        });
      }
    };
    xhr.onerror = () => resolve({ ok: false, code: 'network', message: 'Сеть недоступна' });
    xhr.send(form);
  });
}

// Загружает файлы по очереди; onEvent получает {file, index, total, progress}
// во время отправки и {file, result} после ответа сервера.
export async function uploadAll(files, nick, onEvent) {
  const list = [...files];
  for (let i = 0; i < list.length; i++) {
    const file = list[i];
    const result = await uploadFile(file, nick, (progress) =>
      onEvent({ file, index: i, total: list.length, progress }));
    onEvent({ file, result });
  }
}
