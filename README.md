# AI 图片生成网站

用户输入图片描述提示词后，网站会请求服务端 `/api/generate`，再由服务端携带 `OPENAI_API_KEY` 调用你的图片生成接口。Key 不会暴露到浏览器。

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

## 服务器部署

服务器需要 Node.js 20 或更高版本。

```bash
npm start
```

生产环境建议用进程管理器托管，例如：

```bash
pm2 start server.js --name gpt-image-site
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

## 504 超时说明

图片生成经常会超过 nginx 默认的 60 秒等待时间。服务端已经在调用上游图片接口时发送 JSON 空白心跳，避免大多数反向代理提前返回 HTML 504 页面。

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
