package main

import "strings"

// loginPageHTML is served by the loopback listener created by auth.login.start.
// Miora only offers Google/GitHub OAuth, so there is no device-code flow to
// reproduce: the page either imports the session the user already has in their
// browser, or accepts a pasted token pair. It has no access to the host: it posts
// to the loopback listener, which validates against the upstream before the
// credential is handed back to CPA.
func loginPageHTML(state, provider string) string {
	page := strings.ReplaceAll(loginTemplate, "__STATE__", state)
	return strings.ReplaceAll(page, "__PROVIDER__", provider)
}

const loginTemplate = `<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>连接 Miora · CLIProxyAPI</title>
<style>
  :root {
    --ink: #07080c; --text: #ebedf5; --muted: #8b93a8; --stroke: rgba(255,255,255,.1);
    --brand: #7c5cff; --brand-2: #ff5fa2; --ok: #3ddc97; --err: #ff6b6b;
    --sans: -apple-system, BlinkMacSystemFont, "Segoe UI", "PingFang SC", "Microsoft YaHei", sans-serif;
    --mono: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
  }
  * { box-sizing: border-box; }
  body {
    margin: 0; min-height: 100vh; color: var(--text); font: 14px/1.65 var(--sans);
    background:
      radial-gradient(800px 500px at 10% -10%, rgba(124,92,255,.35), transparent 60%),
      radial-gradient(700px 420px at 95% 10%, rgba(255,95,162,.24), transparent 60%),
      var(--ink);
    display: grid; place-items: center; padding: 28px;
  }
  .card {
    width: min(600px, 100%); border: 1px solid var(--stroke); border-radius: 20px; padding: 26px;
    background: rgba(255,255,255,.045); backdrop-filter: blur(14px);
    box-shadow: 0 30px 80px -40px rgba(0,0,0,.9);
  }
  .brand { display: flex; align-items: center; gap: 12px; margin-bottom: 6px; }
  .mark { width: 32px; height: 32px; border-radius: 10px; background: conic-gradient(from 150deg, var(--brand), var(--brand-2), #ffb45c, var(--brand)); }
  h1 { font-size: 17px; margin: 0; letter-spacing: -.2px; }
  .sub { color: var(--muted); font-size: 12.5px; margin: 2px 0 18px; }
  .pill { display: inline-block; font-size: 11.5px; padding: 2px 9px; border-radius: 999px; border: 1px solid var(--stroke); color: var(--muted); }
  .pill.ok { color: var(--ok); border-color: rgba(61,220,151,.45); }
  .pill.err { color: var(--err); border-color: rgba(255,107,107,.45); }
  button {
    font: inherit; padding: 9px 14px; border-radius: 11px; cursor: pointer; color: #fff; border: none;
    background: linear-gradient(120deg, var(--brand), var(--brand-2));
  }
  button.ghost { background: rgba(255,255,255,.06); color: var(--text); border: 1px solid var(--stroke); }
  button:disabled { opacity: .55; cursor: not-allowed; }
  textarea, input {
    width: 100%; background: rgba(0,0,0,.32); color: var(--text); border: 1px solid var(--stroke);
    border-radius: 11px; padding: 10px 12px; font: inherit; outline: none;
  }
  textarea { min-height: 84px; resize: vertical; font-family: var(--mono); font-size: 12px; }
  .row { display: flex; gap: 8px; align-items: center; flex-wrap: wrap; }
  .block { border: 1px solid var(--stroke); border-radius: 14px; padding: 14px; margin-top: 14px; background: rgba(0,0,0,.18); }
  .block h2 { font-size: 13px; margin: 0 0 8px; color: var(--muted); letter-spacing: .3px; text-transform: uppercase; }
  .hint { color: var(--muted); font-size: 12px; }
  .list { display: grid; gap: 8px; margin-top: 10px; }
  .item { display: flex; justify-content: space-between; gap: 10px; align-items: center; padding: 10px 12px; border: 1px solid var(--stroke); border-radius: 11px; }
  .mono { font-family: var(--mono); font-size: 12px; }
  .status { margin-top: 10px; font-size: 12.5px; }
  #done { display: none; }
  .done-ok #done { display: block; }
  .done-ok #choose { display: none; }
  .ok { color: var(--ok); }
  .err { color: var(--err); }
</style>
</head>
<body>
<div class="card">
  <div class="brand">
    <div class="mark"></div>
    <div>
      <h1>连接 Miora</h1>
      <div class="sub">为 CLIProxyAPI 插件 <span class="mono">__PROVIDER__</span> 导入登录态 · 仅本地回环地址</div>
    </div>
  </div>

  <div id="done">
    <div class="pill ok">已导入</div>
    <p class="hint" id="doneText" style="margin-top:10px">可以关闭此页面，回到 CPA 完成添加账号。</p>
  </div>

  <div id="choose">
    <div class="block">
      <h2>方式一 · 从本机浏览器导入</h2>
      <p class="hint" style="margin:0 0 10px">读取本机 Edge / Chrome 里已登录的 miora.design 会话（只读 Local Storage，不碰密码）。</p>
      <div class="row">
        <button id="scan">扫描浏览器</button>
        <span class="hint" id="scanState"></span>
      </div>
      <div class="list" id="candidates"></div>
    </div>

    <div class="block">
      <h2>方式二 · 粘贴 token</h2>
      <p class="hint" style="margin:0 0 8px">DevTools → Application → Local Storage → https://miora.design，复制 <span class="mono">authToken</span>（可选再复制 <span class="mono">refreshToken</span>）。</p>
      <textarea id="token" placeholder="eyJ0eXAiOiJKV1Qi..."></textarea>
      <textarea id="refresh" placeholder="def50200...（可选，用于自动续期）" style="margin-top:8px;min-height:52px"></textarea>
      <div class="row" style="margin-top:10px">
        <button id="submit">导入</button>
        <span class="status" id="status"></span>
      </div>
    </div>
  </div>
</div>

<script>
(function () {
  "use strict";
  var state = "__STATE__";
  var statusEl = document.getElementById("status");

  function say(text, kind) {
    statusEl.textContent = text || "";
    statusEl.className = "status " + (kind || "");
  }
  function post(path, body) {
    return fetch(path, { method: "POST", headers: { "Content-Type": "application/json", "X-Login-State": state }, body: JSON.stringify(body || {}) })
      .then(function (response) {
        return response.text().then(function (text) {
          var data = null;
          try { data = JSON.parse(text); } catch (e) { data = { error: text }; }
          if (!response.ok) { throw new Error((data && data.error) || ("HTTP " + response.status)); }
          return data;
        });
      });
  }
  function esc(value) {
    return String(value == null ? "" : value).replace(/[&<>"']/g, function (c) {
      return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c];
    });
  }
  function finish(email) {
    document.body.classList.add("done-ok");
    document.getElementById("doneText").textContent = "已导入 " + (email || "Miora 账号") + "，可以关闭此页面。";
  }

  document.getElementById("scan").addEventListener("click", function () {
    var btn = this;
    btn.disabled = true;
    document.getElementById("scanState").textContent = "正在扫描…";
    post("/api/browser-scan", {}).then(function (data) {
      var list = document.getElementById("candidates");
      if (!data.ok || !data.candidates || !data.candidates.length) {
        list.innerHTML = '<div class="hint err">' + esc((data && data.error) || "没有找到登录态") + "</div>";
        return;
      }
      list.innerHTML = data.candidates.map(function (c, index) {
        return '<div class="item"><div><div>' + esc(c.email || c.user_id || "未知账号") + "</div>" +
          '<div class="hint mono">' + esc(c.browser) + " · " + esc(c.profile) + "</div></div>" +
          '<div class="row">' + (c.valid ? '<span class="pill ok">有效</span>' : '<span class="pill err">不可用</span>') +
          '<button class="ghost" data-index="' + index + '">导入</button></div></div>';
      }).join("");
      list.dataset.payload = JSON.stringify(data.candidates);
      document.getElementById("scanState").textContent = "共 " + data.candidates.length + " 条";
    }).catch(function (error) {
      document.getElementById("scanState").textContent = "扫描失败：" + error.message;
    }).finally(function () { btn.disabled = false; });
  });

  document.getElementById("candidates").addEventListener("click", function (event) {
    var btn = event.target.closest("button[data-index]");
    if (!btn) { return; }
    var payload = JSON.parse(document.getElementById("candidates").dataset.payload || "[]");
    var candidate = payload[Number(btn.dataset.index)];
    if (!candidate) { return; }
    btn.disabled = true;
    post("/api/submit", { candidate_index: Number(btn.dataset.index) }).then(function (data) {
      finish(data.email);
    }).catch(function (error) {
      say(error.message, "err");
      btn.disabled = false;
    });
  });

  document.getElementById("submit").addEventListener("click", function () {
    var token = document.getElementById("token").value.trim();
    var refresh = document.getElementById("refresh").value.trim();
    if (!token) { say("请先粘贴 authToken", "err"); return; }
    say("正在验证登录态…");
    post("/api/submit", { auth_token: token, refresh_token: refresh }).then(function (data) {
      finish(data.email);
    }).catch(function (error) { say(error.message, "err"); });
  });
})();
</script>
</body>
</html>
`
