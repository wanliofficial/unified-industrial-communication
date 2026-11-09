// WASM 控制台的浏览器冒烟：真打开页面、真跑 WASM、真等 REST 数据，
// 再在设备页里真的建一台设备（校验 → 应用 → 列表刷新）。
//
// 为什么不用 curl 代替：curl 只能证明「静态文件发得出去」。
// 裁剪后的 WASM 最容易出问题的地方恰恰在运行时——源生成序列化没覆盖到的类型、
// 启动脚本与 index.html 的结构约定、同源 REST 地址解析。这些只有真浏览器能暴露。
//
// 用法：NODE_PATH="$(npm root -g)" node scripts/wasm-smoke.cjs <url> [screenshot.png]
//       UIC_SMOKE_BROWSER=firefox ...             # 换浏览器（默认 chromium）
// 退出码：0 通过，1 失败，2 环境不具备（未安装 playwright 或该浏览器）。
'use strict';

const path = require('path');

async function main() {
  const url = process.argv[2];
  const shot = process.argv[3];
  if (!url) {
    console.error('用法: node wasm-smoke.cjs <url> [screenshot.png]');
    return 1;
  }

  // Uno WASM 的问题常常是引擎相关的（WASM 启动脚本、Blazor 引导、渲染层差异），
  // 只验一个浏览器等于只验一种命。默认 Chromium，可用环境变量换 Firefox。
  const name = process.env.UIC_SMOKE_BROWSER || 'chromium';

  let playwright;
  let engine;
  try {
    playwright = require('playwright');
    engine = playwright[name];
  } catch {
    console.log('  skip 未找到 playwright（npm i -D playwright 或全局安装后设 NODE_PATH）');
    return 2;
  }

  if (!engine) {
    console.log(`  skip playwright 没有名为 ${name} 的浏览器（可用：chromium / firefox / webkit）`);
    return 2;
  }

  const browser = await engine.launch({
    headless: true,
    // 容器里没有 user namespace 时 chromium 起不来；这只是本机验收，不涉及权限边界。
    // Firefox 不认识这个参数，只对 chromium 传。
    ...(name === 'chromium' ? { args: ['--no-sandbox'] } : {}),
  });

  // 一次完整尝试：打开页面、等 WASM 把 REST 数据渲染出来、收集全程的异常与失败请求。
  // 单独成函数是为了在「网络接口变化」这种环境噪声上做一次重试，见下面的 transient。
  const attempt = async () => {
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

    // 账号体系默认开启：先登录，并走完「首次强制改密」，才可能看到业务数据。
    // 这一段同时验证了登录表单、表单校验与改密后的令牌换发。
    await signIn(page);

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

    // 第二层：真的在控制台里建一台设备。
    // 首屏只证明「读得到」；表单证明「写得进」——它跨过 validate、apply、配置版本、
    // 设备重启与列表刷新，是首开流程里唯一无法用 curl 代替的部分。
    const form = await exerciseDeviceForm(page);

    return { page, pageErrors, consoleErrors, failedRequests, badResponses, bootMs, state, form };
  };

  // 本机同时挂着 tailscale / docker / VPN 时，Chromium 会收到网络接口变化事件，
  // 正在下载的框架 .wasm 会被中止成 net::ERR_NETWORK_CHANGED。应用本身已经起来了，
  // 这是环境噪声：只对这一种错误重试一次，其它失败照常判负。
  const transient = (r) =>
    r.failedRequests.some((line) => line.includes('ERR_NETWORK_CHANGED')) ||
    r.pageErrors.some((line) => /Failed to fetch/i.test(line));

  let run = await attempt();
  if (transient(run)) {
    console.log('  ! 浏览器报告网络接口变化，框架资源被中断；重试一次');
    await run.page.close();
    run = await attempt();
  }

  const { page, pageErrors, consoleErrors, failedRequests, badResponses, bootMs, state, form } = run;

  const checks = [];
  const ok = (cond, what) => checks.push({ cond: !!cond, what });

  ok(!state.loader, '启动遮罩在应用挂载后被移除');
  ok(state.text.includes('设备') && state.text.includes('点位') && state.text.includes('监视')
    && state.text.includes('日志') && state.text.includes('配置') && state.text.includes('审计')
    && state.text.includes('账号'), '管理员能看到全部八个导航项');
  ok(state.text.includes('admin') && !state.text.includes('登录网关'),
    '登录后顶栏显示当前账号，登录遮罩已移除');
  ok(/设备\s+\d+\/\d+\s+采集/.test(state.text), '健康状态行显示采集进度（REST /api/v1/health 已生效）');
  ok(form.ok, `设备页的新建设备表单走完校验与应用${form.ok ? `（${form.deviceId}）` : '：' + form.message}`);
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

/**
 * 用首次启动生成的初始口令登录，并完成强制改密。
 *
 * 不依赖控件类型：Uno 的 TextBox 渲染成 <textarea>，PasswordBox 可能是 <input>。
 * 只按「当前可见的输入控件顺序」取，登录表单是 用户名、口令；改密表单是
 * 当前口令（已预填）、新口令、确认。
 */
async function signIn(page) {
  const initial = process.env.UIC_SMOKE_PASSWORD;
  if (!initial) throw new Error('缺少 UIC_SMOKE_PASSWORD：冒烟需要首次启动生成的初始口令');

  await page.waitForFunction(
    () => document.body.innerText.includes('登录网关'),
    null,
    { timeout: 60000 },
  );

  const loginFields = page.locator('textarea:visible, input:visible');
  await loginFields.nth(0).fill('admin');
  await loginFields.nth(1).fill(initial);

  await page.getByText('登录', { exact: true }).first().click();

  await page.waitForFunction(
    () => document.body.innerText.includes('必须修改口令'),
    null,
    { timeout: 60000 },
  );

  const next = 'smoke-' + Math.random().toString(36).slice(2, 10);
  const passwordFields = page.locator('textarea:visible, input:visible');
  await passwordFields.nth(1).fill(next);
  await passwordFields.nth(2).fill(next);

  await page.getByText('保存并进入', { exact: true }).first().click();

  await page.waitForFunction(
    () => !document.body.innerText.includes('必须修改口令'),
    null,
    { timeout: 60000 },
  );

  return next;
}

/**
 * 在设备页里真的建一台设备。
 *
 * Uno 把 TextBox 渲染成 <textarea>，`PlaceholderText` 是画上去的文字而不是 DOM 属性，
 * 所以只能按 DOM 顺序取框：0 是列表上方的搜索框，1 起是表单里当前可见的字段
 * （默认「＋ 新建通道 + modbus-tcp」，即 通道 ID、主机、端口、设备 ID、名称、
 * 采集周期、从站地址、点位 ID、点位名称、偏移）。串口字段在 tcp 协议下不渲染，
 * 正好不在序号里 —— 换协议会改变序号，所以这里固定用默认协议。
 *
 * 主机填 127.0.0.1:502：本机没有从站，设备起来后会是 faulted。这条检查验的是
 * 「写得进」（validate → apply → 版本 → 列表刷新），不是「连得上」。
 */
async function exerciseDeviceForm(page) {
  const suffix = Date.now().toString(36);
  const deviceId = `console-${suffix}`;
  const deviceName = `控制台建的设备 ${suffix}`;

  try {
    await page.getByText('新建设备', { exact: true }).first().click();

    await page.waitForFunction(
      () => document.body.innerText.includes('首个点位 ID'),
      null,
      { timeout: 30000 },
    );

    // 展开表单时会异步拉一次 /api/v1/config 来填通道下拉；重建下拉会打乱手上的引用，
    // 给它一点时间稳定下来再开始填。
    await page.waitForTimeout(500);

    const fill = async (index, value) => {
      const box = page.locator('textarea').nth(index);
      await box.click();
      await box.fill(value);

      // 写错框时宁可在这里失败：后面的 422 只会说「字段非法」，看不出是填错了位置。
      const actual = await box.inputValue();
      if (actual !== value) throw new Error(`第 ${index} 个输入框写入失败（得到 "${actual}"）`);
    };

    await fill(1, `console-bus-${suffix}`);
    await fill(2, '127.0.0.1');
    await fill(4, deviceId);
    await fill(5, deviceName);
    await fill(8, 'value');
    await fill(9, '值');
    await fill(10, '0');

    await page.getByText('创建', { exact: true }).first().click();

    await page.waitForFunction(
      (id) => document.body.innerText.includes(`已创建设备 ${id}`),
      deviceId,
      { timeout: 60000 },
    );

    // 创建成功后表单会清空设备字段、再刷一次列表。名字只可能出现在列表行上，
    // 等它出现就等于等到了那次刷新，避免与 RefreshAsync 抢跑。
    await page.waitForFunction(
      (name) => document.body.innerText.includes(name),
      deviceName,
      { timeout: 60000 },
    );

    return { ok: true, deviceId, message: '' };
  } catch (err) {
    return { ok: false, deviceId, message: err.message };
  }
}

main()
  .then((code) => process.exit(code))
  .catch((err) => {
    console.error(`\x1b[31m✗\x1b[0m 浏览器冒烟失败：${err.message}`);
    process.exit(1);
  });
