// 端到端：在真实浏览器里用「表单」模式新建实例，验证保存链路。
// 用法: node tools/e2e-form.js <管理地址>
const { spawn } = require('child_process');

const baseUrl = process.argv[2] || 'http://127.0.0.1:1332';
const EDGE = process.env.EDGE_PATH || 'C:\\Program Files (x86)\\Microsoft\\Edge\\Application\\msedge.exe';
const PORT = 9334;
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

function connect(wsUrl) {
  const ws = new WebSocket(wsUrl);
  let id = 0;
  const waiting = new Map();
  const events = [];
  ws.onmessage = (m) => {
    const msg = JSON.parse(m.data);
    if (msg.id && waiting.has(msg.id)) {
      const { resolve, reject } = waiting.get(msg.id);
      waiting.delete(msg.id);
      msg.error ? reject(new Error(JSON.stringify(msg.error))) : resolve(msg.result);
    } else if (msg.method) events.push(msg.method);
  };
  const ready = new Promise((res, rej) => { ws.onopen = res; ws.onerror = rej; });
  const send = (method, params = {}) => new Promise((resolve, reject) => {
    const mid = ++id;
    waiting.set(mid, { resolve, reject });
    ws.send(JSON.stringify({ id: mid, method, params }));
  });
  return { ready, send, events, close: () => ws.close() };
}

(async () => {
  const profile = require('path').join(process.env.TEMP, 'edge-e2e-' + Date.now());
  const edge = spawn(EDGE, [
    '--headless=new', '--disable-gpu', '--no-first-run', '--no-default-browser-check',
    `--remote-debugging-port=${PORT}`, `--user-data-dir=${profile}`, 'about:blank',
  ], { stdio: 'ignore' });

  let target = null;
  for (let i = 0; i < 60 && !target; i++) {
    await sleep(500);
    try {
      const list = await (await fetch(`http://127.0.0.1:${PORT}/json/list`)).json();
      target = list.find((t) => t.type === 'page');
    } catch (_) {}
  }
  if (!target) throw new Error('无法连接 Edge');

  const cdp = connect(target.webSocketDebuggerUrl);
  await cdp.ready;
  await cdp.send('Page.enable');
  await cdp.send('Runtime.enable');

  const evaluate = async (expression) => {
    const r = await cdp.send('Runtime.evaluate', { expression, awaitPromise: true, returnByValue: true });
    if (r.exceptionDetails) throw new Error('页面报错: ' + JSON.stringify(r.exceptionDetails.exception));
    return r.result.value;
  };

  cdp.events.length = 0;
  await cdp.send('Page.navigate', { url: baseUrl });
  await sleep(1500);

  // 登录
  await evaluate(`(async () => {
    await api('/api/login', { method:'POST', body: JSON.stringify({username:'admin', password:'admin123'}) });
    hideLogin(); await initApp(); return 'ok';
  })()`);
  await sleep(600);

  // 用表单新建一个实例（指向必然连不上的端口，验证配置本身合法即可）
  const result = await evaluate(`(async () => {
    $('btn-new').click();
    $('inst-name').value = 'e2e表单实例';
    $('f-server').value = '127.0.0.1';
    $('f-port').value = '1';
    $('f-token').value = 'tk';
    const row = $('proxy-rows').children[0];
    row.querySelector('.f-name').value = 'web';
    row.querySelector('.f-localPort').value = '8080';
    row.querySelector('.f-remotePort').value = '6001';
    fire = (el, t) => el.dispatchEvent(new Event(t, { bubbles: true }));
    fire($('f-server'), 'input');
    await save();
    const list = await api('/api/instances');
    const cfg = await api('/api/instances/' + encodeURIComponent('e2e表单实例') + '/config');
    const me = list.find(i => i.name === 'e2e表单实例');
    return {
      found: !!me,
      state: me && me.state,
      toast: [...document.querySelectorAll('.toast')].map(t => t.textContent).join(' | '),
      modalOpen: $('modal-mask').classList.contains('show'),
      config: cfg.config,
    };
  })()`);

  // 表单不合法时不应关闭弹窗、不应发出请求
  const rejected = await evaluate(`(async () => {
    $('btn-new').click();
    $('inst-name').value = '不该被创建';
    $('f-server').value = '127.0.0.1';
    $('f-port').value = '70000';
    const row = $('proxy-rows').children[0];
    row.querySelector('.f-name').value = 'x';
    row.querySelector('.f-localPort').value = '80';
    row.querySelector('.f-remotePort').value = '80';
    fire = (el, t) => el.dispatchEvent(new Event(t, { bubbles: true }));
    fire($('f-port'), 'input');
    await save();
    const list = await api('/api/instances');
    return {
      created: list.some(i => i.name === '不该被创建'),
      modalOpen: $('modal-mask').classList.contains('show'),
      toast: [...document.querySelectorAll('.toast')].map(t => t.textContent).join(' | '),
    };
  })()`);

  cdp.close();
  edge.kill();
  await sleep(400);
  require('fs').rmSync(profile, { recursive: true, force: true });

  console.log('--- 表单创建 ---');
  console.log('  实例已创建:', result.found, '| 状态:', result.state);
  console.log('  弹窗已关闭:', !result.modalOpen);
  console.log('  提示:', result.toast);
  console.log('  落盘配置:\n' + result.config.split('\n').map((l) => '    ' + l).join('\n'));
  console.log('--- 非法端口（70000）应被拒 ---');
  console.log('  误创建:', rejected.created, '| 弹窗保持打开:', rejected.modalOpen);
  console.log('  提示:', rejected.toast);

  const ok = result.found && !result.modalOpen && !rejected.created && rejected.modalOpen &&
    rejected.toast.includes('服务端端口');
  if (!ok) { console.error('e2e 未通过'); process.exit(1); }
  console.log('\ne2e-form: 通过');
})().catch((e) => { console.error('e2e 失败：' + e.message); process.exit(1); });
