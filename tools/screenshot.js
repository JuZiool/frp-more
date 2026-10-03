// 用 CDP 驱动 Edge 截图，产出 README 用的界面预览图。
// 用法: node tools/screenshot.js <管理地址> <输出目录>
const fs = require('fs');
const path = require('path');
const { spawn } = require('child_process');

const baseUrl = process.argv[2] || 'http://127.0.0.1:1332';
const outDir = process.argv[3] || 'doc';
const EDGE = process.env.EDGE_PATH || 'C:\\Program Files (x86)\\Microsoft\\Edge\\Application\\msedge.exe';
const PORT = 9333;

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

async function http(url, opts) {
  const res = await fetch(url, opts);
  return res.json();
}

function connect(wsUrl) {
  const ws = new WebSocket(wsUrl);
  let id = 0;
  const waiting = new Map();
  const events = [];
  const ready = new Promise((resolve, reject) => {
    ws.onopen = () => resolve();
    ws.onerror = (e) => reject(new Error('ws error: ' + e.message));
  });
  ws.onmessage = (m) => {
    const msg = JSON.parse(m.data);
    if (msg.id && waiting.has(msg.id)) {
      const { resolve, reject } = waiting.get(msg.id);
      waiting.delete(msg.id);
      msg.error ? reject(new Error(JSON.stringify(msg.error))) : resolve(msg.result);
    } else if (msg.method) {
      events.push(msg.method);
    }
  };
  const send = (method, params = {}) => new Promise((resolve, reject) => {
    const mid = ++id;
    waiting.set(mid, { resolve, reject });
    ws.send(JSON.stringify({ id: mid, method, params }));
  });
  return { ready, send, events, close: () => ws.close() };
}

async function waitFor(cdp, method, timeout = 15000) {
  const start = Date.now();
  while (Date.now() - start < timeout) {
    if (cdp.events.includes(method)) return;
    await sleep(50);
  }
  throw new Error('等待事件超时: ' + method);
}

(async () => {
  fs.mkdirSync(outDir, { recursive: true });
  const profile = path.join(process.env.TEMP, 'edge-shot-' + Date.now());
  const edge = spawn(EDGE, [
    '--headless=new', '--disable-gpu', '--no-first-run', '--no-default-browser-check',
    '--hide-scrollbars', '--force-device-scale-factor=1',
    `--remote-debugging-port=${PORT}`, `--user-data-dir=${profile}`, 'about:blank',
  ], { stdio: 'ignore' });

  let target = null;
  for (let i = 0; i < 60 && !target; i++) {
    await sleep(500);
    try {
      const list = await http(`http://127.0.0.1:${PORT}/json/list`);
      target = list.find((t) => t.type === 'page');
    } catch (_) { /* 还没起来 */ }
  }
  if (!target) throw new Error('无法连接 Edge 调试端口');

  const cdp = connect(target.webSocketDebuggerUrl);
  await cdp.ready;
  await cdp.send('Page.enable');
  await cdp.send('Runtime.enable');

  const evaluate = async (expression) => {
    const r = await cdp.send('Runtime.evaluate', { expression, awaitPromise: true, returnByValue: true });
    if (r.exceptionDetails) throw new Error('页面脚本报错: ' + JSON.stringify(r.exceptionDetails.exception));
    return r.result.value;
  };

  const goto = async (url) => {
    cdp.events.length = 0;
    await cdp.send('Page.navigate', { url });
    await waitFor(cdp, 'Page.loadEventFired');
    await sleep(600);
  };

  const shot = async (name, width, height) => {
    await cdp.send('Emulation.setDeviceMetricsOverride', { width, height, deviceScaleFactor: 1, mobile: false });
    await sleep(400);
    const r = await cdp.send('Page.captureScreenshot', { format: 'png' });
    const file = path.join(outDir, name);
    fs.writeFileSync(file, Buffer.from(r.data, 'base64'));
    console.log('已保存', file);
  };

  // 1. 主界面
  await goto(baseUrl);
  await evaluate(`(async () => {
    await api('/api/login', { method:'POST', body: JSON.stringify({username:'admin', password:'admin123'}) });
    hideLogin(); await initApp();
    return 'ok';
  })()`);
  await sleep(800);
  await shot('preview.png', 1000, 760);

  // 2. 窄屏无横向溢出
  await cdp.send('Emulation.setDeviceMetricsOverride', { width: 375, height: 700, deviceScaleFactor: 1, mobile: true });
  await sleep(400);
  const overflow = await evaluate(`({
    scrollWidth: document.documentElement.scrollWidth,
    innerWidth: window.innerWidth,
    worst: [...document.querySelectorAll('body *')]
      .filter(el => el.getBoundingClientRect().right > window.innerWidth + 1)
      .map(el => el.tagName + '.' + el.className).slice(0, 5),
  })`);
  console.log('窄屏 375px：scrollWidth=' + overflow.scrollWidth + ' innerWidth=' + overflow.innerWidth +
    ' 溢出元素=' + JSON.stringify(overflow.worst));

  // 3. 新建实例：表单模式
  await cdp.send('Emulation.setDeviceMetricsOverride', { width: 1000, height: 820, deviceScaleFactor: 1, mobile: false });
  await sleep(300);
  await evaluate(`(async () => {
    $('btn-new').click();
    $('inst-name').value = '办公室-web';
    $('f-server').value = 'frps.example.com';
    $('f-port').value = '7000';
    $('f-token').value = 'my-token';
    const row = $('proxy-rows').children[0];
    row.querySelector('.f-name').value = 'web';
    row.querySelector('.f-localPort').value = '8080';
    row.querySelector('.f-remotePort').value = '6001';
    return 'ok';
  })()`);
  await sleep(400);
  await shot('preview-form.png', 1000, 820);

  cdp.close();
  edge.kill();
  await sleep(500);
  fs.rmSync(profile, { recursive: true, force: true });
  console.log('完成');
})().catch((e) => { console.error('截图失败：' + e.message); process.exit(1); });
