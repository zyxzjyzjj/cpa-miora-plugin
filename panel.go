package main

import "strings"

// panelHTML returns the browser dashboard served at
// /v0/resource/plugins/miora-provider/panel.
//
// Resource routes are NOT admin-authenticated, so this page holds no secrets of
// its own: it reads the admin key from the management UI's localStorage (the
// documented same-origin convention) and calls the authenticated
// /v0/management/... routes. Scripts are inlined rather than loaded from a CDN so
// the admin page never pulls third-party code into an admin context.
func panelHTML() string {
	return strings.ReplaceAll(panelTemplate, "__PROVIDER__", providerID)
}

const panelTemplate = `<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="color-scheme" content="dark light">
<title>Miora · CPA 控制台</title>
<style>
  :root {
    --ink: #07080c;
    --ink-2: #0b0d13;
    --card: rgba(255,255,255,.045);
    --card-solid: #12141c;
    --stroke: rgba(255,255,255,.09);
    --stroke-strong: rgba(255,255,255,.18);
    --text: #ebedf5;
    --muted: #8b93a8;
    --dim: #616a80;
    --brand: #7c5cff;
    --brand-2: #ff5fa2;
    --brand-3: #ffb45c;
    --ok: #3ddc97;
    --warn: #ffb45c;
    --err: #ff6b6b;
    --radius: 16px;
    --shadow: 0 18px 50px -20px rgba(0,0,0,.8);
    --mono: ui-monospace, SFMono-Regular, "SF Mono", Menlo, Consolas, "Liberation Mono", monospace;
    --sans: -apple-system, BlinkMacSystemFont, "Segoe UI", "PingFang SC", "Hiragino Sans GB", "Microsoft YaHei", Roboto, sans-serif;
  }
  @media (prefers-color-scheme: light) {
    :root {
      --ink: #efedf7; --ink-2: #ffffff; --card: rgba(255,255,255,.92); --card-solid: #fff;
      --stroke: rgba(28,22,60,.12); --stroke-strong: rgba(28,22,60,.24);
      --text: #151425; --muted: #55506e; --dim: #746e8a;
      --shadow: 0 20px 46px -26px rgba(38,26,86,.45);
    }
    body {
      background:
        radial-gradient(1100px 620px at 8% -10%, rgba(124,92,255,.30), transparent 62%),
        radial-gradient(900px 520px at 96% 0%, rgba(255,95,162,.24), transparent 60%),
        radial-gradient(720px 460px at 60% 108%, rgba(255,180,92,.20), transparent 62%),
        var(--ink);
    }
  }
  * { box-sizing: border-box; }
  html, body { height: 100%; }
  body {
    margin: 0; color: var(--text); font: 14px/1.62 var(--sans);
    background:
      radial-gradient(1100px 620px at 8% -10%, rgba(124,92,255,.28), transparent 60%),
      radial-gradient(900px 520px at 96% 0%, rgba(255,95,162,.20), transparent 58%),
      radial-gradient(720px 460px at 60% 108%, rgba(255,180,92,.13), transparent 60%),
      var(--ink);
    min-height: 100vh;
  }
  a { color: inherit; }
  button, input, select, textarea { font: inherit; color: inherit; }
  ::selection { background: rgba(124,92,255,.4); }

  .shell { display: grid; grid-template-columns: 232px minmax(0, 1fr); min-height: 100vh; }
  .rail {
    position: sticky; top: 0; align-self: start; height: 100vh; padding: 22px 16px;
    display: flex; flex-direction: column; gap: 18px;
    border-right: 1px solid var(--stroke);
    background: linear-gradient(180deg, rgba(255,255,255,.04), transparent 55%);
    backdrop-filter: blur(14px);
  }
  .brand { display: flex; align-items: center; gap: 11px; padding: 2px 6px 6px; }
  .mark {
    width: 34px; height: 34px; border-radius: 11px; flex: none;
    background: conic-gradient(from 160deg, var(--brand), var(--brand-2), var(--brand-3), var(--brand));
    box-shadow: 0 8px 22px -8px rgba(124,92,255,.9), inset 0 1px 0 rgba(255,255,255,.45);
    position: relative;
  }
  .mark::after {
    content: ""; position: absolute; inset: 8px; border-radius: 6px;
    background: rgba(7,8,12,.72); backdrop-filter: blur(2px);
  }
  .brand h1 { font-size: 15px; margin: 0; letter-spacing: .2px; font-weight: 650; }
  .brand p { margin: 0; font-size: 11.5px; color: var(--dim); letter-spacing: .3px; }

  nav { display: flex; flex-direction: column; gap: 3px; }
  .nav-item {
    display: flex; align-items: center; gap: 10px; padding: 9px 11px; border-radius: 11px;
    color: var(--muted); cursor: pointer; border: 1px solid transparent; background: none;
    text-align: left; width: 100%; font-size: 13.5px; transition: .16s ease;
  }
  .nav-item:hover { color: var(--text); background: rgba(255,255,255,.05); }
  .nav-item[aria-current="true"] {
    color: var(--text); background: linear-gradient(100deg, rgba(124,92,255,.26), rgba(255,95,162,.12));
    border-color: rgba(124,92,255,.35); box-shadow: inset 0 1px 0 rgba(255,255,255,.1);
  }
  .nav-item .ico { width: 17px; height: 17px; flex: none; opacity: .92; }
  .nav-item .count { margin-left: auto; font: 11px var(--mono); color: var(--dim); }
  .rail-foot { margin-top: auto; display: grid; gap: 8px; font-size: 11.5px; color: var(--dim); }
  .dot { width: 7px; height: 7px; border-radius: 50%; background: var(--ok); display: inline-block; margin-right: 6px; box-shadow: 0 0 0 4px rgba(61,220,151,.13); }
  .dot.off { background: var(--err); box-shadow: 0 0 0 4px rgba(255,107,107,.14); }

  main { padding: 26px clamp(18px, 3vw, 40px) 64px; min-width: 0; }
  .topbar { display: flex; align-items: flex-end; justify-content: space-between; gap: 16px; flex-wrap: wrap; margin-bottom: 22px; }
  .topbar h2 { margin: 0; font-size: clamp(20px, 2.2vw, 26px); letter-spacing: -.4px; font-weight: 650; }
  .topbar p { margin: 4px 0 0; color: var(--muted); font-size: 13px; }
  .actions { display: flex; gap: 8px; flex-wrap: wrap; align-items: center; }

  .btn {
    border: 1px solid var(--stroke); background: var(--card); color: var(--text);
    padding: 8px 13px; border-radius: 11px; cursor: pointer; display: inline-flex; align-items: center; gap: 7px;
    font-size: 13px; transition: .16s ease; white-space: nowrap;
  }
  .btn:hover { border-color: var(--stroke-strong); transform: translateY(-1px); }
  .btn:active { transform: none; }
  .btn:disabled { opacity: .5; cursor: not-allowed; transform: none; }
  .btn.primary {
    border-color: transparent; color: #fff;
    background: linear-gradient(120deg, var(--brand), var(--brand-2));
    box-shadow: 0 12px 26px -14px rgba(124,92,255,.95);
  }
  .btn.ghost { background: transparent; }
  .btn.danger:hover { border-color: rgba(255,107,107,.6); color: var(--err); }
  .btn.sm { padding: 5px 10px; font-size: 12px; border-radius: 9px; }

  .card {
    background: var(--card); border: 1px solid var(--stroke); border-radius: var(--radius);
    padding: 18px; box-shadow: var(--shadow); backdrop-filter: blur(12px);
  }
  .card h3 { margin: 0 0 14px; font-size: 13px; font-weight: 600; letter-spacing: .4px; text-transform: uppercase; color: var(--muted); }
  .card.flush { padding: 0; overflow: hidden; }

  .stats { display: grid; grid-template-columns: repeat(auto-fit, minmax(170px, 1fr)); gap: 12px; margin-bottom: 16px; }
  .stat { position: relative; overflow: hidden; padding: 16px 17px; }
  .stat .label { font-size: 12px; color: var(--muted); }
  .stat .value { font-size: 26px; font-weight: 650; letter-spacing: -.8px; margin-top: 6px; font-variant-numeric: tabular-nums; }
  .stat .hint { font-size: 11.5px; color: var(--dim); margin-top: 3px; }
  .stat::after {
    content: ""; position: absolute; right: -30px; top: -30px; width: 96px; height: 96px; border-radius: 50%;
    background: radial-gradient(circle, rgba(124,92,255,.35), transparent 70%);
  }
  .stat:nth-child(2)::after { background: radial-gradient(circle, rgba(255,95,162,.32), transparent 70%); }
  .stat:nth-child(3)::after { background: radial-gradient(circle, rgba(255,180,92,.3), transparent 70%); }
  .stat:nth-child(4)::after { background: radial-gradient(circle, rgba(61,220,151,.28), transparent 70%); }

  .grid-2 { display: grid; grid-template-columns: minmax(0, 1.35fr) minmax(0, 1fr); gap: 14px; }
  @media (max-width: 1040px) { .grid-2 { grid-template-columns: 1fr; } }
  .rows { display: grid; gap: 10px; }

  .row-line { display: flex; gap: 10px; align-items: center; justify-content: space-between; padding: 9px 0; border-bottom: 1px dashed var(--stroke); }
  .row-line:last-child { border-bottom: none; }
  .muted { color: var(--muted); }
  .dim { color: var(--dim); }
  .mono { font-family: var(--mono); font-size: 12px; }
  .tiny { font-size: 11.5px; }

  .pill { display: inline-flex; align-items: center; gap: 5px; font-size: 11.5px; padding: 2px 9px; border-radius: 999px; border: 1px solid var(--stroke); color: var(--muted); white-space: nowrap; }
  .pill.ok { color: var(--ok); border-color: rgba(61,220,151,.4); background: rgba(61,220,151,.08); }
  .pill.warn { color: var(--warn); border-color: rgba(255,180,92,.4); background: rgba(255,180,92,.08); }
  .pill.err { color: var(--err); border-color: rgba(255,107,107,.4); background: rgba(255,107,107,.08); }
  .pill.brand { color: #d9d1ff; border-color: rgba(124,92,255,.45); background: rgba(124,92,255,.14); }

  .field { display: grid; gap: 6px; }
  .field > label { font-size: 12px; color: var(--muted); }
  input[type=text], input[type=password], input[type=number], select, textarea {
    width: 100%; background: rgba(0,0,0,.30); border: 1px solid var(--stroke); border-radius: 11px;
    padding: 9px 11px; outline: none; transition: .15s ease;
  }
  input:focus, select:focus, textarea:focus { border-color: rgba(124,92,255,.65); box-shadow: 0 0 0 3px rgba(124,92,255,.16); }
  textarea { resize: vertical; min-height: 96px; line-height: 1.6; }
  .inline { display: grid; grid-template-columns: repeat(auto-fit, minmax(140px, 1fr)); gap: 10px; }

  .segmented { display: inline-flex; padding: 3px; gap: 3px; background: rgba(0,0,0,.25); border: 1px solid var(--stroke); border-radius: 12px; }
  .segmented button { border: none; background: none; color: var(--muted); padding: 6px 12px; border-radius: 9px; cursor: pointer; font-size: 12.5px; }
  .segmented button[aria-pressed="true"] { background: linear-gradient(120deg, rgba(124,92,255,.9), rgba(255,95,162,.75)); color: #fff; }

  .table { width: 100%; border-collapse: collapse; font-size: 13px; }
  .table th, .table td { text-align: left; padding: 9px 12px; border-bottom: 1px solid var(--stroke); vertical-align: middle; }
  .table th { font-size: 11.5px; text-transform: uppercase; letter-spacing: .5px; color: var(--dim); font-weight: 500; }
  .table tr:last-child td { border-bottom: none; }
  .table tbody tr:hover { background: rgba(255,255,255,.03); }
  .scroll { max-height: 460px; overflow: auto; }

  .task { display: grid; gap: 8px; padding: 12px 14px; border: 1px solid var(--stroke); border-radius: 13px; background: rgba(0,0,0,.16); }
  .task .head { display: flex; justify-content: space-between; gap: 10px; align-items: center; }
  .task .prompt { color: var(--muted); font-size: 12.5px; display: -webkit-box; -webkit-line-clamp: 2; -webkit-box-orient: vertical; overflow: hidden; }
  .bar { height: 5px; border-radius: 999px; background: rgba(255,255,255,.09); overflow: hidden; }
  .bar > i { display: block; height: 100%; width: 0%; border-radius: 999px; background: linear-gradient(90deg, var(--brand), var(--brand-2)); transition: width .45s ease; }
  .bar.idle > i { width: 35% !important; animation: slide 1.5s ease-in-out infinite; }
  @keyframes slide { 0% { margin-left: -35%; } 100% { margin-left: 100%; } }

  .gallery { display: grid; grid-template-columns: repeat(auto-fill, minmax(168px, 1fr)); gap: 12px; }
  .thumb {
    position: relative; border-radius: 13px; overflow: hidden; border: 1px solid var(--stroke);
    aspect-ratio: 1/1; background: rgba(0,0,0,.35); cursor: pointer;
  }
  .thumb img, .thumb video { width: 100%; height: 100%; object-fit: cover; display: block; }
  .thumb .cap { position: absolute; inset: auto 0 0 0; padding: 22px 9px 8px; font-size: 11px; color: #fff;
    background: linear-gradient(to top, rgba(0,0,0,.85), transparent); display: flex; justify-content: space-between; gap: 6px; }
  .thumb .kind { position: absolute; top: 8px; left: 8px; }

  .empty { display: grid; place-items: center; text-align: center; gap: 8px; padding: 34px 16px; color: var(--muted); }
  .empty .big { font-size: 30px; opacity: .5; }
  .empty ol { text-align: left; margin: 6px 0 0; padding-left: 18px; font-size: 12.5px; color: var(--dim); }

  .toast-wrap { position: fixed; right: 18px; bottom: 18px; display: grid; gap: 8px; z-index: 60; }
  .toast {
    padding: 10px 14px; border-radius: 12px; border: 1px solid var(--stroke-strong);
    background: rgba(14,16,22,.94); color: #f2f4fa; box-shadow: var(--shadow); max-width: 380px; font-size: 13px;
    animation: pop .18s ease;
  }
  .toast.err { border-color: rgba(255,107,107,.55); }
  .toast.ok { border-color: rgba(61,220,151,.5); }
  @keyframes pop { from { opacity: 0; transform: translateY(8px); } }

  .skeleton { border-radius: 10px; background: linear-gradient(90deg, rgba(255,255,255,.06), rgba(255,255,255,.12), rgba(255,255,255,.06)); background-size: 200% 100%; animation: shimmer 1.3s infinite; height: 14px; }
  @keyframes shimmer { from { background-position: 200% 0; } to { background-position: -200% 0; } }

  .lightbox { position: fixed; inset: 0; background: rgba(4,5,8,.86); backdrop-filter: blur(8px); display: grid; place-items: center; z-index: 70; padding: 24px; }
  .lightbox .inner { max-width: min(960px, 92vw); max-height: 88vh; display: grid; gap: 12px; }
  .lightbox img, .lightbox video { max-width: 100%; max-height: 70vh; border-radius: 14px; }
  .lightbox .meta { color: var(--muted); font-size: 12.5px; display: flex; gap: 10px; flex-wrap: wrap; align-items: center; }

  .kv { display: grid; grid-template-columns: 132px minmax(0,1fr); gap: 6px 12px; font-size: 13px; }
  .kv dt { color: var(--dim); }
  .kv dd { margin: 0; word-break: break-all; }

  .cap-chips { display: flex; flex-wrap: wrap; gap: 6px; }
  .chip { font-size: 11.5px; padding: 3px 9px; border-radius: 8px; background: rgba(124,92,255,.12); border: 1px solid rgba(124,92,255,.28); color: #cfc7ff; }
  @media (prefers-color-scheme: light) { .chip { color: #4a3ec4; } }

  .hint-box { font-size: 12.5px; color: var(--muted); border-left: 2px solid rgba(124,92,255,.5); padding: 2px 0 2px 10px; }
  .split { display: flex; gap: 8px; flex-wrap: wrap; align-items: center; }
  .right { margin-left: auto; }
  .hidden { display: none !important; }
  @media (max-width: 860px) {
    .shell { grid-template-columns: 1fr; }
    .rail { position: static; height: auto; flex-direction: row; align-items: center; overflow-x: auto; gap: 8px; padding: 12px; border-right: none; border-bottom: 1px solid var(--stroke); }
    .rail .brand { padding: 0 8px 0 0; }
    .rail .brand p, .rail-foot { display: none; }
    nav { flex-direction: row; }
    .nav-item { white-space: nowrap; }
    .nav-item .count { display: none; }
  }
  @media (prefers-reduced-motion: reduce) {
    * { animation: none !important; transition: none !important; }
  }
  /* Light-theme component tuning. Kept at the end of the sheet so it wins over
     the base rules above instead of being shadowed by them. */
  @media (prefers-color-scheme: light) {
    input[type=text], input[type=password], input[type=number], select, textarea {
      background: #fff; border-color: rgba(28,22,60,.20); color: var(--text);
    }
    input::placeholder, textarea::placeholder { color: rgba(84,78,110,.62); }
    .btn { background: rgba(255,255,255,.94); }
    .btn:hover { background: #fff; }
    .btn.primary { background: linear-gradient(120deg, var(--brand), var(--brand-2)); color: #fff; }
    .segmented { background: rgba(28,22,60,.07); }
    .segmented button[aria-pressed="true"] { color: #fff; }
    .task, .stat { background: rgba(255,255,255,.9); }
    .nav-item:hover { background: rgba(28,22,60,.06); }
    .table tbody tr:hover { background: rgba(28,22,60,.04); }
    .thumb { background: rgba(28,22,60,.08); }
    .toast { background: rgba(255,255,255,.98); color: var(--text); }
    .lightbox { background: rgba(238,236,248,.92); }
    .hint-box { border-left-color: rgba(124,92,255,.7); }
    .mark::after { background: rgba(255,255,255,.86); }
  }
</style>
</head>
<body>
<div class="shell">
  <aside class="rail">
    <div class="brand">
      <div class="mark" aria-hidden="true"></div>
      <div>
        <h1>Miora</h1>
        <p>CLIProxyAPI 插件</p>
      </div>
    </div>
    <nav id="nav" aria-label="主导航">
      <button class="nav-item" data-view="overview" aria-current="true">
        <svg class="ico" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7"><rect x="3" y="3" width="7" height="9" rx="2"/><rect x="14" y="3" width="7" height="5" rx="2"/><rect x="14" y="12" width="7" height="9" rx="2"/><rect x="3" y="16" width="7" height="5" rx="2"/></svg>
        概览
      </button>
      <button class="nav-item" data-view="accounts">
        <svg class="ico" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7"><circle cx="12" cy="8" r="3.6"/><path d="M4.5 20c1.4-3.5 4.2-5.2 7.5-5.2s6.1 1.7 7.5 5.2"/></svg>
        账号<span class="count" id="navAccounts"></span>
      </button>
      <button class="nav-item" data-view="studio">
        <svg class="ico" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7"><path d="M12 3l2.2 5.3L20 10l-5.8 1.7L12 17l-2.2-5.3L4 10l5.8-1.7z"/><path d="M18 16.5l.9 2.1 2.1.9-2.1.9-.9 2.1-.9-2.1-2.1-.9 2.1-.9z"/></svg>
        生成台
      </button>
      <button class="nav-item" data-view="library">
        <svg class="ico" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7"><rect x="3.5" y="4.5" width="17" height="15" rx="3"/><circle cx="9" cy="10" r="1.6"/><path d="M4 17l4.6-4.2c.8-.7 2-.7 2.8 0L20 19"/></svg>
        产物
      </button>
      <button class="nav-item" data-view="models">
        <svg class="ico" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7"><path d="M12 3l8 4.5v9L12 21l-8-4.5v-9z"/><path d="M12 12l8-4.5M12 12v9M12 12L4 7.5"/></svg>
        模型
      </button>
      <button class="nav-item" data-view="logs">
        <svg class="ico" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7"><path d="M6 3.5h9l4 4V20a1 1 0 0 1-1 1H6a1 1 0 0 1-1-1V8"/><path d="M8 12h8M8 16h6"/></svg>
        日志
      </button>
      <button class="nav-item" data-view="settings">
        <svg class="ico" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7"><circle cx="12" cy="12" r="3.2"/><path d="M19.4 15a1.7 1.7 0 0 0 .3 1.9l.1.1a2 2 0 1 1-2.8 2.8l-.1-.1a1.7 1.7 0 0 0-2.9 1.2 2 2 0 1 1-4 0 1.7 1.7 0 0 0-2.9-1.2l-.1.1a2 2 0 1 1-2.8-2.8l.1-.1A1.7 1.7 0 0 0 3 15a2 2 0 1 1 0-4 1.7 1.7 0 0 0 1.4-2.9l-.1-.1a2 2 0 1 1 2.8-2.8l.1.1A1.7 1.7 0 0 0 10 4a2 2 0 1 1 4 0 1.7 1.7 0 0 0 2.9 1.3l.1-.1a2 2 0 1 1 2.8 2.8l-.1.1A1.7 1.7 0 0 0 21 11a2 2 0 1 1 0 4z"/></svg>
        设置
      </button>
    </nav>
    <div class="rail-foot">
      <div><span class="dot" id="healthDot"></span><span id="healthText">连接中…</span></div>
      <div class="mono" id="versionText">__PROVIDER__</div>
    </div>
  </aside>

  <main>
    <div class="topbar">
      <div>
        <h2 id="viewTitle">概览</h2>
        <p id="viewSub">Miora 的图像 / 视频 / 3D 能力，通过 CLIProxyAPI 反代为 OpenAI 兼容接口。</p>
      </div>
      <div class="actions">
        <div class="field" style="min-width:250px">
          <input id="managementKey" type="password" autocomplete="off" placeholder="CPA 管理密钥（可自动读取）" aria-label="CPA 管理密钥">
        </div>
        <button class="btn" id="btnRefresh">刷新</button>
      </div>
    </div>

    <div id="msg" aria-live="polite" class="hidden"></div>

    <!-- 概览 -->
    <section id="view-overview">
      <div class="stats" id="statTiles">
        <div class="card stat"><div class="label">账号</div><div class="value" id="statAccounts">–</div><div class="hint">已导入的 Miora 登录态</div></div>
        <div class="card stat"><div class="label">模型</div><div class="value" id="statModels">–</div><div class="hint" id="statModelsHint">上游目录</div></div>
        <div class="card stat"><div class="label">可用额度</div><div class="value" id="statCredits">–</div><div class="hint">credits · 免费档常显示 0</div></div>
        <div class="card stat"><div class="label">近期任务</div><div class="value" id="statTasks">–</div><div class="hint" id="statTasksHint">本进程内</div></div>
      </div>
      <div class="grid-2">
        <div class="card">
          <h3>最近生成</h3>
          <div class="rows" id="recentTasks"><div class="skeleton" style="width:60%"></div><div class="skeleton" style="width:80%"></div><div class="skeleton" style="width:45%"></div></div>
        </div>
        <div class="card">
          <h3>插件能力</h3>
          <div class="cap-chips" id="capChips"><div class="skeleton" style="width:90%"></div></div>
          <div class="hint-box" style="margin-top:14px">
            客户端把模型名填成 <span class="mono" id="autoModelHint">miora-auto</span> 即可自动选择模型；
            也可直接用上面的模型 ID，例如 <span class="mono">gem-3.1</span>。
          </div>
          <div class="actions" style="margin-top:14px">
            <button class="btn primary" data-goto="studio">去生成</button>
            <button class="btn" data-goto="accounts">导入登录态</button>
          </div>
        </div>
      </div>
    </section>

    <!-- 账号 -->
    <section id="view-accounts" class="hidden">
      <div class="card" style="margin-bottom:14px">
        <h3>从浏览器一键导入</h3>
        <p class="muted tiny" style="margin:0 0 12px">
          只读本机 Edge / Chrome 的 <span class="mono">Local Storage</span>，提取已登录的 miora.design 会话并验证；不接触密码与 cookie。
        </p>
        <div class="split">
          <button class="btn primary" id="btnScan">扫描本机浏览器</button>
          <span class="dim tiny" id="scanHint">先在浏览器里登录 miora.design，再点扫描。</span>
        </div>
        <div class="rows" id="scanResults" style="margin-top:14px"></div>
      </div>

      <div class="card" style="margin-bottom:14px">
        <h3>手动粘贴</h3>
        <textarea id="pasteToken" placeholder='支持三种格式：&#10;1) 浏览器 localStorage 导出 JSON（authToken / refreshToken）&#10;2) {"auth_token":"eyJ...","refresh_token":"def50200..."}&#10;3) 只粘 eyJ... 这一段 access token'></textarea>
        <div class="split" style="margin-top:10px">
          <button class="btn primary" id="btnPaste">导入</button>
          <span class="dim tiny">DevTools → Application → Local Storage → https://miora.design</span>
        </div>
      </div>

      <div class="card">
        <div class="split" style="margin-bottom:12px">
          <h3 style="margin:0">账号</h3>
          <button class="btn sm right" id="btnLiveQuota">拉取实时额度</button>
        </div>
        <div class="rows" id="accountList"><div class="skeleton" style="width:70%"></div><div class="skeleton" style="width:50%"></div></div>
      </div>
    </section>

    <!-- 生成台 -->
    <section id="view-studio" class="hidden">
      <div class="grid-2">
        <div class="card">
          <h3>提示词</h3>
          <div class="split" style="margin-bottom:10px">
            <div class="segmented" id="mediaTabs">
              <button data-media="image" aria-pressed="true">图像</button>
              <button data-media="video" aria-pressed="false">视频</button>
              <button data-media="three_d" aria-pressed="false">3D</button>
            </div>
            <span class="dim tiny right" id="promptCount">0 字</span>
          </div>
          <textarea id="promptInput" placeholder="描述你想生成的内容…（Ctrl / ⌘ + Enter 直接生成）"></textarea>
          <div class="inline" style="margin-top:12px">
            <div class="field"><label for="modelSelect">模型</label><select id="modelSelect"></select></div>
            <div class="field"><label for="ratioSelect">画面比例</label><select id="ratioSelect"></select></div>
            <div class="field"><label for="resSelect">分辨率</label><select id="resSelect"></select></div>
            <div class="field" id="durationField" style="display:none"><label for="durationInput">时长（秒）</label><input id="durationInput" type="number" min="1" max="30" value="5"></div>
          </div>
          <div class="split" style="margin-top:14px">
            <button class="btn primary" id="btnGenerate">生成</button>
            <span class="dim tiny" id="genHint">提交后可在「产物」看到结果</span>
          </div>
        </div>
        <div class="card">
          <h3>任务</h3>
          <div class="rows" id="studioTasks"><div class="empty"><div class="big">◍</div><div>还没有任务</div></div></div>
        </div>
      </div>
    </section>

    <!-- 产物 -->
    <section id="view-library" class="hidden">
      <div class="card">
        <div class="split" style="margin-bottom:14px">
          <h3 style="margin:0">产物</h3>
          <span class="dim tiny right" id="libraryHint">签名链接 1 小时有效，过期后点「重签」即可，不会重复扣费。</span>
        </div>
        <div class="gallery" id="gallery"><div class="skeleton" style="aspect-ratio:1/1"></div></div>
      </div>
    </section>

    <!-- 模型 -->
    <section id="view-models" class="hidden">
      <div class="card flush">
        <div class="split" style="padding:16px 18px 12px">
          <h3 style="margin:0">模型目录</h3>
          <input id="modelFilter" type="text" placeholder="过滤模型…" style="max-width:220px" class="right">
          <button class="btn sm" id="btnReloadModels">重新拉取</button>
        </div>
        <div class="scroll">
          <table class="table">
            <thead><tr><th>模型</th><th>类型</th><th>比例</th><th>分辨率</th><th>参考图</th><th>状态</th></tr></thead>
            <tbody id="modelRows"><tr><td colspan="6"><div class="skeleton" style="width:60%"></div></td></tr></tbody>
          </table>
        </div>
      </div>
    </section>

    <!-- 日志 -->
    <section id="view-logs" class="hidden">
      <div class="card flush">
        <div class="split" style="padding:16px 18px 12px">
          <h3 style="margin:0">请求日志</h3>
          <button class="btn sm right" id="btnReloadLogs">刷新</button>
        </div>
        <div class="scroll">
          <table class="table">
            <thead><tr><th>时间</th><th>账号</th><th>模型</th><th>状态</th><th>额度</th><th>耗时</th><th>提示词</th></tr></thead>
            <tbody id="logRows"><tr><td colspan="7"><div class="skeleton" style="width:50%"></div></td></tr></tbody>
          </table>
        </div>
      </div>
    </section>

    <!-- 设置 -->
    <section id="view-settings" class="hidden">
      <div class="grid-2">
        <div class="card">
          <h3>运行参数</h3>
          <div class="inline">
            <div class="field"><label for="cfgBaseUrl">上游站点</label><input id="cfgBaseUrl" type="text"></div>
            <div class="field"><label for="cfgPrefix">模型前缀</label><input id="cfgPrefix" type="text" placeholder="miora/"></div>
            <div class="field"><label for="cfgPoll">轮询间隔（秒）</label><input id="cfgPoll" type="number" min="1" max="30"></div>
            <div class="field"><label for="cfgTimeout">任务超时（秒）</label><input id="cfgTimeout" type="number" min="30" max="1800"></div>
            <div class="field"><label for="cfgConcurrent">最大并发</label><input id="cfgConcurrent" type="number" min="1" max="64"></div>
            <div class="field"><label for="cfgImg">默认图像模型</label><input id="cfgImg" type="text"></div>
            <div class="field"><label for="cfgVid">默认视频模型</label><input id="cfgVid" type="text"></div>
            <div class="field"><label for="cfg3d">默认 3D 模型</label><input id="cfg3d" type="text"></div>
          </div>
          <div class="rows" style="margin-top:12px">
            <label class="split tiny"><input type="checkbox" id="cfgWatermark" style="width:auto"> 产物带水印</label>
            <label class="split tiny"><input type="checkbox" id="cfgBrowserImport" style="width:auto"> 允许浏览器登录态导入</label>
            <label class="split tiny"><input type="checkbox" id="cfgUploadRefs" style="width:auto"> 允许上传参考图（实验）</label>
          </div>
          <div class="split" style="margin-top:14px">
            <button class="btn primary" id="btnSaveSettings">保存到 CPA 配置</button>
            <button class="btn" id="btnDiagnostics">运行自检</button>
          </div>
        </div>
        <div class="card">
          <h3>自检 / 原始配置</h3>
          <div class="rows" id="diagRows"><div class="dim tiny">点「运行自检」检查目录、登录态与额度。</div></div>
          <details style="margin-top:14px">
            <summary class="dim tiny" style="cursor:pointer">查看当前配置 JSON</summary>
            <pre class="mono" id="settingsJson" style="white-space:pre-wrap;max-height:260px;overflow:auto"></pre>
          </details>
        </div>
      </div>
    </section>
  </main>
</div>

<div class="toast-wrap" id="toasts" aria-live="polite"></div>
<div class="lightbox hidden" id="lightbox"><div class="inner"><div id="lightboxBody"></div><div class="meta" id="lightboxMeta"></div><div class="split"><button class="btn sm" id="lightboxCopy">复制链接</button><button class="btn sm" id="lightboxSign">重签 URL</button><button class="btn sm right" id="lightboxClose">关闭</button></div></div></div>

<script>
(function () {
  "use strict";
  var P = "__PROVIDER__";
  var BASE = "/v0/management/" + P;
  var PANEL_STORE = "cli-proxy-auth";
  var ENC_PREFIX = "enc::v1::";
  // Compatibility with the host's XOR-obfuscated storage, not an encryption key.
  var STORAGE_SALT = "cli-proxy-api-webui::secure-storage";
  var SS_KEY = P + "-mgmt-key";

  var state = {
    view: "overview",
    status: null,
    accounts: [],
    models: [],
    tasks: [],
    media: [],
    events: [],
    settings: null,
    mediaFilter: "image",
    selectedModel: "",
    lightbox: null,
    polling: null
  };

  function encBytes(text) { return new TextEncoder().encode(text); }
  function xorBytes(data, key) {
    var out = new Uint8Array(data.length);
    for (var i = 0; i < data.length; i++) { out[i] = data[i] ^ key[i % key.length]; }
    return out;
  }
  function base64Bytes(text) {
    var bin = window.atob(text);
    var out = new Uint8Array(bin.length);
    for (var i = 0; i < bin.length; i++) { out[i] = bin.charCodeAt(i); }
    return out;
  }
  function deobfuscate(value) {
    if (!value || value.indexOf(ENC_PREFIX) !== 0) { return value; }
    try {
      var key = encBytes(STORAGE_SALT + "|" + window.location.host + "|" + window.navigator.userAgent);
      return new TextDecoder().decode(xorBytes(base64Bytes(value.slice(ENC_PREFIX.length)), key));
    } catch (e) { return value; }
  }
  function embeddedKey() {
    try { if (window.self === window.top) { return null; } } catch (e) { return null; }
    var raw;
    try { raw = window.localStorage.getItem(PANEL_STORE); } catch (e) { return null; }
    if (!raw) { return null; }
    try {
      var parsed = JSON.parse(deobfuscate(raw));
      var st = (parsed && parsed.state) || parsed || {};
      return typeof st.managementKey === "string" && st.managementKey ? st.managementKey : null;
    } catch (e) { return null; }
  }
  function urlKey() {
    var fragment = new URLSearchParams(window.location.hash.slice(1));
    if (!fragment.has("key")) { return null; }
    var value = (fragment.get("key") || "").trim();
    fragment.delete("key");
    var rest = fragment.toString();
    window.history.replaceState(null, "", window.location.pathname + window.location.search + (rest ? "#" + rest : ""));
    return value || null;
  }
  var initialURLKey = urlKey();
  var memoryKey = "";

  function adminKey() {
    var el = document.getElementById("managementKey");
    var entered = el ? el.value.trim() : "";
    if (entered) { memoryKey = entered; return entered; }
    if (memoryKey) { return memoryKey; }
    if (initialURLKey) { memoryKey = initialURLKey; return initialURLKey; }
    var embedded = embeddedKey();
    if (embedded) { memoryKey = embedded; return embedded; }
    try {
      var remembered = window.sessionStorage.getItem(SS_KEY);
      if (remembered) { memoryKey = remembered; return remembered; }
    } catch (e) {}
    return "";
  }

  function headers() {
    var h = { "Content-Type": "application/json" };
    var k = adminKey();
    if (k) { h["Authorization"] = "Bearer " + k; h["X-API-Key"] = k; }
    return h;
  }

  function esc(value) {
    return String(value == null ? "" : value).replace(/[&<>"']/g, function (c) {
      return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c];
    });
  }
  function fmtTime(value) {
    if (!value) { return "—"; }
    var d = new Date(value);
    if (isNaN(d.getTime())) { return String(value); }
    return d.toLocaleString("zh-CN", { hour12: false });
  }
  function fmtNumber(value) {
    var n = Number(value || 0);
    if (!isFinite(n)) { return "0"; }
    return (Math.round(n * 100) / 100).toString();
  }
  function ago(value) {
    if (!value) { return ""; }
    var diff = (Date.now() - new Date(value).getTime()) / 1000;
    if (!isFinite(diff)) { return ""; }
    if (diff < 60) { return Math.max(0, Math.round(diff)) + " 秒前"; }
    if (diff < 3600) { return Math.round(diff / 60) + " 分钟前"; }
    if (diff < 86400) { return Math.round(diff / 3600) + " 小时前"; }
    return Math.round(diff / 86400) + " 天前";
  }

  function toast(text, kind) {
    var wrap = document.getElementById("toasts");
    var el = document.createElement("div");
    el.className = "toast " + (kind || "");
    el.textContent = text;
    wrap.appendChild(el);
    setTimeout(function () {
      el.style.opacity = "0";
      el.style.transform = "translateY(6px)";
      setTimeout(function () { el.remove(); }, 220);
    }, kind === "err" ? 6200 : 3600);
  }

  function call(path, opts) {
    opts = opts || {};
    return fetch(BASE + path, {
      method: opts.method || "GET",
      headers: headers(),
      body: opts.body ? JSON.stringify(opts.body) : undefined
    }).then(function (response) {
      return response.text().then(function (text) {
        var data = null;
        try { data = text ? JSON.parse(text) : null; } catch (e) { data = { raw: text }; }
        if (!response.ok) {
          var message = (data && (data.error || data.message)) || ("HTTP " + response.status);
          throw new Error(message);
        }
        return data;
      });
    });
  }

  function hostPatch(config) {
    var k = adminKey();
    if (!k) { return Promise.reject(new Error("需要 CPA 管理密钥才能写入配置")); }
    return fetch("/v0/management/plugins/" + P + "/config", {
      method: "PATCH",
      headers: { "Content-Type": "application/json", "Authorization": "Bearer " + k, "X-API-Key": k },
      body: JSON.stringify(config)
    }).then(function (response) {
      if (!response.ok) {
        return response.text().then(function (t) { throw new Error("写入配置失败：HTTP " + response.status + " " + t.slice(0, 160)); });
      }
      return true;
    });
  }

  // ---------------------------------------------------------------- views
  var VIEW_META = {
    overview: ["概览", "Miora 的图像 / 视频 / 3D 能力，通过 CLIProxyAPI 反代为 OpenAI 兼容接口。"],
    accounts: ["账号", "导入并维护 Miora 登录态；插件会自动轮换 refresh token 并写回 auth 文件。"],
    studio: ["生成台", "直接在这里试跑 Miora 的每一个模型，参数按模型能力自动收窄。"],
    library: ["产物", "fileKey 永久有效，签名 URL 1 小时过期，可随时重签。"],
    models: ["模型", "上游媒体模型目录：能力、比例、分辨率与参考图上限。"],
    logs: ["日志", "本进程内的请求记录，便于定位 401、额度与上游失败。"],
    settings: ["设置", "参数写入 CPA 的 plugins.configs，保存后插件会收到 plugin.reconfigure。"]
  };

  function show(view) {
    if (!VIEW_META[view]) { view = "overview"; }
    state.view = view;
    Array.prototype.forEach.call(document.querySelectorAll(".nav-item"), function (btn) {
      btn.setAttribute("aria-current", btn.dataset.view === view ? "true" : "false");
    });
    ["overview", "accounts", "studio", "library", "models", "logs", "settings"].forEach(function (name) {
      document.getElementById("view-" + name).classList.toggle("hidden", name !== view);
    });
    document.getElementById("viewTitle").textContent = VIEW_META[view][0];
    document.getElementById("viewSub").textContent = VIEW_META[view][1];
    if (window.location.hash.slice(1) !== view) {
      window.history.replaceState(null, "", "#" + view);
    }
    if (view === "accounts") { loadAccounts(false); }
    if (view === "studio") { loadTasks(); }
    if (view === "library") { loadMedia(); }
    if (view === "models") { loadModels(); }
    if (view === "logs") { loadLogs(); }
    if (view === "settings") { loadSettings(); }
  }

  // ---------------------------------------------------------------- overview
  function renderOverview() {
    var status = state.status || {};
    document.getElementById("statAccounts").textContent = status.accounts != null ? status.accounts : "–";
    document.getElementById("statModels").textContent = status.models != null ? status.models : "–";
    document.getElementById("statModelsHint").textContent = status.tier ? ("tier · " + status.tier) : "上游目录";
    var credits = 0, any = false;
    state.accounts.forEach(function (acct) {
      if (acct.credits && typeof acct.credits.credits === "number") { credits += acct.credits.credits; any = true; }
    });
    document.getElementById("statCredits").textContent = any ? fmtNumber(credits) : (state.accounts.length ? "待拉取" : "—");
    document.getElementById("statTasks").textContent = state.tasks.length;
    document.getElementById("statTasksHint").textContent = state.tasks.length ? ago(state.tasks[0].finished_at || state.tasks[0].started_at) : "本进程内";
    document.getElementById("navAccounts").textContent = status.accounts != null ? status.accounts : "";
    document.getElementById("versionText").textContent = "v" + (status.version || "0.1.0") + " · " + P;
    var modelHint = document.getElementById("autoModelHint");
    if (modelHint) { modelHint.textContent = (status.model_prefix || "") + "miora-auto"; }

    var dot = document.getElementById("healthDot");
    var text = document.getElementById("healthText");
    if (status.ok) {
      dot.classList.remove("off");
      text.textContent = "已连接 · " + (status.base_url || "");
    } else {
      dot.classList.add("off");
      text.textContent = "上游异常：" + (status.catalog_error || "未知");
    }

    var chips = document.getElementById("capChips");
    var caps = status.capabilities || {};
    var enabled = [];
    Object.keys(caps).forEach(function (key) {
      if (caps[key] === true) { enabled.push(key); }
    });
    chips.innerHTML = enabled.length
      ? enabled.map(function (k) { return '<span class="chip">' + esc(k) + "</span>"; }).join("")
      : '<span class="dim tiny">无</span>';

    renderTaskRows(document.getElementById("recentTasks"), state.tasks.slice(0, 4), true);
  }

  function renderTaskRows(container, tasks, compact) {
    if (!tasks || !tasks.length) {
      container.innerHTML = '<div class="empty"><div class="big">◍</div><div>还没有生成记录</div></div>';
      return;
    }
    container.innerHTML = tasks.map(function (task) {
      var running = task.status && ["pending", "processing", "running", "submitted", "queued", "submitting"].indexOf(task.status) >= 0;
      var pct = Math.max(0, Math.min(100, Number(task.progress || 0)));
      var files = task.files || [];
      var preview = "";
      if (files.length) {
        var file = files[0];
        if ((file.type === "image") && file.url) {
          preview = '<img src="' + esc(file.url) + '" alt="" style="width:64px;height:64px;border-radius:10px;object-fit:cover;border:1px solid var(--stroke)">';
        } else if (file.url) {
          preview = '<span class="pill brand">' + esc(file.type || "file") + "</span>";
        }
      }
      var statusPill = running ? '<span class="pill warn">' + esc(task.status) + "</span>"
        : (task.status === "completed" ? '<span class="pill ok">完成</span>'
          : '<span class="pill err">' + esc(task.status || "unknown") + "</span>");
      var bar = running
        ? '<div class="bar ' + (pct > 0 ? "" : "idle") + '"><i style="width:' + pct + '%"></i></div>'
        : "";
      var meta = [];
      if (task.model) { meta.push(esc(task.model)); }
      if (task.credit) { meta.push(fmtNumber(task.credit) + " credits"); }
      if (task.elapsed_ms) { meta.push((task.elapsed_ms / 1000).toFixed(1) + "s"); }
      var err = task.error ? '<div class="pill err" style="margin-top:4px">' + esc(task.error) + "</div>" : "";
      return '<div class="task">' +
        '<div class="head"><div class="split" style="min-width:0">' + preview +
          '<div style="min-width:0"><div class="mono tiny dim">' + esc(task.task_id || task.id || "—") + "</div>" +
          '<div class="prompt">' + esc(task.prompt || "") + "</div></div></div>" + statusPill + "</div>" +
        bar +
        '<div class="split tiny dim"><span>' + meta.join(" · ") + "</span><span class=\"right\">" + ago(task.finished_at || task.started_at) + "</span></div>" +
        err +
        (compact && files.length ? '<div class="split tiny">' + files.slice(0, 3).map(function (f) {
          return '<a class="pill brand" href="' + esc(f.url) + '" target="_blank" rel="noopener">查看产物</a>';
        }).join("") + "</div>" : "") +
        "</div>";
    }).join("");
  }

  // ---------------------------------------------------------------- accounts
  function loadStatus() {
    return call("/status").then(function (data) {
      state.status = data;
      renderOverview();
    }).catch(function (error) {
      document.getElementById("healthDot").classList.add("off");
      document.getElementById("healthText").textContent = "管理 API 不可用：" + error.message;
    });
  }

  function loadAccounts(live) {
    return call("/accounts" + (live ? "?live=1" : "")).then(function (data) {
      state.accounts = (data && data.accounts) || [];
      renderAccounts();
      renderOverview();
      return state.accounts;
    }).catch(function (error) {
      document.getElementById("accountList").innerHTML = '<div class="empty"><div class="big">!</div><div>' + esc(error.message) + "</div></div>";
    });
  }

  function renderAccounts() {
    var container = document.getElementById("accountList");
    if (!state.accounts.length) {
      container.innerHTML = '<div class="empty"><div class="big">◌</div><div>还没有账号</div><ol>' +
        "<li>在浏览器登录 miora.design</li>" +
        "<li>回到本页点「扫描本机浏览器」</li>" +
        "<li>选一条登录态点「导入」</li></ol></div>";
      return;
    }
    container.innerHTML = state.accounts.map(function (acct) {
      var credits = acct.credits || {};
      var total = Number(credits.total_credits || credits.total || 0);
      var remaining = Number(credits.credits || 0);
      var pct = total > 0 ? Math.max(0, Math.min(100, (remaining / total) * 100)) : (remaining > 0 ? 100 : 0);
      var hours = acct.expires_in_hours;
      var expiryPill = hours == null ? '<span class="pill">未知到期</span>'
        : (hours < 0 ? '<span class="pill err">已过期</span>'
          : (hours < 48 ? '<span class="pill warn">' + hours + " 小时后过期</span>"
            : '<span class="pill ok">' + Math.round(hours / 24) + " 天后过期</span>"));
      var err = credits.error ? '<div class="pill err" style="margin-top:6px">' + esc(credits.error) + "</div>" : "";
      return '<div class="task">' +
        '<div class="head"><div><div style="font-weight:600">' + esc(acct.email || acct.label || acct.name) + "</div>" +
        '<div class="tiny dim mono">' + esc(acct.name || "") + " · " + esc(acct.plan || "free") + "</div></div>" +
        '<div class="split">' + expiryPill +
        (acct.has_refresh_token ? '<span class="pill ok">可自动续期</span>' : '<span class="pill warn">无 refresh token</span>') +
        "</div></div>" +
        '<div class="bar"><i style="width:' + pct + '%"></i></div>' +
        '<div class="split tiny dim"><span>剩余 ' + fmtNumber(remaining) + " / " + fmtNumber(total) + " credits</span>" +
        "<span>已用 " + fmtNumber(credits.used_credits) + "</span>" +
        (credits.checked_at ? '<span class="right">' + ago(credits.checked_at) + "更新</span>" : "") + "</div>" + err +
        '<div class="split"><button class="btn sm" data-acct-test="' + esc(acct.auth_index) + '">测试</button>' +
        '<button class="btn sm" data-acct-refresh="' + esc(acct.auth_index) + '">轮换 token</button>' +
        '<button class="btn sm danger right" data-acct-delete="' + esc(acct.auth_index) + '">删除</button></div>' +
        "</div>";
    }).join("");
  }

  function scanBrowsers() {
    var btn = document.getElementById("btnScan");
    btn.disabled = true;
    document.getElementById("scanResults").innerHTML = '<div class="skeleton" style="width:70%"></div><div class="skeleton" style="width:45%"></div>';
    call("/accounts/scan", { method: "POST", body: {} }).then(function (data) {
      var candidates = (data && data.candidates) || [];
      if (!candidates.length) {
        document.getElementById("scanResults").innerHTML = '<div class="empty"><div class="big">∅</div><div>' + esc((data && data.error) || "没有扫描到登录态") + "</div></div>";
        return;
      }
      document.getElementById("scanResults").innerHTML = candidates.map(function (c, index) {
        var label = c.email || c.user_id || "未知账号";
        var badge = c.valid ? '<span class="pill ok">有效</span>' : '<span class="pill err">不可用</span>';
        return '<div class="task"><div class="head"><div><div style="font-weight:600">' + esc(label) + "</div>" +
          '<div class="tiny dim mono">' + esc(c.browser) + " · " + esc(c.profile) + (c.expires_at ? " · 到期 " + esc(fmtTime(c.expires_at)) : "") + "</div></div>" +
          '<div class="split">' + badge + '<button class="btn sm primary" data-import-index="' + index + '">导入</button></div></div>' +
          '<div class="tiny dim">' + esc(c.message || "") + "</div></div>";
      }).join("");
      state.scanned = candidates;
    }).catch(function (error) {
      document.getElementById("scanResults").innerHTML = '<div class="empty"><div class="big">!</div><div>' + esc(error.message) + "</div></div>";
    }).finally(function () { btn.disabled = false; });
  }

  function importCandidate(index) {
    var candidate = (state.scanned || [])[index];
    if (!candidate) { return; }
    call("/accounts/browser-import", {
      method: "POST",
      body: { auth_token: candidate.auth_token, refresh_token: candidate.refresh_token, label: candidate.email }
    }).then(function (data) {
      toast((data && data.message) || "已导入", "ok");
      document.getElementById("scanResults").innerHTML = "";
      loadAccounts(true);
      loadStatus();
    }).catch(function (error) { toast(error.message, "err"); });
  }

  function importPasted() {
    var raw = document.getElementById("pasteToken").value.trim();
    if (!raw) { toast("先粘贴 authToken 或浏览器导出的 JSON", "err"); return; }
    call("/accounts/import", { method: "POST", body: { raw: raw } }).then(function (data) {
      toast((data && data.message) || "已导入", "ok");
      document.getElementById("pasteToken").value = "";
      loadAccounts(true);
    }).catch(function (error) { toast(error.message, "err"); });
  }

  // ---------------------------------------------------------------- studio
  function loadModels(force) {
    var path = force ? null : "/models";
    var promise = force
      ? call("/models/refresh", { method: "POST", body: {} })
      : call(path);
    return promise.then(function (data) {
      state.models = (data && data.models) || [];
      renderModelRows();
      renderModelSelect();
      if (force) { toast("模型目录已刷新", "ok"); }
    }).catch(function (error) { toast("模型目录加载失败：" + error.message, "err"); });
  }

  function mediaLabel(media) {
    if (media === "video") { return "视频"; }
    if (media === "three_d") { return "3D"; }
    return "图像";
  }

  function renderModelRows() {
    var filter = (document.getElementById("modelFilter").value || "").toLowerCase().trim();
    var rows = state.models.filter(function (model) {
      if (!filter) { return true; }
      return (model.id || "").toLowerCase().indexOf(filter) >= 0 || (model.label || "").toLowerCase().indexOf(filter) >= 0;
    });
    var body = document.getElementById("modelRows");
    if (!rows.length) {
      body.innerHTML = '<tr><td colspan="6"><div class="dim">没有匹配的模型</div></td></tr>';
      return;
    }
    body.innerHTML = rows.map(function (model) {
      var ratios = (model.aspect_ratios || []).slice(0, 5).join(" ") || "—";
      var resolutions = (model.resolutions || []).join(" ") || "—";
      var refs = model.supports_refs ? ("≤ " + (model.max_references || 1)) : "不支持";
      var status = model.available ? '<span class="pill ok">可用</span>' : '<span class="pill warn">受限</span>';
      return "<tr><td><div style=\"font-weight:600\">" + esc(model.label || model.upstream) + "</div>" +
        '<div class="tiny dim mono">' + esc(model.id) + "</div></td>" +
        "<td>" + esc(mediaLabel(model.media)) + "</td>" +
        '<td class="tiny">' + esc(ratios) + "</td>" +
        '<td class="tiny">' + esc(resolutions) + "</td>" +
        '<td class="tiny">' + esc(refs) + "</td>" +
        "<td>" + status + "</td></tr>";
    }).join("");
  }

  function modelsForMedia(media) {
    return state.models.filter(function (model) { return model.media === media; });
  }

  function renderModelSelect() {
    var select = document.getElementById("modelSelect");
    var options = modelsForMedia(state.mediaFilter);
    var autoId = ((state.status && state.status.model_prefix) || "") + "miora-auto";
    var html = '<option value="' + esc(autoId) + '">自动（miora-auto）</option>';
    html += options.map(function (model) {
      return '<option value="' + esc(model.id) + '">' + esc(model.label || model.upstream) + " · " + esc(model.upstream) + "</option>";
    }).join("");
    select.innerHTML = html;
    if (state.selectedModel && options.some(function (m) { return m.id === state.selectedModel; })) {
      select.value = state.selectedModel;
    }
    applyModelDefaults();
  }

  function currentModel() {
    var id = document.getElementById("modelSelect").value;
    for (var i = 0; i < state.models.length; i++) {
      if (state.models[i].id === id) { return state.models[i]; }
    }
    return null;
  }

  function applyModelDefaults() {
    var model = currentModel();
    var ratioSelect = document.getElementById("ratioSelect");
    var resSelect = document.getElementById("resSelect");
    var durationField = document.getElementById("durationField");
    var durationInput = document.getElementById("durationInput");

    var ratios = (model && model.aspect_ratios) || ["1:1", "16:9", "9:16", "4:3", "3:4"];
    ratioSelect.innerHTML = ratios.map(function (r) {
      return '<option value="' + esc(r) + '"' + (model && r === model.default_ratio ? " selected" : "") + ">" + esc(r) + "</option>";
    }).join("");

    var resolutions = (model && model.resolutions) || (state.mediaFilter === "video" ? ["480P", "768P"] : ["1K", "1.5K", "2K"]);
    resSelect.innerHTML = resolutions.map(function (r) {
      return '<option value="' + esc(r) + '"' + (model && r === model.default_res ? " selected" : "") + ">" + esc(r) + "</option>";
    }).join("");

    durationField.style.display = state.mediaFilter === "video" ? "" : "none";
    if (state.mediaFilter === "video") {
      var range = (model && model.seconds_range) || { min: 5, max: 15 };
      durationInput.min = range.min || 1;
      durationInput.max = range.max || 30;
      durationInput.value = (model && model.default_seconds) || range.min || 5;
    }
  }

  function submitGeneration() {
    var prompt = document.getElementById("promptInput").value.trim();
    if (!prompt) { toast("先写提示词", "err"); return; }
    var model = currentModel();
    var payload = {
      prompt: prompt,
      model: model ? model.upstream : "",
      media: state.mediaFilter,
      aspect_ratio: document.getElementById("ratioSelect").value,
      resolution: document.getElementById("resSelect").value
    };
    if (state.mediaFilter === "video") {
      payload.duration = Number(document.getElementById("durationInput").value || 5);
    }
    var btn = document.getElementById("btnGenerate");
    btn.disabled = true;
    call("/generate", { method: "POST", body: payload }).then(function (data) {
      toast("已提交：" + (data.model || "") + "（" + mediaLabel(data.media || state.mediaFilter) + "）", "ok");
      loadTasks();
      startPolling();
    }).catch(function (error) { toast(error.message, "err"); })
      .finally(function () { btn.disabled = false; });
  }

  function loadTasks() {
    return call("/tasks?limit=20").then(function (data) {
      state.tasks = (data && data.tasks) || [];
      renderTaskRows(document.getElementById("studioTasks"), state.tasks, true);
      renderOverview();
      var running = state.tasks.some(function (task) {
        return ["pending", "processing", "running", "submitted", "queued", "submitting"].indexOf(task.status) >= 0;
      });
      if (running) { startPolling(); }
    }).catch(function (error) {
      document.getElementById("studioTasks").innerHTML = '<div class="empty"><div class="big">!</div><div>' + esc(error.message) + "</div></div>";
    });
  }

  function startPolling() {
    if (state.polling) { return; }
    state.polling = setInterval(function () {
      call("/tasks?limit=20").then(function (data) {
        state.tasks = (data && data.tasks) || [];
        renderTaskRows(document.getElementById("studioTasks"), state.tasks, true);
        renderOverview();
        var running = state.tasks.some(function (task) {
          return ["pending", "processing", "running", "submitted", "queued", "submitting"].indexOf(task.status) >= 0;
        });
        if (!running) {
          clearInterval(state.polling);
          state.polling = null;
          loadMedia();
        }
      }).catch(function () {
        clearInterval(state.polling);
        state.polling = null;
      });
    }, 2500);
  }

  // ---------------------------------------------------------------- library
  function loadMedia() {
    return call("/media?limit=60").then(function (data) {
      state.media = (data && data.media) || [];
      renderGallery();
    }).catch(function (error) { toast("产物加载失败：" + error.message, "err"); });
  }

  function renderGallery() {
    var gallery = document.getElementById("gallery");
    if (!state.media.length) {
      gallery.innerHTML = '<div class="empty" style="grid-column:1/-1"><div class="big">◍</div><div>还没有产物</div><div class="tiny dim">去「生成台」跑一张试试</div></div>';
      return;
    }
    gallery.innerHTML = state.media.map(function (item, index) {
      var isImage = item.media === "image";
      var body = isImage
        ? '<img loading="lazy" src="' + esc(item.url) + '" alt="">'
        : '<div class="empty" style="height:100%"><div class="big">▶</div><div class="tiny">' + esc(item.media) + "</div></div>";
      return '<div class="thumb" data-media-index="' + index + '">' + body +
        '<span class="kind pill brand">' + esc(mediaLabel(item.media)) + "</span>" +
        '<div class="cap"><span>' + esc((item.prompt || "").slice(0, 28)) + "</span><span>" + fmtNumber(item.credit) + "c</span></div></div>";
    }).join("");
  }

  function openLightbox(index) {
    var item = state.media[index];
    if (!item) { return; }
    state.lightbox = item;
    var body = document.getElementById("lightboxBody");
    if (item.media === "image") {
      body.innerHTML = '<img src="' + esc(item.url) + '" alt="">';
    } else if (item.media === "video") {
      body.innerHTML = '<video src="' + esc(item.url) + '" controls autoplay playsinline></video>';
    } else {
      body.innerHTML = '<div class="card"><a class="btn primary" href="' + esc(item.url) + '" target="_blank" rel="noopener">下载 3D 模型</a></div>';
    }
    document.getElementById("lightboxMeta").innerHTML =
      '<span class="mono tiny">' + esc(item.file_key) + "</span><span>" + esc(item.model || "") + "</span><span>" + esc(item.media) + "</span>";
    document.getElementById("lightbox").classList.remove("hidden");
  }

  function resignLightbox() {
    if (!state.lightbox) { return; }
    call("/media/sign", { method: "POST", body: { file_key: state.lightbox.file_key } }).then(function (data) {
      var url = (data && data.urls && data.urls[state.lightbox.file_key]) || "";
      if (!url) { toast("重签失败：没有返回 URL", "err"); return; }
      state.lightbox.url = url;
      state.media.forEach(function (item) {
        if (item.file_key === state.lightbox.file_key) { item.url = url; }
      });
      openLightbox(state.media.indexOf(state.lightbox));
      renderGallery();
      toast("已重新签名（1 小时有效）", "ok");
    }).catch(function (error) { toast(error.message, "err"); });
  }

  // ---------------------------------------------------------------- logs
  function loadLogs() {
    return call("/logs?limit=100").then(function (data) {
      state.events = (data && data.events) || [];
      var body = document.getElementById("logRows");
      if (!state.events.length) {
        body.innerHTML = '<tr><td colspan="7"><div class="dim">还没有请求记录</div></td></tr>';
        return;
      }
      body.innerHTML = state.events.map(function (event) {
        var pill = event.status === "completed" ? '<span class="pill ok">完成</span>'
          : (event.status === "failed" ? '<span class="pill err">失败</span>' : '<span class="pill">' + esc(event.status) + "</span>");
        return "<tr><td class=\"tiny\">" + esc(fmtTime(event.time)) + "</td>" +
          "<td class=\"tiny\">" + esc(event.account || "—") + "</td>" +
          '<td class="mono">' + esc(event.model || "—") + "</td>" +
          "<td>" + pill + "</td>" +
          '<td class="tiny">' + fmtNumber(event.credit) + "</td>" +
          '<td class="tiny">' + (event.elapsed_ms ? (event.elapsed_ms / 1000).toFixed(1) + "s" : "—") + "</td>" +
          '<td class="tiny dim">' + esc(event.prompt || event.error || "") + "</td></tr>";
      }).join("");
    }).catch(function (error) { toast("日志加载失败：" + error.message, "err"); });
  }

  // ---------------------------------------------------------------- settings
  function loadSettings() {
    return call("/settings").then(function (data) {
      var cfg = (data && data.config) || {};
      state.settings = cfg;
      document.getElementById("cfgBaseUrl").value = cfg.base_url || "";
      document.getElementById("cfgPrefix").value = cfg.model_prefix || "";
      document.getElementById("cfgPoll").value = cfg.poll_interval_seconds || 4;
      document.getElementById("cfgTimeout").value = cfg.task_timeout_seconds || 420;
      document.getElementById("cfgConcurrent").value = cfg.max_concurrent_tasks || 4;
      document.getElementById("cfgImg").value = cfg.default_image_model || "";
      document.getElementById("cfgVid").value = cfg.default_video_model || "";
      document.getElementById("cfg3d").value = cfg.default_3d_model || "";
      document.getElementById("cfgWatermark").checked = !!cfg.include_watermark;
      document.getElementById("cfgBrowserImport").checked = cfg.enable_browser_import !== false;
      document.getElementById("cfgUploadRefs").checked = !!cfg.upload_reference_images;
      document.getElementById("settingsJson").textContent = JSON.stringify(cfg, null, 2);
    }).catch(function (error) { toast("设置读取失败：" + error.message, "err"); });
  }

  function saveSettings() {
    var patch = {
      base_url: document.getElementById("cfgBaseUrl").value.trim(),
      model_prefix: document.getElementById("cfgPrefix").value.trim(),
      poll_interval_seconds: Number(document.getElementById("cfgPoll").value || 4),
      task_timeout_seconds: Number(document.getElementById("cfgTimeout").value || 420),
      max_concurrent_tasks: Number(document.getElementById("cfgConcurrent").value || 4),
      default_image_model: document.getElementById("cfgImg").value.trim(),
      default_video_model: document.getElementById("cfgVid").value.trim(),
      default_3d_model: document.getElementById("cfg3d").value.trim(),
      include_watermark: document.getElementById("cfgWatermark").checked,
      enable_browser_import: document.getElementById("cfgBrowserImport").checked,
      upload_reference_images: document.getElementById("cfgUploadRefs").checked
    };
    hostPatch(patch).then(function () {
      toast("已写入 CPA 配置，插件会立即重新加载", "ok");
      setTimeout(function () { loadStatus(); loadSettings(); }, 600);
    }).catch(function (error) { toast(error.message, "err"); });
  }

  function runDiagnostics() {
    var container = document.getElementById("diagRows");
    container.innerHTML = '<div class="skeleton" style="width:60%"></div>';
    call("/diagnostics").then(function (data) {
      var checks = (data && data.checks) || [];
      if (!checks.length) { container.innerHTML = '<div class="dim tiny">没有返回检查项</div>'; return; }
      container.innerHTML = checks.map(function (check) {
        return '<div class="row-line"><div><div>' + esc(check.name) + "</div>" +
          '<div class="tiny dim">' + esc(check.detail || check.error || "") + "</div></div>" +
          (check.ok ? '<span class="pill ok">正常</span>' : '<span class="pill err">异常</span>') + "</div>";
      }).join("");
    }).catch(function (error) { container.innerHTML = '<div class="pill err">' + esc(error.message) + "</div>"; });
  }

  // ---------------------------------------------------------------- wiring
  document.getElementById("nav").addEventListener("click", function (event) {
    var btn = event.target.closest(".nav-item");
    if (btn) { show(btn.dataset.view); }
  });
  document.body.addEventListener("click", function (event) {
    var goto = event.target.closest("[data-goto]");
    if (goto) { show(goto.dataset.goto); return; }
    var imp = event.target.closest("[data-import-index]");
    if (imp) { importCandidate(Number(imp.dataset.importIndex)); return; }
    var test = event.target.closest("[data-acct-test]");
    if (test) {
      var idx = test.dataset.acctTest;
      test.disabled = true;
      call("/accounts/test", { method: "POST", body: { auth_index: idx } })
        .then(function (data) {
          var acct = (data && data.account) || {};
          toast(acct.error ? ("测试失败：" + acct.error) : ("OK · " + (acct.email || "") + " · 剩余 " + fmtNumber(acct.credits)), acct.error ? "err" : "ok");
          loadAccounts(false);
        })
        .catch(function (error) { toast(error.message, "err"); })
        .finally(function () { test.disabled = false; });
      return;
    }
    var refresh = event.target.closest("[data-acct-refresh]");
    if (refresh) {
      refresh.disabled = true;
      call("/accounts/refresh", { method: "POST", body: { auth_index: refresh.dataset.acctRefresh } })
        .then(function (data) { toast((data && data.message) || "已轮换", "ok"); loadAccounts(true); })
        .catch(function (error) { toast(error.message, "err"); })
        .finally(function () { refresh.disabled = false; });
      return;
    }
    var del = event.target.closest("[data-acct-delete]");
    if (del) {
      if (!window.confirm("删除这个账号的 auth 文件？该账号将不再可用于请求。")) { return; }
      call("/accounts/delete", { method: "POST", body: { auth_index: del.dataset.acctDelete } })
        .then(function () { toast("已删除", "ok"); loadAccounts(false); })
        .catch(function (error) { toast(error.message, "err"); });
      return;
    }
    var thumb = event.target.closest("[data-media-index]");
    if (thumb) { openLightbox(Number(thumb.dataset.mediaIndex)); }
  });

  document.getElementById("managementKey").addEventListener("change", function () {
    try { window.sessionStorage.setItem(SS_KEY, this.value.trim()); } catch (e) {}
    memoryKey = this.value.trim();
    loadStatus().then(function () { loadAccounts(false); });
  });

  document.getElementById("btnRefresh").addEventListener("click", function () {
    loadStatus().then(function () { return loadAccounts(false); }).then(function () { loadTasks(); }).then(function () {
      if (state.view === "library") { loadMedia(); }
      if (state.view === "logs") { loadLogs(); }
      toast("已刷新", "ok");
    });
  });
  document.getElementById("btnScan").addEventListener("click", scanBrowsers);
  document.getElementById("btnPaste").addEventListener("click", importPasted);
  document.getElementById("btnLiveQuota").addEventListener("click", function () {
    call("/accounts?live=1").then(function (data) {
      state.accounts = (data && data.accounts) || [];
      renderAccounts();
      toast("已拉取实时额度", "ok");
    }).catch(function (error) { toast(error.message, "err"); });
  });

  document.getElementById("mediaTabs").addEventListener("click", function (event) {
    var btn = event.target.closest("button[data-media]");
    if (!btn) { return; }
    state.mediaFilter = btn.dataset.media;
    Array.prototype.forEach.call(this.querySelectorAll("button"), function (item) {
      item.setAttribute("aria-pressed", item === btn ? "true" : "false");
    });
    renderModelSelect();
  });
  document.getElementById("modelSelect").addEventListener("change", function () {
    state.selectedModel = this.value;
    applyModelDefaults();
  });
  document.getElementById("promptInput").addEventListener("input", function () {
    document.getElementById("promptCount").textContent = this.value.length + " 字";
  });
  document.getElementById("promptInput").addEventListener("keydown", function (event) {
    if ((event.metaKey || event.ctrlKey) && event.key === "Enter") {
      event.preventDefault();
      submitGeneration();
    }
  });
  document.getElementById("btnGenerate").addEventListener("click", submitGeneration);
  document.getElementById("btnReloadModels").addEventListener("click", function () { loadModels(true); });
  document.getElementById("modelFilter").addEventListener("input", renderModelRows);
  document.getElementById("btnReloadLogs").addEventListener("click", loadLogs);
  document.getElementById("btnSaveSettings").addEventListener("click", saveSettings);
  document.getElementById("btnDiagnostics").addEventListener("click", runDiagnostics);
  document.getElementById("lightboxClose").addEventListener("click", function () {
    document.getElementById("lightbox").classList.add("hidden");
  });
  document.getElementById("lightboxCopy").addEventListener("click", function () {
    if (!state.lightbox) { return; }
    if (navigator.clipboard) {
      navigator.clipboard.writeText(state.lightbox.url).then(function () { toast("已复制链接", "ok"); });
    } else {
      toast(state.lightbox.url, "ok");
    }
  });
  document.getElementById("lightboxSign").addEventListener("click", resignLightbox);
  document.getElementById("lightbox").addEventListener("click", function (event) {
    if (event.target === this) { this.classList.add("hidden"); }
  });
  document.addEventListener("keydown", function (event) {
    if (event.key === "Escape") { document.getElementById("lightbox").classList.add("hidden"); }
  });

  // boot
  show((window.location.hash || "#overview").slice(1));
  loadStatus().then(function () {
    return loadAccounts(false);
  }).then(function () {
    return call("/tasks?limit=20");
  }).then(function (data) {
    state.tasks = (data && data.tasks) || [];
    renderOverview();
    if (state.view === "studio") { renderTaskRows(document.getElementById("studioTasks"), state.tasks, true); }
    return loadModels();
  }).catch(function (error) {
    toast("初始化失败：" + error.message, "err");
  });
})();
</script>
</body>
</html>
`
