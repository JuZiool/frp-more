// 变异测试：把生成逻辑故意改坏，确认自测会失败（即测试不是空转）。
// 用法：node tools/mutation-check.js <jsdom 目录>
const fs = require('fs');
const path = require('path');
const { execFileSync } = require('child_process');

const root = path.join(__dirname, '..');
const htmlPath = path.join(root, 'internal', 'server', 'static', 'index.html');
const original = fs.readFileSync(htmlPath, 'utf8');
const jsdomPath = process.argv[2] || process.env.JSDOM_PATH || 'jsdom';

const mutations = {
  '去掉 TOML 转义': (s) => s.replace(
    `const tomlStr = (s) => '"' + String(s).replace(/\\\\/g, '\\\\\\\\').replace(/"/g, '\\\\"') + '"';`,
    `const tomlStr = (s) => '"' + s + '"';`
  ),
  '端口不再做范围校验': (s) => s.replace(
    'return /^\\d+$/.test(String(v)) && +v >= 1 && +v <= 65535 ? +v : 0;',
    'return +v;'
  ),
  '代理重名不再拦截': (s) => s.replace(
    'if (seen.has(name)) throw new Error(`代理名称重复：${name}`);',
    ''
  ),
  'http 类型也写 remotePort': (s) => s.replace(
    "http: ['customDomains', 'subdomain'], https: ['customDomains', 'subdomain'],",
    "http: ['customDomains', 'subdomain', 'remotePort'], https: ['customDomains', 'subdomain'],"
  ),
  '未勾选仍写 loginFailExit': (s) => s.replace(
    "if (f.retry) out += 'loginFailExit = false\\n';",
    "out += 'loginFailExit = false\\n';"
  ),
  '不写 localPort': (s) => s.replace(
    'out += `localPort = ${localPort}\\n`;',
    ''
  ),
  '新建默认进 TOML 模式': (s) => s.replace(
    "{ editingName = null; openModal('新建实例', { mode: 'form' }); }",
    "{ editingName = null; openModal('新建实例'); }"
  ),
  '类型切换不再显隐字段': (s) => s.replace(
    "box.querySelector('.f-type').onchange = () => syncProxyFields(box);",
    ''
  ),
  '编辑时也显示模式切换': (s) => s.replace(
    "$('mode-seg').style.display = mode === 'form' ? '' : 'none';",
    "$('mode-seg').style.display = '';"
  ),
};

function run(script) {
  try {
    execFileSync(process.execPath, [path.join(__dirname, script)], {
      cwd: root, stdio: 'pipe', env: { ...process.env, JSDOM_PATH: jsdomPath },
    });
    return true;
  } catch (_) {
    return false;
  }
}

const results = [];
for (const [label, mutate] of Object.entries(mutations)) {
  const mutated = mutate(original);
  if (mutated === original) { results.push([label, 'NO-OP', '替换未生效']); continue; }
  fs.writeFileSync(htmlPath, mutated);
  const genPass = run('check-form.js');
  const uiPass = run('check-form-ui.js');
  fs.writeFileSync(htmlPath, original);
  const caught = !genPass || !uiPass;
  results.push([label, caught ? 'OK' : '!! 空转', `生成逻辑:${genPass ? '通过' : '捕获'} 交互:${uiPass ? '通过' : '捕获'}`]);
}

// 基线
fs.writeFileSync(htmlPath, original);
const baseGen = run('check-form.js');
const baseUi = run('check-form-ui.js');
results.push(['（基线）原代码', baseGen && baseUi ? 'OK' : '!! 空转', `生成逻辑:${baseGen ? '通过' : '捕获'} 交互:${baseUi ? '通过' : '捕获'}`]);

for (const [label, verdict, detail] of results) {
  console.log(`${verdict.padEnd(8)} ${label.padEnd(22)} ${detail}`);
}
const bad = results.filter(([, v]) => v !== 'OK');
if (bad.length) { console.error(`\n有 ${bad.length} 项异常`); process.exit(1); }
console.log('\nmutation-check: 所有变异体都被捕获，测试有效');
