# AI 图片生成网站

用户输入图片描述提示词后，网站会请求 Go 服务端 `/api/generate`，再由服务端携带 `OPENAI_API_KEY` 调用你的图片生成接口。Key 不会暴露到浏览器。

后端内置了适合多人同时使用的基础保护：

- 有界队列：避免 100 人同时点击时全部直接打到上游接口
- 上游并发上限：通过 `MAX_IMAGE_CONCURRENCY` 控制真实生图并发
- 注册登录：未登录用户不能调用生图接口
- 用户额度：默认每个用户 1 小时最多 6 次生图
- 单 IP 限流：降低被刷爆 API 额度的风险
- 长请求心跳：减少 nginx / 浏览器等待期间的 504
- 上游响应大小上限：避免 base64 大图导致内存峰值失控

## 本地运行

1. 复制环境变量示例：

```bash
cp .env.example .env
```

2. 修改 `.env`：

```bash
OPENAI_API_KEY=你的key
OPENAI_IMAGE_URL=你的图片生成完整URL
OPENAI_IMAGE_MODEL=gpt-image-2
SESSION_SECRET=请换成一段很长的随机字符串
```

如果你的供应商给的是 base url，而不是完整接口 URL，可以使用：

```bash
OPENAI_BASE_URL=https://api.openai.com/v1
```

3. 启动：

```bash
npm start
```

打开 `http://localhost:3000`。

也可以直接使用 Go：

```bash
go run .
```

## 服务器部署

服务器需要 Go 1.22 或更高版本。推荐先编译二进制再部署：

```bash
go build -o bin/gpt-image-site .
PORT=3000 ./bin/gpt-image-site
```

生产环境建议用进程管理器托管，例如：

```bash
pm2 start ./bin/gpt-image-site --name gpt-image-site
```

再用 Nginx 反向代理到 `http://127.0.0.1:3000`，并配置 HTTPS。

## 环境变量

- `PORT`：服务端口，默认 `3000`
- `OPENAI_API_KEY`：你的图片生成接口 key
- `OPENAI_IMAGE_URL`：完整图片生成接口 URL，优先级最高
- `OPENAI_BASE_URL`：当没有设置 `OPENAI_IMAGE_URL` 时使用，服务端会拼接 `/images/generations`
- `OPENAI_IMAGE_MODEL`：模型名，默认代码使用 `gpt-image-2`
- `IMAGE_REQUEST_TIMEOUT_MS`：服务端等待图片接口的最长时间，默认 `240000`（4 分钟）
- `RESPONSE_HEARTBEAT_MS`：生成过程中写给浏览器/反向代理的心跳间隔，默认 `15000`
- `MAX_IMAGE_CONCURRENCY`：同时调用上游生图接口的数量，默认 `6`
- `MAX_IMAGE_QUEUE`：等待执行的最大排队数量，默认 `120`
- `QUEUE_TIMEOUT_MS`：请求在队列中等待的最长时间，默认 `180000`（3 分钟）
- `RATE_LIMIT_WINDOW_MS`：单 IP 限流窗口，默认 `60000`
- `RATE_LIMIT_MAX_REQUESTS`：单 IP 在限流窗口内最多请求次数，默认 `4`
- `OPENAI_RESPONSE_MAX_BYTES`：上游 JSON 响应体最大字节数，默认 `67108864`（64MB）
- `SESSION_SECRET`：登录会话签名密钥，生产环境必须固定配置为强随机字符串
- `AUTH_DATA_FILE`：用户账号和用量数据文件，默认 `./data/auth.json`
- `USER_IMAGE_LIMIT`：每个用户在窗口期内最多生图次数，默认 `6`
- `USER_IMAGE_WINDOW_MS`：用户额度窗口，默认 `3600000`（1 小时）

## 账号与额度

用户通过前端注册/登录后，服务端会设置 `HttpOnly` Cookie。`/api/generate` 会先验证登录状态，再检查用户额度，默认每个账号 1 小时最多 6 次。

单机部署时，账号和用量数据保存在 `AUTH_DATA_FILE` 指向的 JSON 文件中。请注意：

- 不要把 `data/auth.json` 提交到 Git
- 部署服务器时要备份并持久化 `data/` 目录
- `SESSION_SECRET` 重启前后要保持不变，否则已有登录 Cookie 会失效
- 如果未来要多台服务器横向扩容，需要把用户和用量存储换成 Postgres/Redis 这类共享存储

## 100 人左右使用建议

先把 `MAX_IMAGE_CONCURRENCY` 设置为你的上游图片接口能稳定承受的值，而不是服务器 CPU 能承受的值。图片生成的瓶颈通常是上游 API 的每分钟图片数、请求数和响应时间。

建议初始配置：

```bash
MAX_IMAGE_CONCURRENCY=6
MAX_IMAGE_QUEUE=120
QUEUE_TIMEOUT_MS=180000
RATE_LIMIT_MAX_REQUESTS=4
```

如果上游经常返回 429，降低 `MAX_IMAGE_CONCURRENCY`。如果排队过长，应该提升上游额度或增加账号/项目配额，而不是盲目提高并发。

## 504 超时说明

图片生成经常会超过 nginx 默认的 60 秒等待时间。服务端会在排队和调用上游期间发送 JSON 空白心跳，避免大多数反向代理提前返回 HTML 504 页面。

如果服务器仍然出现 504，请检查 nginx 站点配置里的超时是否过短，例如：

```nginx
location / {
  proxy_pass http://127.0.0.1:3000;
  proxy_http_version 1.1;
  proxy_set_header Host $host;
  proxy_set_header X-Real-IP $remote_addr;
  proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
  proxy_set_header X-Forwarded-Proto $scheme;
  proxy_read_timeout 300s;
  proxy_send_timeout 300s;
  proxy_buffering off;
}
```

## 接口返回格式

当前服务端兼容常见的 OpenAI Images API 返回：

```json
{
  "data": [
    {
      "b64_json": "..."
    }
  ]
}
```

也兼容返回图片 URL 的格式：

```json
{
  "data": [
    {
      "url": "https://..."
    }
  ]
}
```
