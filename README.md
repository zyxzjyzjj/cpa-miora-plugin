# miora-provider · Miora × CLIProxyAPI 插件

把 [Miora](https://miora.design)（图像 / 视频 / 3D 创作平台）的生成能力**反代成 OpenAI 兼容接口**，以 [CLIProxyAPI](https://help.router-for.me/cn/plugin/development.html)（下称 CPA）原生动态库插件的形式运行。

- 插件 ID：`miora-provider`
- 版本：`0.1.0`
- 平台：Windows `amd64`（本仓库现成产物），Linux / macOS 可用同一份源码编译

---

## 1. 它做什么，不做什么

### 做

| 能力 | 说明 |
| --- | --- |
| `auth_provider` | 登录态导入与轮换：一键读取本机 Edge / Chrome 里已登录的 miora.design 会话，或手动粘贴 token；插件自行负责 `refresh_token` 轮换并写回 auth 文件 |
| `model_provider` | 拉取 Miora 官方媒体模型目录（免费档 42 个：24 图像 / 12 视频 / 6 个 3D），映射成 CPA 模型，附能力、比例、分辨率、参考图上限 |
| `executor` | 把 `/v1/chat/completions` 请求翻译成 Miora 的异步生成任务：提交 → 轮询 → 签名产物 → 以 Markdown 媒体 + `miora` 结构化扩展返回；支持流式进度 |
| `quota_provider` | 用 `/api/ai/quota/credit` 的真实余额填充 CPA 的额度视图 |
| `usage_plugin` | 记录每次请求的账号 / 模型 / 耗时 / 扣费，进面板日志 |
| `scheduler` | 多账号轮转，降低单账号并发与频控压力 |
| `management_api` | 账号、生成台、产物、模型、日志、设置、自检，一套中文控制台 |

### 不做

**没有 chat 反代。** Miora 的 LLM 是**有状态云端 Agent**：每次请求要 `runtimeId`（绑 projectId）+ `acpLink` 临时 token，工具调用需要 Socket.IO 上的人工审批（`TOOL_CONFIRMATION_PENDING`），协议是 ACP JSON-RPC。OpenAI 的 chat 协议是无状态的，二者不存在可靠的转换路径——强行做只会得到一个随时卡死的假 chat。

所以本插件反代的是 Miora **真正开放、无状态、可自动化**的部分：媒体生成。对客户端而言一样是标准 `/v1/chat/completions`，只是回复里给的是图片 / 视频 / 3D 文件而不是文字。

---

## 2. 安装

### 2.1 编译

插件 ABI 是 C ABI，**CGO 必需**。

```bash
# Windows（MinGW-w64 gcc 或 LLVM-MinGW 的 x86_64-w64-mingw32-gcc）
build.bat

# Linux / macOS
bash build.sh
```

等价的手工命令：

```bash
CGO_ENABLED=1 go build -trimpath -buildmode=c-shared -ldflags "-s -w" \
  -o plugins/windows/amd64/miora-provider.dll .
```

产物导出四个符号：`cliproxy_plugin_init`、`cliproxyPluginCall`、`cliproxyPluginFree`、`cliproxyPluginShutdown`。

### 2.2 放置与启用

```
<CPA 工作目录>/
├── cli-proxy-api(.exe)
├── config.yaml
├── plugins/windows/amd64/miora-provider.dll     # ← 放这里
└── auths/                                        # ← auth-dir
```

`config.yaml` 追加：

```yaml
plugins:
  enabled: true
  dir: "plugins"
  configs:
    miora-provider:
      enabled: true
      priority: 1
      base_url: "https://miora.design"
      poll_interval_seconds: 2
      task_timeout_seconds: 300
      max_concurrent_tasks: 2
```

重启 CPA，`GET /v0/management/plugins` 里应看到：

```json
{ "id": "miora-provider", "registered": true, "effective_enabled": true,
  "supports_oauth": true, "supports_quota": true,
  "menus": [{ "path": "/v0/resource/plugins/miora-provider/panel", "menu": "Miora" }] }
```

`X-CPA-SUPPORT-PLUGIN: 1` 若为 `0`，说明该 CPA 二进制没有带插件支持，换官方 release 即可。

---

## 3. 登录态（三种任一）

Miora 海外站只支持 Google / GitHub OAuth，凭证落在浏览器 `localStorage`：

| 键 | 说明 |
| --- | --- |
| `authToken` | RS256 JWT，`iss=ardot.ai`，15 天有效 |
| `refreshToken` | 不透明串，**一次性轮换**：用一次就换新的 |
| `miora__user_info` | `userId` / `email` / `plan` |

### 方式 A · 面板一键导入（推荐）

打开 `http://<CPA>/v0/resource/plugins/miora-provider/panel` → **账号** → **扫描本机浏览器**。

插件只读 `Local Storage/leveldb` 里 ASCII 内联的 token 与用户信息，不碰密码、cookie、浏览器凭据库；扫到的每条都会立刻向上游 `/api/ai/user/profile` 验证，失效的会标红。

### 方式 B · 粘贴

DevTools → Application → Local Storage → `https://miora.design`，复制 `authToken`（可选 `refreshToken`），贴进面板「手动粘贴」。支持三种格式：浏览器导出 JSON、`{"auth_token":...,"refresh_token":...}`、裸 JWT。

### 方式 C · CPA 原生「添加账号」

CPA 点添加账号选 `miora-provider`，宿主会调 `auth.login.start`：插件在本机回环地址起一个一次性登录页（10 分钟有效），页面里同样可以「扫描浏览器」或粘贴 token，完成后 `auth.login.poll` 返回凭证。

### token 轮换（重要）

`refreshToken` 是**一次性**的：每次刷新都会作废旧的。插件：

- 在 `auth.refresh` 里刷新并把新 token 写回 `AuthData.StorageJSON`；
- 后台每 10 分钟巡检一次，到期前 `auto_refresh_hours`（默认 24h）主动轮换，并调用 `host.auth.save` 落盘；
- 刷新失败不会把账号标记为永久失效，只会在面板提示「重新导入」。

> 若你手动在别处（例如 miora2api）用掉过同一个 `refreshToken`，这里会拿到 400「无效的刷新令牌」。重新从浏览器导入即可，浏览器自己会拿到新的一对。

---

## 4. 客户端用法

### 4.1 最简

```bash
curl http://127.0.0.1:8317/v1/chat/completions \
  -H "Authorization: Bearer <CPA API KEY>" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "hy-image-3.5-f",
    "messages": [{"role":"user","content":"a green pear on a wooden table, product photo"}]
  }'
```

响应（实测 15.8s，1024×1024 PNG）：

```json
{
  "choices": [{
    "index": 0,
    "message": {
      "role": "assistant",
      "content": "![Miora image](https://sign.miorausercontent.com/file/.../xPS_qpVaBk0zy9lT.png?...)\n\n> hy-image-3.5-f · 1:1 · 1.5K · 15.8s\n> 链接 1 小时内有效，可用面板按 fileKey 重新签名（不会重复扣费）。"
    },
    "finish_reason": "stop"
  }],
  "miora": {
    "task_ids": ["fdd4cc15-1109-4997-994a-68a25e3ac0ed"],
    "files": [{"file_key": "file/741997187129584/2026/10/30/.../xPS_qpVaBk0zy9lT.png", "url": "https://...", "type": "image"}],
    "credits": 0
  }
}
```

`miora` 是插件加的扩展字段：网页端 / 脚本可以直接拿 `file_key` 重签 URL，不必解析 Markdown。

### 4.2 自动选模型

把 `model` 填成 `miora-auto`（或 `auto`），插件按 `family` 选默认模型：

```json
{"model": "miora-auto", "family": "video", "messages": [{"role":"user","content":"waves at sunset"}]}
```

### 4.3 参数

| 字段 | 作用 | 备注 |
| --- | --- | --- |
| `aspect_ratio` | `1:1` / `16:9` / `9:16` … | 会按模型 `capabilities.aspectRatios` 就近收敛 |
| `size` | `1024x1024` | 未给 `aspect_ratio` / `resolution` 时由它推导 |
| `resolution` | `1K` / `1.5K` / `2K` / `480P` / `768P` | 超出模型允许范围时回落到默认 |
| `duration` / `seconds` | 视频秒数 | 按模型 `secondsRange` 收敛 |
| `n` | 1–4 | 并发生成多份 |
| `family` | `image` / `video` / `3d` | 仅 `miora-auto` 用 |
| `miora` | `{...}` 对象 | 上述字段的覆盖写法，优先级更高 |

提示词里出现 `16:9` 这样的比例也会被识别（仅当请求没显式给比例）。

### 4.4 图生（参考图）

图像模型最多 20 张参考图，视频模型吃「首帧」，3D 也支持图生。两种给法：

1. **已有 Miora 文件**（首选，零上传）：`image_url` 填 `miora://file/...` 或 `file/...`；
2. **任意图片**：`data:` URL 或 `http(s)` URL，需要打开 `upload_reference_images: true`（实验特性，走腾讯 COS 直传，见 §7）。

```json
{
  "model": "gem-3.1",
  "messages": [{
    "role": "user",
    "content": [
      {"type": "text", "text": "把这只猫画成赛博朋克风格"},
      {"type": "image_url", "image_url": {"url": "miora://file/741997.../cat.png"}}
    ]
  }]
}
```

### 4.5 流式

`stream: true` 时先给 `role`，再给进度文本（`渲染中…`），最后给最终 Markdown，宿主补 `data: [DONE]`。

### 4.6 客户端兼容

CPA 会把 Anthropic / Gemini / Responses 客户端的请求翻译成本插件声明的 `chat-completions` 输入格式再送进来，回复原路翻译回去。因此 Claude Code、Cherry Studio、OpenAI SDK 都能用同一套模型——只是拿到的是媒体而不是文字。

---

## 5. 面板

`/v0/resource/plugins/miora-provider/panel`（CPA 主面板菜单里也叫 **Miora**）

- **概览**：账号数、模型数、可用额度、最近生成，插件能力标签
- **账号**：浏览器一键导入、手动粘贴、账号卡片（邮箱 / 有效期 / 余额条 / 测试 / 轮换 token / 删除）
- **生成台**：图像 / 视频 / 3D 切换，模型与参数按能力自动收窄，`Ctrl/⌘ + Enter` 直接生成，任务实时进度
- **产物**：网格预览，图片灯箱、视频播放、3D 下载，一键复制 / 重签 URL
- **模型**：42 个模型的能力表（比例 / 分辨率 / 参考图 / 可用性），支持过滤与重新拉取
- **日志**：请求记录（状态 / 额度 / 耗时 / 提示词 / 错误）
- **设置**：参数写回 CPA 的 `plugins.configs`，保存后触发 `plugin.reconfigure`；附带自检（目录 / 登录态 / 额度 / 上游调用轨迹）

管理密钥获取方式与 CPA 生态一致：同源 iframe 打开时自动读主面板 `localStorage` 的 `cli-proxy-auth`（AES 前缀 `enc::v1::` 解混淆），也支持 `?key=` 一次性参数（用完即从地址栏抹掉）或手动输入。

---

## 6. 管理 API

全部挂在 `/v0/management/miora-provider/`，需要管理密钥。

| 方法 | 路径 | 作用 |
| --- | --- | --- |
| GET | `/status` | 版本、上游地址、模型数、账号数、目录状态 |
| GET | `/accounts` | 账号列表（`?live=1` 现场拉额度） |
| POST | `/accounts/scan` | 扫描本机浏览器登录态并验证 |
| POST | `/accounts/import` | 粘贴导入 `{raw}` / `{auth_token, refresh_token}` |
| POST | `/accounts/browser-import` | 导入扫描结果 |
| POST | `/accounts/test` | `{auth_index}` 验证账号 |
| POST | `/accounts/refresh` | `{auth_index}` 立即轮换并写回 |
| POST | `/accounts/delete` | `{auth_index}` 删除 auth 文件 |
| GET | `/models` | 模型目录 + 默认模型 |
| POST | `/models/refresh` | 强制重新拉取 |
| POST | `/generate` | 面板生成 `{prompt, model, media, aspect_ratio, resolution, duration}` |
| GET | `/tasks` | 最近任务 `?limit=` |
| GET | `/media` | 产物列表 |
| POST | `/media/sign` | `{file_key}` 重签 URL（1 小时） |
| GET | `/logs` | 请求日志 |
| GET | `/settings` | 当前配置 |
| GET | `/diagnostics` | 自检 + 上游调用轨迹 |

---

## 7. 配置项

| 字段 | 默认 | 说明 |
| --- | --- | --- |
| `base_url` | `https://miora.design` | 上游站点 |
| `language` | `zh-CN` | 目录语言 |
| `model_prefix` | 空 | 对外模型 ID 前缀，如 `miora/` |
| `include_models` / `exclude_models` | 空 | 模型白 / 黑名单 |
| `models` | 空 | 离线静态模型表（`id/upstream/media/label`） |
| `default_image_model` / `default_video_model` / `default_3d_model` | 空 | `miora-auto` 与未知模型的兜底 |
| `default_video_seconds` | `5` | 视频默认时长 |
| `poll_interval_seconds` | `4` | 任务轮询间隔 |
| `task_timeout_seconds` | `420` | 单次生成最长等待 |
| `max_concurrent_tasks` | `4` | 并发等待上限（1–64） |
| `reserve_credits` | `0` | 低于该余额直接拒绝，0 关闭 |
| `upload_reference_images` | `false` | 允许 `data:`/`http(s)` 参考图上传（COS 直传，实验） |
| `enable_browser_import` | `true` | 允许面板读本机浏览器 |
| `browser_profiles` | 空 | 限定 `edge:Default` 这类 profile |
| `include_watermark` | `false` | 要水印版本 |
| `response_footer` | `true` | 回复末尾附生成信息 |
| `catalog_ttl_minutes` | `30` | 目录缓存 |
| `auto_refresh_hours` | `24` | 提前多久轮换 token |
| `event_log_size` | `200` | 日志条数 |
| `proxy_url` | 空 | 上游代理，`direct://` 直连 |
| `use_host_http` | `true` | 走宿主 HTTP 桥；被网关拦截会自动回退直连 |

---

## 8. 故障排查

| 现象 | 原因与处理 |
| --- | --- |
| `405 Not Allowed`（`nginx/1.19.10` 的 HTML） | 历史版本把 `base_url` 塞进 auth 的 `Attributes`，CPA 会据此改用内置 OpenAI 兼容链路直连上游而绕过插件执行器。**已修复**：`Attributes` 现在留空；若你的 auth 文件是旧版插件写的，重新导入一次即可 |
| `401` / `credential_invalid` | 登录态过期或被别的工具轮换掉：面板「扫描本机浏览器」重新导入 |
| 额度显示 0，但生成成功 | `/api/ai/quota/credit` 对免费档常返回 0，平台用另一套免费池计费；以任务是否成功为准，面板日志里有真实扣费字段 |
| 任务超时 | 视频 / 3D 本来就要 60–180s，调大 `task_timeout_seconds`；任务在上游仍会继续，稍后用面板按 `file_key` 重签 |
| 参考图报「需要开启 upload_reference_images」 | COS 直传默认关闭；或改成传 `miora://file/...` 的已有文件 |
| 面板打不开 / 401 | 管理密钥没带：从 CPA 主面板进入，或 `?key=<管理密钥>` |
| 模型列表为空 | `GET /models` 看 `error`；目录接口免登录，通常是被代理挡了，设 `proxy_url` 或 `direct://` |

---

## 9. 实测数据（2026-10，含本仓库端到端验证）

| 项 | 结果 |
| --- | --- |
| 模型目录 | 42 个（图像 24 / 视频 12 / 3D 6），`userTier=free` |
| 文生图 `hy-image-3.5-f` | 1:1 / 1.5K，**15.8s**，PNG 2.3MB |
| 流式 | 13.8s，`role → 进度 → 内容 → [DONE]` |
| 浏览器导入 | Edge `Default` 命中，另一条失效 token 被正确标红 |
| 管理路由 | 账号 / 模型 / 生成 / 任务 / 产物 / 重签 / 设置 / 自检 全通 |
| 参考图上限 | 图像 20（`hy-image-3.5-f`）、14（`gem-3.0/3.1`）、10（`seedream-5.0-pro`）等，随模型不同 |

上游协议细节（提交字段、双层信封、video/3D 走 `media-generate/progress` 等）与 [Zhengyuuuui/miora2api](https://github.com/Zhengyuuuui/miora2api) 的实测结论一致，本插件在其基础上补齐了 CPA 侧的插件化、账号轮换与面板。

---

## 10. 开发

```
main.go            C ABI、RPC 分发、宿主回调、日志
config.go          配置解析与默认值
miora.go           上游客户端：目录、账号、提交、轮询、签名
catalog.go         模型目录缓存
models.go          模型注册与能力声明
auth.go            凭证、auth.parse/login/refresh、后台 token 巡检
browser_import.go  Edge/Chrome Local Storage 解析
executor.go        chat-completions → 生成任务 → OpenAI 响应/流
tasks.go           任务存储、并发闸门、结果渲染
management.go      管理路由
panel.go           控制台单页（内联样式与脚本）
login_page.go      回环登录页
quota.go usage.go scheduler.go cos.go trace.go
plugin_test.go     21 项单元测试（解析、目录映射、生成流水线、登录回环、面板不泄密）
```

```bash
go test ./...      # 需要 CGO 与 C 编译器
go vet ./...
```

测试覆盖：配置解析与默认值、凭证三种格式、浏览器 leveldb 提取（含忽略异源）、chat payload 解析、模型 / 媒体类型推断、比例与时长收敛、任务流水线（httptest 假上游）、失败映射、`auth.parse` 两种来源、路由后缀、状态接口不泄露 token、面板与登录页占位符。

---

## 11. 说明

本项目用于互操作性研究与本地集成。Miora 的协议、模型与额度归其平台方所有；请遵守其服务条款，不要用于绕过计费或批量滥用。插件不存储、不上传你的浏览器数据：登录态只在内存与 CPA 的 auth 文件之间流动。
