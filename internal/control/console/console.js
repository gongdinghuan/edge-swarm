'use strict';

const $ = id => document.getElementById(id);
const states = {
  ready: '可接单', probing: '后端未就绪', offline: '离线', draining: '排空', revoked: '已撤销',
  queued: '排队', running: '执行中', succeeded: '完成', failed: '失败',
  cancelled: '已取消', interrupted: '中断', completed_with_errors: '部分任务未成功',
};
const reasons = {stop: '生成完成', length: '达到输出上限', content_filter: '内容已过滤'};
const errors = {
  unauthorized: '业务密钥无效，请检查业务密钥。',
  admin_required: '管理员密钥无效，请检查管理员密钥。',
  database_unavailable: '中心服务在线，数据库暂不可用。',
  storage_error: '中心存储暂不可用，请稍后重试。',
  storage_unavailable: '中心存储暂不可用，请稍后重试。',
  invalid_request: '请求参数无效，请检查模型名、提示词和输出上限。',
  invalid_model: '模型名无效。',
  model_not_available: '当前业务密钥无权使用此模型，或模型名有误。',
  not_found: '未找到记录，请检查 ID 和密钥。',
  queue_full: '当前租户的队列已满，请等待已有任务结束。',
  create_failed: '提交失败，请检查队列或稍后重试。',
  execution_failed: '模型执行失败。请查看任务详情或检查设备后端。',
  execution_interrupted: '生成已中断，下方保留收到的内容。',
  interrupted: '生成已中断，下方保留收到的内容。',
  deadline_or_client_cancelled: '任务超时或已取消，请查看任务列表。',
  idempotency_conflict: '同一提交标识已用于不同内容。',
  batch_too_large: '批量内容过大，请减少提示词数量或长度。',
  invalid_batch_item: '批组中有无效的提示词或模型设置。',
  invalid_batch_manifest: '批组须包含 1 至 20 条有效提示词。',
};
const taskErrors = {
  'backend execution failed': '模型后端执行失败',
  'execution authority expired': '执行租约或任务期限已到',
  'invalid output sequence': '输出序列异常',
};
const pending = job => ['queued', 'running'].includes(job.status);
const label = status => states[status] || status || '—';
let business = {key: '', authorized: false};
let admin = {key: '', authorized: false};
let active = null, poll = null, busyBatch = null, busyEnroll = null, group = null;
let models = [], modelsLoaded = false, centralReady = false;
let recentJobs = [], registeredNodes = [], outputAvailable = false;
let apiFormat = 'python';
let selection = 0;

function setText(id, message) {
  const element = $(id);
  if (element && element.textContent !== message) element.textContent = message;
}

function element(name, className, text) {
  const node = document.createElement(name);
  node.className = className;
  if (text !== undefined) node.textContent = text;
  return node;
}

function tell(message, tone = 'info') {
  $('notice').dataset.tone = tone;
  setText('notice', message);
}

function displayError(error) {
  if (error.name === 'TimeoutError') return '请求超时，请稍后重试。';
  if (error instanceof SyntaxError) return '中心响应格式异常，请稍后重试。';
  if (error instanceof TypeError) return '无法连接中心，请检查网络和服务地址。';
  return error.message || '操作失败，请稍后重试。';
}

async function request(path, key, opts = {}) {
  const headers = new Headers(opts.headers);
  if (key) headers.set('Authorization', 'Bearer ' + key);
  if (opts.body) headers.set('Content-Type', 'application/json');
  const signal = opts.signal || AbortSignal.timeout(10000);
  const response = await fetch(path, {...opts, headers, signal, cache: 'no-store', redirect: 'error'});
  if (!response.ok) {
    const value = await response.json().catch(() => ({}));
    const error = new Error(errors[value.error?.code] || `请求失败（HTTP ${response.status}）。`);
    error.status = response.status;
    throw error;
  }
  return response;
}

async function json(path, key, opts) {
  return (await request(path, key, opts)).json();
}

function empty(id, text) {
  const element = $(id);
  if (!element) return;
  delete element.dataset.snapshot;
  const message = document.createElement('p');
  message.className = 'empty-state muted';
  message.textContent = text;
  element.replaceChildren(message);
}

function button(title, action) {
  const element = document.createElement('button');
  element.type = 'button';
  element.className = 'secondary compact';
  element.textContent = title;
  element.addEventListener('click', async () => {
    element.disabled = true;
    try { await action(); } catch (error) { tell(displayError(error), 'error'); }
    finally { element.disabled = false; }
  });
  return element;
}

function table(target, headers, rows) {
  if (!target) return;
  if (!rows.length) { empty(target.id, '暂无记录。'); return; }
  const element = document.createElement('table');
  const headings = element.createTHead().insertRow();
  for (const value of headers) {
    const heading = document.createElement('th');
    heading.scope = 'col';
    heading.textContent = value;
    headings.append(heading);
  }
  const body = element.createTBody();
  for (const values of rows) {
    const row = body.insertRow();
    for (const value of values) {
      const cell = row.insertCell();
      if (value instanceof Node) cell.append(value);
      else cell.textContent = String(value ?? '—');
    }
  }
  target.replaceChildren(element);
}

function tag(state, suffix = '') {
  const span = document.createElement('span');
  span.className = 'tag';
  span.dataset.status = state;
  span.textContent = label(state) + suffix;
  return span;
}

function shortID(value) {
  const id = document.createElement('code');
  id.className = 'mono';
  id.textContent = value.slice(0, 8);
  id.title = value;
  return id;
}

function updateModelStatus() {
  const selected = $('model').value.trim();
  let message = '连接业务密钥后可发现模型。';
  if (business.key && !modelsLoaded) message = '暂时无法确认模型是否可接单。';
  if (modelsLoaded && !models.length) message = '暂无可接单的模型。手动填写模型名仍可提交排队。';
  if (modelsLoaded && models.length) message = `发现 ${models.length} 个可接单模型。`;
  if (modelsLoaded && selected && !models.includes(selected)) message = '所选模型当前未就绪或不可见，提交后可能排队。';
  setText('model-status', message);
}

function updateButtons() {
  $('generate').disabled = !centralReady || !business.authorized || active !== null;
  $('stop').disabled = !active || active.stopped;
  $('batch').disabled = !centralReady || !business.authorized || busyBatch !== null;
  $('copy-output').disabled = !outputAvailable;
  setText('header-access-label', admin.authorized ? '管理员已连接' : business.authorized ? '业务已连接' : '连接密钥');
  const enrollButton = $('enroll').querySelector('button[type="submit"]');
  if (enrollButton) enrollButton.disabled = !centralReady || !admin.authorized || busyEnroll !== null;
}

function renderModels(entries) {
  models = entries.map(model => model.id).sort((a, b) => a.localeCompare(b));
  modelsLoaded = true;
  const options = $('models');
  if (!unchanged(options, models)) options.replaceChildren(...models.map(model => new Option(model, model)));
  if (!$('model').value && models.length === 1) $('model').value = models[0];
  setText('summary-models', String(models.length));
  const catalog = $('catalog-list');
  if (!unchanged(catalog, models)) {
    if (!models.length) empty('catalog-list', '当前业务密钥暂无可用模型。');
    else catalog.replaceChildren(...models.map(model => {
      const entry = element('article', 'catalog-model');
      entry.append(element('code', 'chip', model), button('使用模型', () => selectModel(model)));
      return entry;
    }));
  }
  updateModelStatus();
  renderAPIExample();
}

function unchanged(target, value) {
  const snapshot = JSON.stringify(value);
  if (target.dataset.snapshot === snapshot) return true;
  target.dataset.snapshot = snapshot;
  return false;
}

function taskPath(job) {
  return '/swarm/v1/' + (job.kind === 'batch' ? 'jobs' : 'requests') + '/' + encodeURIComponent(job.id);
}

async function showJob(job, credentials) {
  const selected = ++selection;
  setText('job-detail', '正在加载任务详情…');
  try {
    const current = await json(taskPath(job), credentials.key);
    if (business !== credentials || selection !== selected) return;
    setText('job-detail', [
      `任务：${current.id}`, `模型：${current.model} · ${label(current.status)}`,
      `尝试次数：${current.attempts ?? 0}`,
      current.finish_reason && `结束原因：${reasons[current.finish_reason] || current.finish_reason}`,
      current.error && `原因：${taskErrors[current.error] || current.error}`,
      current.result,
    ].filter(Boolean).join('\n'));
  } catch (error) {
    if (business === credentials && selection === selected) setText('job-detail', displayError(error));
  }
}

function jobActions(job, credentials) {
  const actions = document.createElement('div');
  actions.className = 'actions';
  actions.append(button('详情', () => {
    location.hash = 'job-detail-title';
    return showJob(job, credentials);
  }));
  if (pending(job)) actions.append(button('取消', async () => {
    await request(taskPath(job) + '/cancel', credentials.key, {method: 'POST'});
    if (business !== credentials) return;
    tell('已提交任务取消请求。');
    await refresh();
  }));
  return actions;
}

function renderJobs(items, credentials) {
  items.sort((a, b) => (b.created_ms ?? b.deadline_ms) - (a.created_ms ?? a.deadline_ms));
  recentJobs = items;
  renderActivity(items);
  const visible = items.slice(0, 20);
  if (unchanged($('jobs'), visible.map(job => [job.id, job.kind, job.model, job.status]))) return;
  table($('jobs'), ['任务', '模型', '类型', '状态', '操作'], visible.map(job => [
    shortID(job.id), job.model, job.kind === 'batch' ? '离线' : '对话', tag(job.status), jobActions(job, credentials),
  ]));
}

function renderActivity(items, message = '连接业务密钥后查看近期任务。') {
  for (const [id, state] of [['activity-queued', 'queued'], ['activity-running', 'running'], ['activity-done', 'succeeded']]) {
    const count = items ? String(items.filter(job => job.status === state).length) : '—';
    setText(id, count);
    if (state === 'running') setText('summary-tasks', count);
    if (state === 'queued') setText('summary-queued', count);
  }
  const target = $('activity-jobs');
  const jobs = items?.slice(0, 5) || [];
  if (unchanged(target, [message, Boolean(items), jobs.map(job => [job.id, job.model, job.status])])) return;
  if (!jobs.length) {
    target.replaceChildren(element('li', 'activity-empty muted', items ? '暂无近期任务。' : message));
    return;
  }
  target.replaceChildren(...jobs.map(job => {
    const item = element('li', 'activity-job');
    item.append(shortID(job.id), element('span', 'activity-model', job.model), tag(job.status));
    return item;
  }));
}

function renderTopology(nodes, message = '连接管理员密钥后查看节点拓扑。') {
  const target = $('topology-nodes');
  const connected = nodes?.filter(node => node.connected) || [];
  setText('summary-nodes', nodes ? String(connected.length) : '—');
  setText('summary-node-total', nodes ? String(nodes.length) : '—');
  setText('topology-topline', nodes ? `${connected.length} 个已连接 · ${connected.filter(node => node.state === 'ready').length} 个可接单` : '管理员视图');
  if (!connected.length) {
    const content = nodes ? '暂无已连接节点。' : message;
    if (!unchanged(target, content)) target.replaceChildren(element('p', 'empty-state muted', content));
    return;
  }
  const visible = connected.slice(0, 2);
  if (unchanged(target, [connected.length, visible.map(node => [node.id, node.state, node.active, node.model, node.gpu, node.vram_mib, node.backend_kind])])) return;
  const workers = visible.map(node => {
    const worker = element('article', 'topology-worker');
    const head = element('div', 'topology-worker-head');
    const name = element('strong', '', '工作节点');
    head.append(name, shortID(node.id), tag(node.state, node.active ? ' · 执行中' : ''));
    const meta = element('div', 'topology-worker-meta');
    meta.append(element('span', 'chip', node.model), element('span', 'muted',
      [node.backend_kind, node.gpu, node.vram_mib && `${node.vram_mib} MiB`].filter(Boolean).join(' · ') || '硬件信息未上报'));
    worker.append(head, meta);
    return worker;
  });
  if (connected.length > workers.length) {
    const more = element('a', 'topology-more', `查看全部 ${connected.length} 个在线节点`);
    more.href = '#devices-panel';
    workers.push(more);
  }
  target.replaceChildren(...workers);
}

function renderNodes(nodes, credentials) {
  registeredNodes = nodes;
  renderTopology(nodes);
  const modelFilter = $('node-model-filter');
  const modelNames = [...new Set(nodes.map(node => node.model))].sort((a, b) => a.localeCompare(b));
  if (!unchanged(modelFilter, modelNames)) {
    const selected = modelFilter.value;
    modelFilter.replaceChildren(new Option('全部模型', ''), ...modelNames.map(model => new Option(model, model)));
    if (modelNames.includes(selected)) modelFilter.value = selected;
  }
  renderFilteredNodes(credentials);
}

function renderFilteredNodes(credentials = admin) {
  if (!credentials.authorized) return;
  const query = $('node-filter').value.trim().toLowerCase(), state = $('node-state').value, model = $('node-model-filter').value;
  const nodes = registeredNodes.filter(node => (!state || node.state === state) && (!model || node.model === model) &&
    [node.id, node.model, node.gpu, node.backend_kind].some(value => value?.toLowerCase().includes(query)));
  if (!nodes.length) {
    empty('nodes', registeredNodes.length ? '没有符合筛选条件的节点。' : '暂无已注册的节点。');
    return;
  }
  const target = $('nodes');
  const snapshot = nodes.map(({last_seen, ...node}) => node);
  if (unchanged(target, snapshot)) {
    target.querySelectorAll('.node-last-seen').forEach((cell, i) => {
      const time = lastSeen(nodes[i]);
      if (cell.textContent !== time) cell.textContent = time;
    });
    return;
  }
  table(target, ['节点 ID', '连接', '就绪状态', '模型 ID', 'GPU', '总显存', '最近心跳', '操作'], nodes.map(node => {
    const actions = document.createElement('div');
    actions.className = 'actions';
    const path = '/swarm/v1/nodes/' + encodeURIComponent(node.id);
    const operate = async action => {
      await request(path + '/' + action, credentials.key, {method: 'POST'});
      if (admin === credentials) await refresh();
    };
    if (node.state !== 'revoked') {
      const draining = node.state === 'draining';
      actions.append(button(draining ? '恢复' : '排空', () => operate(draining ? 'resume' : 'drain')));
      actions.append(button('撤销', () => {
        if (confirm('撤销此设备的连接凭据？')) return operate('revoke');
      }));
    }
    const link = tag(node.connected ? 'ready' : 'offline');
    if (node.connected) link.textContent = '在线';
    const gpu = element('span', '', node.gpu || '—');
    if (node.backend_kind) gpu.title = node.backend_kind;
    return [shortID(node.id), link, tag(node.state, node.active ? ' · 执行中' : ''), element('span', 'chip', node.model),
      gpu, node.vram_mib ? `${node.vram_mib} MiB` : '—', element('span', 'node-last-seen', lastSeen(node)), actions];
  }));
}

function lastSeen(node) {
  if (!node.last_seen) return '—';
  const date = new Date(node.last_seen);
  return Number.isFinite(date.getTime()) ? date.toLocaleTimeString('zh-CN', {hour12: false}) : '—';
}

function groupPath(id) {
  return '/swarm/v1/batches/' + encodeURIComponent(id);
}

function renderGroup(batch, credentials) {
  group = batch;
  const target = $('batch-detail');
  if (!target) return;
  if (unchanged(target, [batch.id, batch.status, batch.jobs.map(job => [job.id, job.status])])) return;
  const title = document.createElement('p');
  const finished = batch.jobs.filter(job => !pending(job)).length;
  title.textContent = `批组 ${batch.id} · ${label(batch.status)} · 已结束 ${finished}/${batch.jobs.length}`;
  const list = document.createElement('div');
  list.className = 'table-scroll';
  target.replaceChildren(title);
  if (batch.jobs.some(pending)) {
    target.append(button('取消整批', async () => {
      await request(groupPath(batch.id) + '/cancel', credentials.key, {method: 'POST'});
      if (business !== credentials) return;
      setText('batch-status', '已提交整批取消请求。');
      await refresh();
    }));
  }
  target.append(list);
  table(list, ['序号', '任务', '状态', '操作'], batch.jobs.map((job, i) => [
    i + 1, shortID(job.id), tag(job.status), jobActions(job, credentials),
  ]));
}

async function refresh() {
  poll?.abort();
  const current = new AbortController();
  poll = current;
  const signal = AbortSignal.any([current.signal, AbortSignal.timeout(4500)]);
  const apiCredentials = business, adminCredentials = admin;
  const currentGroup = group;
  const fetchJSON = (path, credentials) => json(path, credentials?.key, {signal});
  const health = fetchJSON('/readyz').then(() => {
    if (poll !== current) return;
    centralReady = true;
    $('status').dataset.state = 'ready';
    setText('status', '控制中心就绪');
    updateButtons();
  }, error => {
    if (poll !== current) return;
    centralReady = false;
    $('status').dataset.state = 'offline';
    setText('status', error.status ? '控制中心暂未就绪' : '控制中心不可达');
    updateButtons();
  });
  const apiInfo = apiCredentials.key && fetchJSON('/v1/models', apiCredentials).then(value => {
    if (poll !== current) return;
    apiCredentials.authorized = true;
    setText('api-status', '业务密钥已连接');
    renderModels(value.data);
    updateButtons();
  }, error => {
    if (poll !== current) return;
    apiCredentials.authorized = false;
    models = [];
    modelsLoaded = false;
    emptyModelOptions();
    empty('catalog-list', displayError(error));
    setText('summary-models', '—');
    setText('api-status', displayError(error));
    updateModelStatus();
    updateButtons();
  });
  const taskInfo = apiCredentials.key && Promise.all([
    fetchJSON('/swarm/v1/jobs?limit=20', apiCredentials),
    fetchJSON('/swarm/v1/requests?limit=20', apiCredentials),
  ]).then(([jobs, requests]) => {
    if (poll === current) renderJobs([...jobs.data, ...requests.data], apiCredentials);
  }, error => {
    if (poll !== current) return;
    recentJobs = [];
    renderActivity(null, displayError(error));
    empty('jobs', displayError(error));
  });
  const nodeInfo = adminCredentials.key && fetchJSON('/swarm/v1/nodes', adminCredentials).then(value => {
    if (poll !== current) return;
    adminCredentials.authorized = true;
    setText('admin-status', '管理员密钥已连接');
    renderNodes(value.data, adminCredentials);
    updateButtons();
  }, error => {
    if (poll !== current) return;
    adminCredentials.authorized = false;
    resetNodes(displayError(error));
    setText('admin-status', displayError(error));
    updateButtons();
  });
  const groupInfo = currentGroup?.jobs.some(pending) && fetchJSON(groupPath(currentGroup.id), apiCredentials).then(value => {
    if (poll === current && group === currentGroup) renderGroup(value, apiCredentials);
  }, error => {
    if (poll === current && group === currentGroup) setText('batch-status', displayError(error));
  });
  await Promise.allSettled([health, apiInfo, taskInfo, nodeInfo, groupInfo]);
  if (poll === current && !$('search-results').hidden) renderSearch();
}

function resetNodes(message) {
  registeredNodes = [];
  const filter = $('node-model-filter');
  delete filter.dataset.snapshot;
  filter.replaceChildren(new Option('全部模型', ''));
  renderTopology(null, message);
  empty('nodes', message);
}

function emptyModelOptions() {
  const options = $('models');
  delete options.dataset.snapshot;
  options.replaceChildren();
}

function payload(model, prompt, tokens, stream, temperature) {
  const body = {model, messages: [{role: 'user', content: prompt}], max_tokens: tokens, stream};
  if (temperature !== undefined) body.temperature = temperature;
  return body;
}

function apiExample(format, origin, body) {
  const quote = value => "'" + value.replace(/'/g, "'\"'\"'") + "'";
  if (format === 'curl') return [
    `curl ${body.stream ? '-N ' : ''}${quote(origin + '/v1/chat/completions')} \\`,
    '  -H "Authorization: Bearer $SWARM_API_KEY" \\',
    "  -H 'Content-Type: application/json' \\",
    `  --data ${quote(JSON.stringify(body))}`,
  ].join('\n');
  return [
    '# OpenAI 兼容客户端', 'import os', 'from openai import OpenAI', '', 'client = OpenAI(',
    `    base_url=${JSON.stringify(origin + '/v1')},`, '    api_key=os.environ["SWARM_API_KEY"],', ')', '',
    'response = client.chat.completions.create(', `    model=${JSON.stringify(body.model)},`,
    `    messages=${JSON.stringify(body.messages)},`, `    max_tokens=${body.max_tokens},`,
    `    temperature=${body.temperature},`, `    stream=${body.stream ? 'True' : 'False'},`, ')', '',
    ...(body.stream ? ['for event in response:', '    if event.choices:', '        print(event.choices[0].delta.content or "", end="", flush=True)']
      : ['print(response.choices[0].message.content)']),
  ].join('\n');
}

function renderAPIExample() {
  const body = payload($('model').value.trim() || 'your-model', '请介绍一下蜂群。', Number($('tokens').value),
    $('stream-mode').checked, Number($('temperature').value));
  $('api-format-python').setAttribute('aria-pressed', String(apiFormat === 'python'));
  $('api-format-curl').setAttribute('aria-pressed', String(apiFormat === 'curl'));
  setText('api-example', apiExample(apiFormat, location.origin, body));
}

function selectModel(model) {
  $('model').value = model;
  updateModelStatus();
  renderAPIExample();
  location.hash = 'inference-panel';
  $('model').focus({preventScroll: true});
}

function renderSearch() {
  const target = $('search-results');
  const query = $('console-search').value.trim().toLowerCase();
  if (!query) { target.hidden = true; return; }
  const hits = [
    ...models.filter(model => model.toLowerCase().includes(query)).map(model => ['模型', model, () => selectModel(model)]),
    ...registeredNodes.filter(node => [node.id, node.model].some(value => value.toLowerCase().includes(query)))
      .map(node => ['节点', node.id, () => {
        $('node-filter').value = node.id;
        $('node-state').value = $('node-model-filter').value = '';
        renderFilteredNodes();
        location.hash = 'devices-panel';
        $('node-filter').focus({preventScroll: true});
      }]),
    ...recentJobs.filter(job => [job.id, job.model].some(value => value.toLowerCase().includes(query)))
      .map(job => ['任务', `${job.id.slice(0, 8)} · ${job.model}`, () => {
        location.hash = 'job-detail-title';
        $('job-detail').focus({preventScroll: true});
        return showJob(job, business);
      }]),
  ].slice(0, 7);
  target.hidden = false;
  if (unchanged(target, [query, hits.map(hit => hit.slice(0, 2))])) return;
  if (!hits.length) {
    target.replaceChildren(element('p', 'search-empty muted', '当前已加载数据中没有匹配项。'));
    return;
  }
  target.replaceChildren(...hits.map(([kind, name, action]) => {
    const hit = button('', () => { target.hidden = true; return action(); });
    hit.className = 'search-hit';
    hit.append(element('span', 'search-kind', kind), document.createTextNode(name));
    return hit;
  }));
}

async function copyText(text, message) {
  try { await navigator.clipboard.writeText(text); tell(message, 'success'); }
  catch { tell('当前浏览器无法写入剪贴板，请选中文本后手动复制。', 'error'); }
}

function batchItems(text, model, tokens) {
  const prompts = text.split(/\r?\n/).map(line => line.trim()).filter(Boolean);
  if (prompts.length < 1 || prompts.length > 20) throw new Error('请输入 1 至 20 条提示词，每行一条。');
  return prompts.map(prompt => payload(model, prompt, tokens, false));
}

async function readStream(body, onDelta) {
  if (!body) throw new Error('中心未返回流式响应。');
  const reader = body.getReader();
  const decoder = new TextDecoder();
  let buffer = '', finish = '';
  try {
    for (;;) {
      const chunk = await reader.read();
      if (chunk.done) throw new Error('生成连接已中断。可在任务列表检查状态。');
      buffer += decoder.decode(chunk.value, {stream: true});
      const frames = buffer.split(/\r?\n\r?\n/);
      buffer = frames.pop();
      for (const frame of frames) {
        const data = frame.split(/\r?\n/).filter(line => line.startsWith('data:'))
          .map(line => line.slice(5).replace(/^ /, '')).join('\n');
        if (!data) continue;
        if (data === '[DONE]') return finish;
        let event;
        try { event = JSON.parse(data); } catch { throw new Error('收到无法读取的模型输出，生成已中断。'); }
        if (event.error) throw new Error(errors[event.error.code] || '模型输出中断。');
        const choice = event.choices?.[0];
        if (choice?.delta?.content) onDelta(choice.delta.content);
        if (choice?.finish_reason) finish = choice.finish_reason;
      }
    }
  } finally {
    await reader.cancel().catch(() => {});
    reader.releaseLock();
  }
}

function stopStream(stream) {
  if (stream.stopped) return;
  stream.stopped = true;
  stream.controller.abort();
  if (stream.id) {
    request('/swarm/v1/requests/' + encodeURIComponent(stream.id) + '/cancel', stream.credentials.key, {method: 'POST'})
      .catch(() => {});
  }
  updateButtons();
}

function hideToken() {
  setText('node-token', '');
  $('node-secret').hidden = true;
}

function streamStatus(text, state = '') {
  $('stream-status').dataset.status = state;
  setText('stream-status', text);
}

function init() {
  $('credentials').addEventListener('submit', event => {
    event.preventDefault();
    const apiKey = $('api-key').value.trim(), adminKey = $('admin-key').value.trim();
    if (business.key !== apiKey) {
      business = {key: apiKey, authorized: false};
      if (active) stopStream(active);
      active = null;
      busyBatch = null;
      group = null;
      models = [];
      modelsLoaded = false;
      recentJobs = [];
      outputAvailable = false;
      emptyModelOptions();
      setText('output', '推理结果将在这里显示。');
      streamStatus('等待提交');
      setText('summary-latency', '—');
      setText('job-detail', '选择任务查看详情。');
      setText('batch-status', '');
      setText('batch-lookup-status', '');
      if ($('batch-id')) $('batch-id').value = '';
      setText('summary-models', '—');
      renderActivity(null, apiKey ? '正在加载近期任务…' : '连接业务密钥后查看近期任务。');
      empty('catalog-list', apiKey ? '正在加载模型…' : '连接业务密钥后可查看模型目录。');
      empty('batch-detail', '提交批组后可在这里查看进度。');
      empty('jobs', apiKey ? '正在加载任务…' : '连接业务密钥后可查看当前租户的任务。');
      setText('api-status', apiKey ? '正在验证业务密钥…' : '业务密钥未连接');
      updateModelStatus();
    }
    if (admin.key !== adminKey) {
      admin = {key: adminKey, authorized: false};
      busyEnroll = null;
      hideToken();
      resetNodes(adminKey ? '正在加载设备…' : '连接管理员密钥后可管理设备。');
      setText('admin-status', adminKey ? '正在验证管理员密钥…' : '管理员密钥未连接');
    }
    const results = $('search-results');
    results.hidden = true;
    delete results.dataset.snapshot;
    results.replaceChildren();
    tell('');
    updateButtons();
    refresh();
  });
  $('refresh').addEventListener('click', refresh);
  $('refresh-nodes').addEventListener('click', refresh);
  $('model').addEventListener('input', () => { updateModelStatus(); renderAPIExample(); });
  $('tokens').addEventListener('input', renderAPIExample);
  $('stream-mode').addEventListener('change', renderAPIExample);
  const temperature = $('temperature');
  temperature.addEventListener('input', () => { setText('temperature-value', temperature.value); renderAPIExample(); });
  const prompt = $('prompt');
  const countPrompt = () => setText('prompt-count', `${prompt.value.length} / ${prompt.maxLength}`);
  prompt.addEventListener('input', countPrompt);
  $('prompt-example').addEventListener('click', () => {
    if (!prompt.value.trim()) prompt.value = '请用三条简短的要点介绍蜂群如何调度异地 GPU。';
    countPrompt();
    prompt.focus();
  });
  $('prompt-clear').addEventListener('click', () => { prompt.value = ''; countPrompt(); prompt.focus(); });
  $('copy-output').addEventListener('click', () => copyText($('output').textContent, '已复制生成结果。'));
  $('copy-api').addEventListener('click', () => copyText($('api-example').textContent, '已复制 API 调用示例。'));
  for (const format of ['python', 'curl']) {
    $('api-format-' + format).addEventListener('click', () => { apiFormat = format; renderAPIExample(); });
  }
  $('node-filter').addEventListener('input', () => renderFilteredNodes());
  for (const filter of ['node-state', 'node-model-filter']) $(filter).addEventListener('change', () => renderFilteredNodes());
  const search = $('console-search'), searchResults = $('search-results');
  search.addEventListener('input', renderSearch);
  search.addEventListener('focus', renderSearch);
  search.addEventListener('keydown', event => {
    if (event.key === 'Escape') searchResults.hidden = true;
    if (event.key === 'Enter' && !searchResults.hidden) {
      event.preventDefault();
      searchResults.querySelector('button')?.click();
    }
  });
  document.addEventListener('focusin', event => {
    if (event.target !== search && !searchResults.contains(event.target)) searchResults.hidden = true;
  });
  const markNavigation = () => {
    const current = $(location.hash.slice(1))?.closest('section[id]')?.id || 'overview';
    for (const link of document.querySelectorAll('.section-nav a')) {
      if (link.hash === '#' + current) link.setAttribute('aria-current', 'location');
      else link.removeAttribute('aria-current');
    }
  };
  window.addEventListener('hashchange', markNavigation);
  $('hide-token').addEventListener('click', hideToken);
  $('enroll').addEventListener('submit', async event => {
    event.preventDefault();
    if (!admin.authorized || busyEnroll) return;
    const credentials = busyEnroll = admin;
    updateButtons();
    tell('');
    try {
      const node = await json('/swarm/v1/nodes/enroll', credentials.key, {
        method: 'POST', body: JSON.stringify({model: $('node-model').value.trim()}),
      });
      if (admin !== credentials) return;
      setText('node-token', `SWARM_NODE_ID=${node.node_id}\nSWARM_NODE_TOKEN=${node.token}`);
      $('node-secret').hidden = false;
      await refresh();
    } catch (error) {
      if (admin === credentials) tell(displayError(error), 'error');
    } finally {
      if (busyEnroll === credentials) busyEnroll = null;
      updateButtons();
    }
  });

  $('batch-form')?.addEventListener('submit', async event => {
    event.preventDefault();
    if (!business.authorized || busyBatch) return;
    if (!$('model').reportValidity() || !$('tokens').reportValidity()) return;
    const credentials = busyBatch = business;
    updateButtons();
    try {
      const model = $('model').value.trim();
      if (!model) throw new Error('请输入模型名。');
      const items = batchItems($('batch-prompts').value, model, Number($('tokens').value));
      setText('batch-status', `正在提交 ${items.length} 条任务…`);
      const batch = await json('/swarm/v1/batches', credentials.key, {
        method: 'POST', body: JSON.stringify({items}),
      });
      if (business !== credentials) return;
      renderGroup(batch, credentials);
      if ($('batch-id')) $('batch-id').value = batch.id;
      setText('batch-status', `已提交 ${batch.jobs.length} 条任务，可在任务区查看或取消整批。`);
      await refresh();
    } catch (error) {
      if (business === credentials) setText('batch-status', displayError(error));
    } finally {
      if (busyBatch === credentials) busyBatch = null;
      updateButtons();
    }
  });

  $('batch-lookup')?.addEventListener('submit', async event => {
    event.preventDefault();
    const credentials = business;
    const lookupStatus = $('batch-lookup-status') ? 'batch-lookup-status' : 'batch-status';
    if (!credentials.authorized) {
      setText(lookupStatus, '请先连接有效的业务密钥。');
      return;
    }
    const id = $('batch-id').value.trim().toLowerCase();
    if (!/^[0-9a-f]{32}$/.test(id)) {
      setText(lookupStatus, '请输入完整的 32 位批组 ID。');
      return;
    }
    setText(lookupStatus, '正在查找批组…');
    try {
      const batch = await json(groupPath(id), credentials.key);
      if (business !== credentials || $('batch-id').value.trim().toLowerCase() !== id) return;
      renderGroup(batch, credentials);
      setText(lookupStatus, '已加载批组。');
    } catch (error) {
      if (business === credentials && $('batch-id').value.trim().toLowerCase() === id) {
        setText(lookupStatus, displayError(error));
      }
    }
  });

  $('stop').addEventListener('click', () => {
    if (!active) return;
    streamStatus('正在停止…', 'queued');
    stopStream(active);
  });
  $('inference').addEventListener('submit', async event => {
    event.preventDefault();
    if (!business.authorized || active) return;
    const model = $('model').value.trim(), prompt = $('prompt').value;
    if (!model || !prompt.trim()) { tell('请输入模型名和提示词。', 'error'); return; }
    const streaming = $('stream-mode').checked, started = performance.now();
    const stream = active = {controller: new AbortController(), credentials: business, id: '', stopped: false};
    outputAvailable = false;
    updateButtons();
    setText('output', '');
    streamStatus('等待模型响应…', 'queued');
    tell('');
    try {
      const response = await request('/v1/chat/completions', stream.credentials.key, {
        method: 'POST', signal: stream.controller.signal,
        body: JSON.stringify(payload(model, prompt, Number($('tokens').value), streaming, Number(temperature.value))),
      });
      stream.id = response.headers.get('X-Request-ID') || '';
      let reason;
      if (streaming) {
        reason = await readStream(response.body, content => {
          if (active !== stream || stream.stopped) return;
          if (!outputAvailable) {
            outputAvailable = true;
            setText('summary-latency', String(Math.round(performance.now() - started)));
            streamStatus('正在流式生成…', 'running');
            updateButtons();
          }
          const output = $('output');
          const follow = output.scrollHeight - output.scrollTop - output.clientHeight < 24;
          output.append(document.createTextNode(content));
          if (follow) output.scrollTop = output.scrollHeight;
        });
      } else {
        const choice = (await response.json()).choices?.[0];
        reason = choice?.finish_reason;
        if (active === stream && !stream.stopped && typeof choice?.message?.content === 'string') {
          outputAvailable = Boolean(choice.message.content);
          setText('output', choice.message.content);
        }
      }
      if (active === stream) {
        streamStatus(stream.stopped ? '已停止接收输出' : reasons[reason] || '生成完成', stream.stopped ? 'cancelled' : 'succeeded');
        if (!$('output').textContent) setText('output', '模型未返回文本。');
      }
    } catch (error) {
      stream.controller.abort();
      if (active !== stream) return;
      streamStatus(stream.stopped ? '已停止接收输出' : '生成中断', stream.stopped ? 'cancelled' : 'interrupted');
      if (!stream.stopped) tell(displayError(error), 'error');
    } finally {
      if (active === stream) {
        active = null;
        updateButtons();
        refresh();
      }
    }
  });

  countPrompt();
  renderAPIExample();
  markNavigation();
  updateButtons();
  refresh();
  setInterval(refresh, 5000);
}

if (typeof document !== 'undefined') init();
