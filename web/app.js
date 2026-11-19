const SUPPORTED = ['en', 'pt', 'es'];
const lang = pickLanguage(navigator.languages || [navigator.language]);
const state = { key: '', messages: {}, instance: null, timer: 0, streams: new Map() };
const $ = (sel) => document.querySelector(sel);

function pickLanguage(list) {
  for (const raw of list) {
    const base = String(raw || '').toLowerCase().split(/[-_]/)[0];
    if (SUPPORTED.includes(base)) return base;
  }
  return 'en';
}

function t(key, ...args) {
  const text = state.messages[key] ?? key;
  return text.replace(/\{(\d+)\}/g, (_, i) => String(args[Number(i)] ?? ''));
}

const plural = new Intl.PluralRules(lang === 'pt' ? 'pt-BR' : lang);

function tn(key, n) {
  const form = `${key}.${plural.select(n)}`;
  return t(state.messages[form] ? form : `${key}.other`, numberFmt.format(n));
}

const dateFmt = new Intl.DateTimeFormat(lang === 'pt' ? 'pt-BR' : lang, { dateStyle: 'short', timeStyle: 'medium' });
const numberFmt = new Intl.NumberFormat(lang === 'pt' ? 'pt-BR' : lang, { maximumFractionDigits: 1 });

function fmtDate(value) {
  if (!value || value.startsWith('0001')) return '—';
  return dateFmt.format(new Date(value));
}

function fmtDuration(ms) {
  return t('unit.seconds', numberFmt.format(ms / 1000));
}

async function loadMessages() {
  const res = await fetch(`locales/${lang}.json`);
  state.messages = await res.json();
  document.documentElement.lang = lang === 'pt' ? 'pt-BR' : lang;
  document.title = t('app.title');
  for (const el of document.querySelectorAll('[data-i18n]')) el.textContent = t(el.dataset.i18n);
}

class ApiError extends Error {
  constructor(status, code, message) {
    super(message);
    this.status = status;
    this.code = code;
  }
}

async function api(method, path, body) {
  const headers = { apikey: state.key };
  if (body !== undefined) headers['Content-Type'] = 'application/json';
  const res = await fetch(`/api${path}`, { method, headers, body: body === undefined ? undefined : JSON.stringify(body) });
  if (res.status === 204) return {};
  const data = await res.json().catch(() => ({}));
  if (!res.ok) throw new ApiError(res.status, data.error?.code || 'internal_error', data.error?.message || '');
  return data;
}

function errorText(err) {
  if (err instanceof ApiError) {
    const key = `error.${err.code}`;
    return state.messages[key] ? t(key) : t('error.generic', err.code);
  }
  return t('error.network');
}

function stateText(value) {
  const key = `state.${value}`;
  return state.messages[key] ? t(key) : t('state.unknown', value);
}

function el(tag, props = {}, ...children) {
  const node = document.createElement(tag);
  for (const [k, v] of Object.entries(props)) {
    if (k === 'class') node.className = v;
    else if (k === 'text') node.textContent = v;
    else if (k.startsWith('on')) node.addEventListener(k.slice(2), v);
    else if (v !== false && v !== undefined) node.setAttribute(k, v === true ? '' : v);
  }
  for (const c of children) if (c) node.append(c);
  return node;
}

async function busy(button, fn) {
  button.disabled = true;
  try {
    return await fn();
  } finally {
    button.disabled = false;
  }
}

function confirmDialog(title, body, okLabel) {
  const dialog = $('#confirm');
  $('#confirm-title').textContent = title;
  $('#confirm-body').textContent = body;
  $('#confirm-ok').textContent = okLabel;
  dialog.returnValue = '';
  dialog.showModal();
  return new Promise((resolve) => {
    dialog.addEventListener('close', () => {
      const ok = dialog.returnValue === 'ok';
      dialog.returnValue = '';
      resolve(ok);
    }, { once: true });
  });
}

function show(view) {
  for (const id of ['auth-view', 'list-view', 'detail-view']) $(`#${id}`).hidden = id !== view;
  $('#lock').hidden = view === 'auth-view';
}

function route() {
  clearInterval(state.timer);
  if (!state.key) return show('auth-view');
  const match = location.hash.match(/^#\/instances\/([A-Za-z0-9_]+)$/);
  if (match) return openDetail(match[1]);
  show('list-view');
  loadList();
  state.timer = setInterval(loadList, 10000);
}

async function unlock(event) {
  event.preventDefault();
  const input = $('#api-key');
  const error = $('#auth-error');
  error.hidden = true;
  state.key = input.value.trim();
  try {
    await api('GET', '/agents');
    input.value = '';
    route();
  } catch (err) {
    state.key = '';
    error.textContent = err instanceof ApiError && err.status === 401 ? t('auth.invalid') : errorText(err);
    error.hidden = false;
  }
}

function lock() {
  state.key = '';
  for (const c of state.streams.values()) c.abort();
  state.streams.clear();
  location.hash = '#/';
  route();
}

function actionButton(label, onclick, cls = '') {
  return el('button', { type: 'button', class: cls, text: label, onclick });
}

async function loadList() {
  const status = $('#list-status');
  try {
    const { instances } = await api('GET', '/instances');
    const body = $('#instances');
    body.replaceChildren();
    if (!instances.length) {
      body.append(el('tr', {}, el('td', { colspan: '6', class: 'muted', text: t('list.empty') })));
    }
    for (const inst of instances) {
      const name = inst.name || inst.id;
      const row = el('tr', {},
        el('th', { scope: 'row' }, el('a', { href: `#/instances/${inst.id}`, text: name })),
        el('td', { 'data-label': t('col.state') }, el('span', { class: `badge ${inst.state}`, text: stateText(inst.state) })),
        el('td', { 'data-label': t('col.created'), text: fmtDate(inst.created_at) }),
        el('td', { 'data-label': t('col.activity'), text: fmtDate(inst.last_activity_at) }),
        el('td', { 'data-label': t('col.running'), text: inst.running_agents.join(', ') || '—' }),
        el('td', { 'data-label': t('col.actions') }, el('div', { class: 'row wrap' },
          actionButton(t('action.start'), (e) => lifecycle(e.currentTarget, inst.id, 'start', loadList), 'primary'),
          actionButton(t('action.stop'), (e) => lifecycle(e.currentTarget, inst.id, 'stop', loadList)),
          el('a', { class: 'button', href: `#/instances/${inst.id}`, text: t('action.open') }),
          actionButton(t('action.delete'), (e) => removeInstance(e.currentTarget, inst), 'danger'),
        )),
      );
      body.append(row);
    }
    status.textContent = tn('list.count', instances.length);
  } catch (err) {
    status.textContent = errorText(err);
  }
}

async function lifecycle(button, id, action, after) {
  const status = location.hash.startsWith('#/instances/') ? $('#detail-status') : $('#list-status');
  status.textContent = t(action === 'start' ? 'status.starting' : 'status.stopping');
  await busy(button, async () => {
    try {
      await api('POST', `/instances/${id}/${action}`);
      status.textContent = t(action === 'start' ? 'status.started' : 'status.stopped');
    } catch (err) {
      status.textContent = errorText(err);
    }
  });
  after();
}

async function removeInstance(button, inst) {
  const ok = await confirmDialog(t('delete.title'), t('delete.body', inst.name || inst.id), t('action.delete'));
  if (!ok) return;
  await busy(button, async () => {
    try {
      await api('DELETE', `/instances/${inst.id}?confirm=${encodeURIComponent(inst.id)}`);
      $('#list-status').textContent = t('status.deleted');
    } catch (err) {
      $('#list-status').textContent = errorText(err);
    }
  });
  loadList();
}

async function createInstance(event) {
  event.preventDefault();
  const form = event.currentTarget;
  const button = form.querySelector('button[type=submit]');
  await busy(button, async () => {
    try {
      const data = await api('POST', '/instances', { name: form.name.value.trim() });
      $('#list-status').textContent = t('status.created');
      form.reset();
      location.hash = `#/instances/${data.instance.id}`;
    } catch (err) {
      $('#list-status').textContent = errorText(err);
    }
  });
}

async function openDetail(id) {
  show('detail-view');
  $('#runs').replaceChildren();
  await loadDetail(id);
  state.timer = setInterval(() => loadDetail(id, true), 15000);
}

async function loadDetail(id, quiet = false, force = false) {
  const status = $('#detail-status');
  try {
    const data = await api('GET', `/instances/${id}/status`);
    state.instance = data.instance;
    renderInfo(data.instance);
    if (!quiet || force || !connectionsInUse()) renderConnections(data);
    await loadHistory(id);
    if (!quiet) status.textContent = '';
  } catch (err) {
    status.textContent = errorText(err);
  }
}

function connectionsInUse() {
  const list = $('#connections');
  return Boolean(list.querySelector('.login')) || list.contains(document.activeElement) || [...list.querySelectorAll('input')].some((i) => i.value);
}

function renderInfo(inst) {
  $('#detail-title').textContent = inst.name || inst.id;
  const rows = [
    ['info.id', inst.id],
    ['info.state', stateText(inst.state)],
    ['info.created', fmtDate(inst.created_at)],
    ['info.activity', fmtDate(inst.last_activity_at)],
    ['info.idleStop', inst.idle_stop_at ? fmtDate(inst.idle_stop_at) : '—'],
    ['info.running', inst.running_agents.join(', ') || '—'],
    ['info.repo', inst.selected_repo || '—'],
  ];
  if (inst.last_error) rows.push(['info.error', inst.last_error]);
  $('#info').replaceChildren(...rows.flatMap(([k, v]) => [el('dt', { text: t(k) }), el('dd', { text: v })]));
  $('#detail-start').disabled = inst.state === 'running';
  $('#detail-stop').disabled = inst.state === 'stopped';
}

function connectionStatus(c) {
  const key = `conn.${c.status}`;
  return state.messages[key] ? t(key) : t('conn.unknown', c.status);
}

const TITLES = { claude: 'Claude Code', codex: 'Codex', antigravity: 'Google Antigravity', github: 'GitHub' };

function renderConnections(data) {
  const list = $('#connections');
  list.replaceChildren();
  const inst = data.instance;
  const gh = data.github;
  list.append(el('li', { class: 'conn' },
    el('div', { class: 'row between' }, el('strong', { text: TITLES.github }), el('span', { class: `badge ${gh.status}`, text: connectionStatus(gh) })),
    gh.account ? el('p', { class: 'muted', text: gh.account }) : null,
  ));
  for (const c of data.agents) {
    const item = el('li', { class: 'conn' },
      el('div', { class: 'row between' }, el('strong', { text: TITLES[c.provider] || c.provider }), el('span', { class: `badge ${c.status}`, text: connectionStatus(c) })),
      c.account ? el('p', { class: 'muted', text: `${c.method === 'api_key' ? t('conn.apiKey') : t('conn.account')}: ${c.account}` }) : null,
    );
    const actions = el('div', { class: 'row wrap' });
    actions.append(actionButton(t('conn.login'), (e) => startLogin(e.currentTarget, inst.id, c.provider, item)));
    if (c.method === 'account' && c.status === 'connected') {
      actions.append(actionButton(t('conn.logout'), (e) => logout(e.currentTarget, inst.id, c.provider)));
    }
    actions.append(apiKeyForm(inst.id, c.provider));
    if (c.method === 'api_key' && c.status === 'connected') {
      actions.append(actionButton(t('conn.removeKey'), (e) => removeKey(e.currentTarget, inst.id, c.provider), 'danger'));
    }
    item.append(actions);
    list.append(item);
  }
}

function apiKeyForm(instanceId, agent) {
  const id = `key-${agent}`;
  const form = el('form', { class: 'row wrap' },
    el('label', { for: id, class: 'visually-hidden', text: t('conn.keyLabel', TITLES[agent]) }),
    el('input', { id, type: 'password', autocomplete: 'off', placeholder: t('conn.keyPlaceholder'), required: true, spellcheck: 'false' }),
    el('button', { type: 'submit', text: t('conn.saveKey') }),
  );
  form.addEventListener('submit', async (event) => {
    event.preventDefault();
    const input = form.querySelector('input');
    await busy(form.querySelector('button'), async () => {
      try {
        await api('PUT', `/instances/${encodeURIComponent(instanceId)}/agents/${agent}/api-key`, { api_key: input.value });
        input.value = '';
        $('#detail-status').textContent = t('status.keySaved');
        loadDetail(state.instance.id, true, true);
      } catch (err) {
        $('#detail-status').textContent = errorText(err);
      }
    });
  });
  return form;
}

async function removeKey(button, instanceId, agent) {
  await busy(button, async () => {
    try {
      await api('DELETE', `/instances/${encodeURIComponent(instanceId)}/agents/${agent}/api-key`);
      $('#detail-status').textContent = t('status.keyRemoved');
    } catch (err) {
      $('#detail-status').textContent = errorText(err);
    }
  });
  loadDetail(state.instance.id, true, true);
}

async function logout(button, id, agent) {
  await busy(button, async () => {
    try {
      await api('POST', `/instances/${id}/agents/${agent}/logout`);
      $('#detail-status').textContent = t('status.loggedOut');
    } catch (err) {
      $('#detail-status').textContent = errorText(err);
    }
  });
  loadDetail(id, true, true);
}

async function startLogin(button, id, agent, item) {
  item.querySelector('.login')?.remove();
  const box = el('div', { class: 'login stack', role: 'group', 'aria-label': t('login.title') });
  item.append(box);
  await busy(button, async () => {
    try {
      const { login } = await api('POST', `/instances/${id}/agents/${agent}/login`);
      renderLogin(box, id, agent, login);
    } catch (err) {
      box.append(el('p', { class: 'error', role: 'alert', text: errorText(err) }));
    }
  });
}

function renderLogin(box, id, agent, login) {
  box.replaceChildren(el('p', { role: 'status', text: t(`login.${login.status}`) }));
  if (login.url) {
    box.append(el('p', {}, el('span', { text: t('login.open') + ' ' }), el('a', { href: login.url, target: '_blank', rel: 'noopener noreferrer', text: t('login.link') })));
  }
  if (login.code) box.append(el('p', {}, el('span', { text: t('login.code') + ' ' }), el('code', { text: login.code })));
  if (login.error) box.append(el('p', { class: 'error', text: t(`login.error.${login.error}`) }));
  if (login.needs_input && login.status === 'waiting_for_user') {
    const inputId = `login-code-${agent}`;
    const form = el('form', { class: 'row wrap' },
      el('label', { for: inputId, text: t('login.paste') }),
      el('input', { id: inputId, required: true, autocomplete: 'off', spellcheck: 'false' }),
      el('button', { type: 'submit', class: 'primary', text: t('login.submit') }),
    );
    form.addEventListener('submit', async (event) => {
      event.preventDefault();
      await busy(form.querySelector('button'), async () => {
        try {
          const res = await api('POST', `/instances/${id}/agents/${agent}/login/input`, { code: form.querySelector('input').value.trim() });
          renderLogin(box, id, agent, res.login);
        } catch (err) {
          box.append(el('p', { class: 'error', role: 'alert', text: errorText(err) }));
        }
      });
    });
    box.append(form);
  }
  if (login.status === 'starting' || login.status === 'waiting_for_user') {
    box.append(actionButton(t('action.cancel'), async (e) => {
      await busy(e.currentTarget, () => api('DELETE', `/instances/${id}/agents/${agent}/login`).catch(() => {}));
    }, 'ghost'));
    pollLogin(box, id, agent, login.status);
  }
}

function pollLogin(box, id, agent, status) {
  setTimeout(async () => {
    if (!box.isConnected) return;
    try {
      const res = await api('GET', `/instances/${id}/agents/${agent}/login`);
      if (res.login.status === status) return pollLogin(box, id, agent, status);
      renderLogin(box, id, agent, res.login);
      if (res.login.status === 'succeeded') loadDetail(id, true, true);
    } catch {}
  }, 3000);
}

async function loadHistory(id) {
  const { runs } = await api('GET', `/instances/${id}/history`);
  state.history = runs;
  const list = $('#history');
  list.replaceChildren();
  if (!runs.length) list.append(el('li', { class: 'muted', text: t('history.empty') }));
  for (const r of runs) {
    list.append(el('li', {},
      el('details', {},
        el('summary', {},
          el('strong', { text: TITLES[r.agent] || r.agent }), ' ',
          el('span', { class: `badge ${r.status}`, text: t(`run.${r.status}`) }), ' ',
          el('span', { class: 'muted', text: `${fmtDate(r.started_at)} · ${fmtDuration(r.duration_ms)}` }),
        ),
        el('p', { class: 'muted', text: r.prompt }),
        el('pre', { text: r.response || '—' }),
        r.session_id ? el('p', { class: 'muted', text: t('history.session', r.session_id) }) : null,
        r.error_code ? el('p', { class: 'error', text: `${r.error_code}: ${r.error_message || ''}` }) : null,
      ),
    ));
  }
}

function lastSession(agent) {
  return (state.history || []).find((r) => r.agent === agent && r.session_id)?.session_id || '';
}

async function sendPrompt(event) {
  event.preventDefault();
  const form = event.currentTarget;
  const inst = state.instance;
  const agent = form.agent.value;
  const body = { prompt: form.prompt.value };
  if (form.model.value.trim()) body.model = form.model.value.trim();
  if (form.resume.checked) {
    const session = lastSession(agent);
    if (session) body.session_id = session;
  }
  const button = form.querySelector('button[type=submit]');
  await busy(button, async () => {
    try {
      const data = await api('POST', `/instances/${inst.id}/agents/${agent}/run`, body);
      form.prompt.value = '';
      streamRun(inst.id, data.run);
    } catch (err) {
      $('#detail-status').textContent = errorText(err);
    }
  });
}

function streamRun(instanceId, run) {
  const node = $('#run-template').content.firstElementChild.cloneNode(true);
  node.querySelector('.run-agent').textContent = TITLES[run.agent] || run.agent;
  node.querySelector('.run-prompt').textContent = run.prompt;
  node.querySelector('.run-cancel').textContent = t('action.cancelRun');
  const statusEl = node.querySelector('.run-status');
  const output = node.querySelector('.run-output');
  const tools = node.querySelector('.run-tools');
  const errorEl = node.querySelector('.run-error');
  const cancel = node.querySelector('.run-cancel');
  const setStatus = (s) => { statusEl.textContent = t(`run.${s}`); statusEl.className = `badge run-status ${s}`; };
  setStatus('running');
  $('#runs').prepend(node);

  const controller = new AbortController();
  state.streams.set(run.id, controller);
  cancel.addEventListener('click', () => busy(cancel, () => api('POST', `/instances/${instanceId}/runs/${run.id}/cancel`).catch((err) => {
    errorEl.textContent = errorText(err);
    errorEl.hidden = false;
  })));

  const finish = (view) => {
    setStatus(view.status);
    if (!output.textContent && view.response) output.textContent = view.response;
    if (view.error_code) {
      errorEl.textContent = `${view.error_code}: ${view.error_message || ''}`;
      errorEl.hidden = false;
    }
    cancel.hidden = true;
    state.streams.delete(run.id);
    loadHistory(instanceId).catch(() => {});
    loadDetail(instanceId, true);
  };

  readStream(`/api/instances/${instanceId}/runs/${run.id}/stream`, controller.signal, (type, data) => {
    if (type === 'delta') output.textContent += data.text || '';
    else if (type === 'tool') tools.append(el('li', { text: data.detail ? `${data.tool}: ${data.detail}` : data.tool }));
    else if (type === 'done') finish(data);
  }).catch((err) => {
    if (controller.signal.aborted) return;
    errorEl.textContent = errorText(err);
    errorEl.hidden = false;
  });
}

async function readStream(url, signal, onEvent) {
  let lastId = 0;
  for (let attempt = 0; attempt < 5; attempt++) {
    const res = await fetch(url, { headers: { apikey: state.key, 'Last-Event-ID': String(lastId) }, signal });
    if (!res.ok) {
      const data = await res.json().catch(() => ({}));
      throw new ApiError(res.status, data.error?.code || 'internal_error', '');
    }
    const reader = res.body.pipeThrough(new TextDecoderStream()).getReader();
    let buffer = '';
    let done = false;
    while (!done) {
      const chunk = await reader.read();
      if (chunk.done) break;
      buffer += chunk.value;
      let index;
      while ((index = buffer.indexOf('\n\n')) >= 0) {
        const block = buffer.slice(0, index);
        buffer = buffer.slice(index + 2);
        let type = 'message';
        let data = '';
        for (const line of block.split('\n')) {
          if (line.startsWith('id: ')) lastId = Number(line.slice(4));
          else if (line.startsWith('event: ')) type = line.slice(7);
          else if (line.startsWith('data: ')) data += line.slice(6);
        }
        if (!data) continue;
        onEvent(type, JSON.parse(data));
        if (type === 'done') done = true;
      }
    }
    if (done) return;
    await new Promise((r) => setTimeout(r, 1000));
  }
}

async function main() {
  await loadMessages();
  $('#auth-form').addEventListener('submit', unlock);
  $('#lock').addEventListener('click', lock);
  $('#refresh').addEventListener('click', (e) => busy(e.currentTarget, loadList));
  $('#create-form').addEventListener('submit', createInstance);
  $('#chat-form').addEventListener('submit', sendPrompt);
  $('#detail-start').addEventListener('click', (e) => lifecycle(e.currentTarget, state.instance.id, 'start', () => loadDetail(state.instance.id)));
  $('#detail-stop').addEventListener('click', (e) => lifecycle(e.currentTarget, state.instance.id, 'stop', () => loadDetail(state.instance.id)));
  $('#detail-refresh').addEventListener('click', (e) => busy(e.currentTarget, () => loadDetail(state.instance.id)));
  window.addEventListener('hashchange', route);
  route();
}

main();
