// WASM 控制台的浏览器冒烟：真打开页面、真跑 WASM、真等 REST 数据。
//
// 为什么不用 curl 代替：curl 只能证明「静态文件发得出去」。
// 裁剪后的 WASM 最容易出问题的地方恰恰在运行时——源生成序列化没覆盖到的类型、
// 启动脚本与 index.html 的结构约定、同源 REST 地址解析。这些只有真浏览器能暴露。
//
// 用法：NODE_PATH="$(npm root -g)" node scripts/wasm-smoke.cjs <url> [screenshot.png]
// 退出码：0 通过，1 失败，2 环境不具备（未安装 playwright）。
'use strict';

const path = require('path');

async function main() {
  const url = process.argv[2];
  const shot = process.argv[3];
  if (!url) {
    console.error('用法: node wasm-smoke.cjs <url> [screenshot.png]');
    return 1;
  }

  let chromium;
  try {
    ({ chromium } = require('playwright'));
  } catch {
    console.log('  skip 未找到 playwright（npm i -D playwright 或全局安装后设 NODE_PATH）');
    return 2;
  }

  const browser = await chromium.launch({
    headless: true,
    // 容器里没有 user namespace 时 chromium 起不来；这只是本机验收，不涉及权限边界。
    args: ['--no-sandbox'],
  });

  const page = await browser.newPage();

  const pageErrors = [];
  const consoleErrors = [];
  const failedRequests = [];
  const badResponses = [];

  page.on('pageerror', (err) => pageErrors.push(err.message));
  page.on('console', (msg) => {
    if (msg.type() === 'error') consoleErrors.push(msg.text());
  });
  page.on('requestfailed', (req) => {
    // favicon 不存在不影响控制台，浏览器会自动放弃。
    if (!req.url().includes('favicon')) failedRequests.push(`${req.url()} ${req.failure()?.errorText ?? ''}`);
  });
  page.on('response', (res) => {
    if (res.status() >= 400 && !res.url().includes('favicon')) {
      badResponses.push(`${res.status()} ${res.url()}`);
    }
  });

  const started = Date.now();
  await page.goto(url, { waitUntil: 'domcontentloaded', timeout: 60000 });

  // 判据不是「页面有 HTML」，而是「REST 数据已经渲染出来」：
  // 它同时要求 WASM 启动、裁剪后的序列化可用、同源地址解析正确。
  await page.waitForFunction(
    () => document.body && document.body.innerText.includes('虚拟设备 1'),
    null,
    { timeout: 120000 },
  );

  const bootMs = Date.now() - started;

  const state = await page.evaluate(() => ({
    loader: document.querySelector('.uno-loader') !== null,
    text: document.body.innerText,
    canvases: document.querySelectorAll('canvas').length,
  }));

  const checks = [];
  const ok = (cond, what) => checks.push({ cond: !!cond, what });

  ok(!state.loader, '启动遮罩在应用挂载后被移除');
  ok(state.text.includes('设备') && state.text.includes('点位') && state.text.includes('监视')
    && state.text.includes('日志') && state.text.includes('配置'), '五个导航项都渲染出来');
  ok(/设备\s+\d+\/\d+\s+采集/.test(state.text), '健康状态行显示采集进度（REST /api/v1/health 已生效）');
  ok(pageErrors.length === 0, `没有未捕获的页面异常${pageErrors.length ? '：' + pageErrors[0] : ''}`);
  ok(failedRequests.length === 0, `没有失败的请求${failedRequests.length ? '：' + failedRequests[0] : ''}`);
  ok(badResponses.length === 0, `没有 4xx/5xx 响应${badResponses.length ? '：' + badResponses[0] : ''}`);

  if (shot) {
    await page.screenshot({ path: shot, fullPage: true });
    console.log(`  截图 ${path.resolve(shot)}`);
  }

  console.log(`  WASM 启动 + 首屏数据 ${bootMs} ms`);

  let failed = 0;
  for (const c of checks) {
    if (c.cond) {
      console.log(`  \x1b[32m✓\x1b[0m ${c.what}`);
    } else {
      console.log(`  \x1b[31m✗\x1b[0m ${c.what}`);
      failed++;
    }
  }

  if (consoleErrors.length > 0) {
    console.log(`  \x1b[33m!\x1b[0m 控制台有 ${consoleErrors.length} 条 error 级日志（不计入失败，供排查）：`);
    for (const line of consoleErrors.slice(0, 5)) console.log(`     ${line}`);
  }

  await browser.close();
  return failed === 0 ? 0 : 1;
}

main()
  .then((code) => process.exit(code))
  .catch((err) => {
    console.error(`\x1b[31m✗\x1b[0m 浏览器冒烟失败：${err.message}`);
    process.exit(1);
  });
