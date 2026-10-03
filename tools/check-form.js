// 自测 index.html 里「表单 → TOML」的生成逻辑。
// 直接用 node 运行：node tools/check-form.js
// 生成逻辑带 ==== form-toml:start/end ==== 标记，这里抽出来在 node 里跑断言。
const fs = require('fs');
const path = require('path');
const assert = require('assert');

const html = fs.readFileSync(path.join(__dirname, '..', 'internal', 'server', 'static', 'index.html'), 'utf8');
const m = html.match(/\/\/ ==== form-toml:start ====([\s\S]*?)\/\/ ==== form-toml:end ====/);
assert(m, '未在 index.html 中找到 form-toml 标记段');

const { tomlFromForm, portNum } = new Function(m[1] + '\nreturn { tomlFromForm, portNum };')();

const base = { serverAddr: 'frps.example.com', serverPort: '7000', token: '', retry: true };
const web = { name: 'web', type: 'tcp', localIP: '127.0.0.1', localPort: '8080', remotePort: '6001' };

// 1. 基本 tcp 代理
assert.strictEqual(
  tomlFromForm({ ...base, proxies: [web] }),
  `serverAddr = "frps.example.com"\nserverPort = 7000\nloginFailExit = false\n` +
  `\n[[proxies]]\nname = "web"\ntype = "tcp"\nlocalIP = "127.0.0.1"\nlocalPort = 8080\nremotePort = 6001\n`
);

// 2. 取消勾选重试 -> 不出现 loginFailExit
const noRetry = tomlFromForm({ ...base, retry: false, proxies: [web] });
assert(!noRetry.includes('loginFailExit'), '未勾选重试却写入了 loginFailExit');

// 3. token 中的引号 / 反斜杠被转义
const esc = tomlFromForm({ ...base, token: 'a"b\\c', proxies: [web] });
assert(esc.includes('auth.token = "a\\"b\\\\c"'), 'token 未正确转义: ' + esc.split('\n')[2]);

// 4. http 类型：域名拆成数组，不写 remotePort
const http = { name: 'blog', type: 'http', localIP: '127.0.0.1', localPort: '80', customDomains: 'a.com, b.com', subdomain: 'x' };
const httpOut = tomlFromForm({ ...base, proxies: [http] });
assert(httpOut.includes('customDomains = ["a.com", "b.com"]'), '域名数组生成错误');
assert(httpOut.includes('subdomain = "x"'), '子域名缺失');
assert(!httpOut.includes('remotePort'), 'http 代理不应写 remotePort');

// 5. stcp 类型：写 secretKey
const stcp = { name: 'nas', type: 'stcp', localIP: '127.0.0.1', localPort: '5000', secretKey: 'sk' };
assert(tomlFromForm({ ...base, proxies: [stcp] }).includes('secretKey = "sk"'));

// 6. 非 tcp 类型忽略残留的 remotePort 输入
assert(!tomlFromForm({ ...base, proxies: [{ ...stcp, remotePort: '9999' }] }).includes('remotePort'));

// 7. 非法输入必须被拒
const bad = [
  [{ ...base, serverAddr: '  ', proxies: [web] }, '服务端地址'],
  [{ ...base, serverPort: '0', proxies: [web] }, '服务端端口'],
  [{ ...base, serverPort: '70000', proxies: [web] }, '服务端端口'],
  [{ ...base, serverPort: 'abc', proxies: [web] }, '服务端端口'],
  [{ ...base, proxies: [] }, '代理'],
  [{ ...base, proxies: [{ ...web, name: '' }] }, '名称'],
  [{ ...base, proxies: [{ ...web, localPort: '0' }] }, '本地端口'],
  [{ ...base, proxies: [{ ...web, remotePort: '' }] }, '远程端口'],
  [{ ...base, proxies: [web, { ...web, name: 'web' }] }, '重复'],
];
for (const [input, label] of bad) {
  assert.throws(() => tomlFromForm(input), (e) => e.message.includes(label), `非法输入未被拒: ${JSON.stringify(input)}`);
}

// 8. 多个代理顺序稳定
const two = tomlFromForm({ ...base, proxies: [web, { ...web, name: 'ssh', localPort: '22', remotePort: '6022' }] });
assert.strictEqual(two.match(/\[\[proxies\]\]/g).length, 2);

// 9. 端口边界
assert.strictEqual(portNum('1'), 1);
assert.strictEqual(portNum('65535'), 65535);
assert.strictEqual(portNum('65536'), 0);
assert.strictEqual(portNum('1.5'), 0);
assert.strictEqual(portNum('-1'), 0);

console.log('check-form: 全部通过');
