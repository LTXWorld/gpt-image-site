import http from "node:http";
import { readFile } from "node:fs/promises";
import path from "node:path";
import { fileURLToPath } from "node:url";

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const publicDir = path.join(__dirname, "public");

await loadEnvFile(path.join(__dirname, ".env"));

const PORT = Number(process.env.PORT || 3000);
const DEFAULT_IMAGE_URL = "https://api.openai.com/v1/images/generations";
const MAX_BODY_BYTES = 96 * 1024;

const mimeTypes = {
  ".html": "text/html; charset=utf-8",
  ".css": "text/css; charset=utf-8",
  ".js": "text/javascript; charset=utf-8",
  ".json": "application/json; charset=utf-8",
  ".png": "image/png",
  ".jpg": "image/jpeg",
  ".jpeg": "image/jpeg",
  ".webp": "image/webp",
  ".svg": "image/svg+xml"
};

const allowedSizes = new Set(["auto", "1024x1024", "1536x1024", "1024x1536"]);
const allowedQualities = new Set(["auto", "low", "medium", "high"]);
const allowedFormats = new Set(["png", "jpeg", "webp"]);
const allowedBackgrounds = new Set(["auto", "opaque", "transparent"]);

const server = http.createServer(async (req, res) => {
  try {
    const url = new URL(req.url || "/", `http://${req.headers.host || "localhost"}`);

    if (req.method === "POST" && url.pathname === "/api/generate") {
      await handleGenerate(req, res);
      return;
    }

    if (req.method === "GET") {
      await serveStatic(url.pathname, res);
      return;
    }

    sendJson(res, 405, { error: "Method not allowed" });
  } catch (error) {
    console.error(error);
    sendJson(res, 500, { error: "服务器内部错误，请稍后再试。" });
  }
});

server.listen(PORT, () => {
  console.log(`Image generation site running at http://localhost:${PORT}`);
});

async function handleGenerate(req, res) {
  const apiKey = process.env.OPENAI_API_KEY;

  if (!apiKey) {
    sendJson(res, 500, {
      error: "服务端还没有配置 OPENAI_API_KEY。请在 .env 或服务器环境变量中设置它。"
    });
    return;
  }

  let body;
  try {
    body = JSON.parse(await readRequestBody(req));
  } catch {
    sendJson(res, 400, { error: "请求体不是有效的 JSON。" });
    return;
  }

  const prompt = String(body.prompt || "").trim();
  const size = normalizeOption(body.size, allowedSizes, "1024x1024");
  const quality = normalizeOption(body.quality, allowedQualities, "auto");
  const format = normalizeOption(body.format, allowedFormats, "png");
  const background = normalizeOption(body.background, allowedBackgrounds, "auto");

  if (!prompt) {
    sendJson(res, 400, { error: "请输入图片描述提示词。" });
    return;
  }

  if (prompt.length > 32000) {
    sendJson(res, 400, { error: "提示词过长，请控制在 32000 个字符以内。" });
    return;
  }

  if (background === "transparent" && format === "jpeg") {
    sendJson(res, 400, { error: "透明背景需要选择 PNG 或 WebP 格式。" });
    return;
  }

  const upstreamUrl = getImageEndpoint();
  const payload = {
    model: process.env.OPENAI_IMAGE_MODEL || "gpt-image-2",
    prompt,
    n: 1,
    size,
    quality,
    output_format: format,
    background
  };

  const upstreamResponse = await fetch(upstreamUrl, {
    method: "POST",
    headers: {
      Authorization: `Bearer ${apiKey}`,
      "Content-Type": "application/json"
    },
    body: JSON.stringify(payload)
  });

  const responseText = await upstreamResponse.text();
  const responseData = parseJson(responseText);

  if (!upstreamResponse.ok) {
    sendJson(res, upstreamResponse.status, {
      error: extractUpstreamError(responseData, responseText)
    });
    return;
  }

  const image = normalizeImageResult(responseData, format);

  if (!image) {
    sendJson(res, 502, {
      error: "图片生成接口没有返回可识别的图片数据。请检查上游接口返回格式。"
    });
    return;
  }

  sendJson(res, 200, {
    image,
    revisedPrompt: responseData?.data?.[0]?.revised_prompt || responseData?.revised_prompt || "",
    model: payload.model
  });
}

async function serveStatic(requestPath, res) {
  const cleanPath = decodeURIComponent(requestPath.split("?")[0]);
  const filePath = cleanPath === "/" ? "/index.html" : cleanPath;
  const absolutePath = path.resolve(publicDir, `.${filePath}`);

  if (!absolutePath.startsWith(publicDir)) {
    sendText(res, 403, "Forbidden");
    return;
  }

  try {
    const content = await readFile(absolutePath);
    const ext = path.extname(absolutePath).toLowerCase();
    res.writeHead(200, {
      "Content-Type": mimeTypes[ext] || "application/octet-stream",
      "Cache-Control": ext === ".html" ? "no-cache" : "public, max-age=3600"
    });
    res.end(content);
  } catch {
    const fallback = await readFile(path.join(publicDir, "index.html"));
    res.writeHead(200, { "Content-Type": "text/html; charset=utf-8" });
    res.end(fallback);
  }
}

function readRequestBody(req) {
  return new Promise((resolve, reject) => {
    let size = 0;
    const chunks = [];

    req.on("data", (chunk) => {
      size += chunk.length;
      if (size > MAX_BODY_BYTES) {
        reject(new Error("Request body too large"));
        req.destroy();
        return;
      }
      chunks.push(chunk);
    });

    req.on("end", () => resolve(Buffer.concat(chunks).toString("utf8")));
    req.on("error", reject);
  });
}

function normalizeOption(value, allowed, fallback) {
  const next = String(value || fallback).trim();
  return allowed.has(next) ? next : fallback;
}

function getImageEndpoint() {
  if (process.env.OPENAI_IMAGE_URL) {
    return process.env.OPENAI_IMAGE_URL;
  }

  if (process.env.OPENAI_BASE_URL) {
    return `${process.env.OPENAI_BASE_URL.replace(/\/+$/, "")}/images/generations`;
  }

  return DEFAULT_IMAGE_URL;
}

function normalizeImageResult(data, requestedFormat) {
  const item = data?.data?.[0] || data?.output?.find?.((entry) => entry?.result || entry?.b64_json);
  const base64 = item?.b64_json || item?.result || data?.b64_json || data?.image_base64;
  const url = item?.url || data?.url || data?.image_url;

  if (typeof url === "string" && url.length > 0) {
    return url;
  }

  if (typeof base64 === "string" && base64.length > 0) {
    if (base64.startsWith("data:image/")) {
      return base64;
    }

    const mime = requestedFormat === "jpeg" ? "image/jpeg" : `image/${requestedFormat}`;
    return `data:${mime};base64,${base64}`;
  }

  return "";
}

function extractUpstreamError(data, fallbackText) {
  const message =
    data?.error?.message ||
    data?.message ||
    (typeof fallbackText === "string" && fallbackText.trim());

  return message || "图片生成接口调用失败。";
}

function parseJson(value) {
  try {
    return JSON.parse(value);
  } catch {
    return null;
  }
}

function sendJson(res, status, payload) {
  res.writeHead(status, { "Content-Type": "application/json; charset=utf-8" });
  res.end(JSON.stringify(payload));
}

function sendText(res, status, text) {
  res.writeHead(status, { "Content-Type": "text/plain; charset=utf-8" });
  res.end(text);
}

async function loadEnvFile(filePath) {
  try {
    const content = await readFile(filePath, "utf8");

    for (const rawLine of content.split(/\r?\n/)) {
      const line = rawLine.trim();
      if (!line || line.startsWith("#")) continue;

      const separator = line.indexOf("=");
      if (separator === -1) continue;

      const key = line.slice(0, separator).trim();
      let value = line.slice(separator + 1).trim();

      if (
        (value.startsWith('"') && value.endsWith('"')) ||
        (value.startsWith("'") && value.endsWith("'"))
      ) {
        value = value.slice(1, -1);
      }

      if (key && process.env[key] === undefined) {
        process.env[key] = value;
      }
    }
  } catch {
    // .env is optional in production because hosts usually inject env vars directly.
  }
}
