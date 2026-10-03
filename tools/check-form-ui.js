// 用 jsdom 真实加载 index.html，验证「表单 / TOML」切换的实际交互。
// 运行：JSDOM_PATH=<jsdom 路径> node tools/check-form-ui.js
const fs = require('fs');
const path = require('path');
const assert = require('assert');

let JSDOM = null;
for (const candidate of [process.env.JSDOM_PATH, 'jsdom'].filter(Boolean)) {
  try { ({ JSDOM } = require(candidate)); break; } catch (_) { /* 继续找 */ }
}
if (!JSDOM) {
  console.error('需要 jsdom：设置环境变量 JSDOM_PATH 指向 jsdom 目录');
  process.exit(2);
}

const html = fs.readFileSync(path.join(__dirname, '..', 'internal', 'server', 'static', 'index.html'), 'utf8');
const pending = [];
let confirmAnswer = true;

const dom = new JSDOM(html, {
  runScripts: 'dangerously',
  url: 'http://localhost:1332/',
  beforeParse(window) {
    // 页面初始化会请求这些接口，这里全部挂起，避免网络噪音
    window.fetch = (url) => new Promise((resolve) => pending.push({ url: String(url), resolve }));
    window.confirm = () => confirmAnswer;
    window.alert = () => {};
  },
});

const { window } = dom;
const { document } = window;
const $ = (id) => document.getElementById(id);
const fire = (el, type) => el.dispatchEvent(new window.Event(type, { bubbles: true }));
const click = (el) => el.dispatchEvent(new window.MouseEvent('click', { bubbles: true }));
const tick = () => new Promise((r) => setImmediate(r));
const toastText = () => [...document.querySelectorAll('.toast')].map((t) => t.textContent).join(' | ');

(async () => {
  // 让挂起的 /api/session 返回 401，页面停在登录页（不影响模态框逻辑）
  pending.splice(0).forEach((p) => p.resolve({ ok: false, status: 401, json: async () => ({}) }));

  // --- 新建实例默认进入表单模式 ---
  click($('btn-new'));
  assert.strictEqual($('modal-mask').classList.contains('show'), true, '模态框未打开');
  assert.strictEqual($('mode-form').classList.contains('active'), true, '新建时应默认表单模式');
  assert.strictEqual($('form-mode').style.display, '', '表单区应可见');
  assert.strictEqual($('toml-mode').style.display, 'none', 'TOML 区应隐藏');
  assert.strictEqual($('proxy-rows').children.length, 1, '应默认带一个代理行');

  // --- 代理类型切换：字段按类型显隐 ---
  const firstRow = () => $('proxy-rows').children[0];
  const typeSel = firstRow().querySelector('.f-type');
  const visible = (f) => firstRow().querySelector('.w-' + f).style.display !== 'none';
  assert.strictEqual(visible('remotePort'), true, 'tcp 应显示远程端口');
  assert.strictEqual(visible('customDomains'), false, 'tcp 不应显示自定义域名');
  typeSel.value = 'http'; fire(typeSel, 'change');
  assert.strictEqual(visible('remotePort'), false, 'http 不应显示远程端口');
  assert.strictEqual(visible('customDomains'), true, 'http 应显示自定义域名');
  typeSel.value = 'stcp'; fire(typeSel, 'change');
  assert.strictEqual(visible('secretKey'), true, 'stcp 应显示密钥');
  typeSel.value = 'tcp'; fire(typeSel, 'change');

  // --- 添加 / 删除代理行 + 序号 ---
  const titles = () => [...$('proxy-rows').children].map((b) => b.querySelector('.proxy-box-head b').textContent);
  assert.deepStrictEqual(titles(), ['代理 1'], '单个代理应显示序号 1');
  click($('add-proxy'));
  assert.strictEqual($('proxy-rows').children.length, 2, '添加代理失败');
  assert.deepStrictEqual(titles(), ['代理 1', '代理 2'], '多代理序号错误：' + titles());
  // 填好第一个代理，再删掉后面新增的那个（保持 firstRow() 仍指向有用的一行）
  firstRow().querySelector('.f-name').value = 'web';
  click($('proxy-rows').children[1].querySelector('.row-del'));
  assert.strictEqual($('proxy-rows').children.length, 1, '删除代理失败');
  assert.deepStrictEqual(titles(), ['代理 1'], '删除后序号应重新编号：' + titles());

  // --- 表单不完整时切 TOML：应被拦下并提示 ---
  $('f-server').value = ''; fire($('f-server'), 'input');
  click($('mode-toml'));
  assert.strictEqual($('mode-form').classList.contains('active'), true, '表单不完整时应留在表单模式');
  assert.ok(toastText().includes('服务端地址'), '应提示缺少服务端地址，实际：' + toastText());

  // --- 填完整后切 TOML：生成内容正确 ---
  $('f-server').value = 'frps.example.com'; fire($('f-server'), 'input');
  $('f-port').value = '7000'; fire($('f-port'), 'input');
  $('f-token').value = 'tok'; fire($('f-token'), 'input');
  firstRow().querySelector('.f-localPort').value = '8080'; fire(firstRow().querySelector('.f-localPort'), 'input');
  firstRow().querySelector('.f-remotePort').value = '6001'; fire(firstRow().querySelector('.f-remotePort'), 'input');
  click($('mode-toml'));
  assert.strictEqual($('mode-toml').classList.contains('active'), true, '应切到 TOML 模式');
  const toml = $('inst-config').value;
  assert.ok(toml.includes('serverAddr = "frps.example.com"'), 'TOML 缺 serverAddr:\n' + toml);
  assert.ok(toml.includes('serverPort = 7000'), 'TOML 缺 serverPort');
  assert.ok(toml.includes('auth.token = "tok"'), 'TOML 缺 token');
  assert.ok(toml.includes('loginFailExit = false'), 'TOML 缺 loginFailExit');
  assert.ok(toml.includes('name = "web"') && toml.includes('remotePort = 6001'), 'TOML 缺代理:\n' + toml);

  // --- 手改 TOML 后切回表单：需要确认 ---
  fire($('inst-config'), 'input');
  confirmAnswer = false;
  click($('mode-form'));
  assert.strictEqual($('mode-toml').classList.contains('active'), true, '取消确认后应仍停在 TOML');
  confirmAnswer = true;
  click($('mode-form'));
  assert.strictEqual($('mode-form').classList.contains('active'), true, '确认后应切回表单');

  // --- 未改动表单时切 TOML 不应覆盖已有内容 ---
  click($('modal-close'));
  click($('btn-new'));
  $('inst-config').value = 'KEEP-ME';
  click($('mode-toml'));
  assert.strictEqual($('inst-config').value, 'KEEP-ME', '表单未改动时切 TOML 不应覆盖');
  click($('modal-close'));

  // --- 编辑已有实例：不提供表单入口 ---
  window.openEdit('demo');
  await tick();                              // 等 openEdit 发出请求
  pending.splice(0).forEach((p) => p.resolve({ ok: true, status: 200, json: async () => ({ config: 'serverAddr = "x"\n' }) }));
  await tick(); await tick();
  assert.strictEqual($('modal-mask').classList.contains('show'), true, '编辑模态框未打开');
  assert.strictEqual($('mode-seg').style.display, 'none', '编辑时不应显示表单/TOML 切换');
  assert.strictEqual($('toml-mode').style.display, '', '编辑时应显示 TOML');
  assert.strictEqual($('inst-config').value, 'serverAddr = "x"\n', '编辑应回显原配置');

  console.log('check-form-ui: 全部通过');
  window.close();
})().catch((e) => {
  console.error('check-form-ui 失败：' + e.message);
  process.exit(1);
});
