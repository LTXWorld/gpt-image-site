const authPanel = document.querySelector("#authPanel");
const authForm = document.querySelector("#authForm");
const authMessage = document.querySelector("#authMessage");
const usernameInput = document.querySelector("#username");
const passwordInput = document.querySelector("#password");
const loginButton = document.querySelector("#loginButton");
const registerButton = document.querySelector("#registerButton");
const workspace = document.querySelector("#workspace");
const form = document.querySelector("#generateForm");
const promptInput = document.querySelector("#prompt");
const submitButton = document.querySelector("#submitButton");
const statusPill = document.querySelector("#statusPill");
const userLabel = document.querySelector("#userLabel");
const quotaLabel = document.querySelector("#quotaLabel");
const quotaResetText = document.querySelector("#quotaResetText");
const logoutButton = document.querySelector("#logoutButton");
const emptyState = document.querySelector("#emptyState");
const loadingState = document.querySelector("#loadingState");
const resultImage = document.querySelector("#resultImage");
const errorMessage = document.querySelector("#errorMessage");
const revisedPrompt = document.querySelector("#revisedPrompt");
const downloadButton = document.querySelector("#downloadButton");
const formatSelect = document.querySelector("#format");
const backgroundSelect = document.querySelector("#background");

let lastObjectUrl = "";
let currentUser = null;
let currentQuota = null;

initAuth();

authForm.addEventListener("submit", async (event) => {
  event.preventDefault();
  await submitAuth("/api/login");
});

registerButton.addEventListener("click", async () => {
  await submitAuth("/api/register");
});

logoutButton.addEventListener("click", async () => {
  try {
    await fetch("/api/logout", { method: "POST" });
  } finally {
    currentUser = null;
    currentQuota = null;
    showAuth();
  }
});

form.addEventListener("submit", async (event) => {
  event.preventDefault();

  const payload = Object.fromEntries(new FormData(form).entries());
  setLoading(true);
  clearMessage();

  try {
    const response = await fetch("/api/generate", {
      method: "POST",
      headers: {
        "Content-Type": "application/json"
      },
      body: JSON.stringify(payload)
    });

    const data = await readResponseData(response);

    if (!response.ok || data.ok === false || data.error) {
      if (response.status === 401) {
        currentUser = null;
        currentQuota = null;
        showAuth();
      }
      if (data.quota) {
        updateQuota(data.quota);
      }
      throw new Error(data.error || "图片生成失败。");
    }

    if (data.quota) {
      updateQuota(data.quota);
    }
    showImage(data.image, payload.format);
    showRevisedPrompt(data.revisedPrompt);
    statusPill.textContent = "已生成";
  } catch (error) {
    showError(error.message || "图片生成失败，请稍后再试。");
    statusPill.textContent = "生成失败";
  } finally {
    setLoading(false);
  }
});

async function initAuth() {
  try {
    const response = await fetch("/api/me");
    const data = await readResponseData(response);

    if (!response.ok || data.authenticated === false) {
      showAuth();
      return;
    }

    setAuthenticated(data.user, data.quota);
  } catch {
    showAuth();
  }
}

async function submitAuth(endpoint) {
  const payload = Object.fromEntries(new FormData(authForm).entries());
  setAuthLoading(true);
  clearAuthMessage();

  try {
    const response = await fetch(endpoint, {
      method: "POST",
      headers: {
        "Content-Type": "application/json"
      },
      body: JSON.stringify(payload)
    });
    const data = await readResponseData(response);

    if (!response.ok || data.ok === false || data.error) {
      throw new Error(data.error || "登录失败。");
    }

    passwordInput.value = "";
    setAuthenticated(data.user, data.quota);
  } catch (error) {
    showAuthError(error.message || "登录失败，请稍后再试。");
  } finally {
    setAuthLoading(false);
  }
}

function setAuthenticated(user, quota) {
  currentUser = user;
  currentQuota = quota;
  authPanel.classList.add("is-hidden");
  workspace.classList.remove("is-hidden");
  userLabel.textContent = user?.username ? `账号：${user.username}` : "已登录";
  updateQuota(quota);
  promptInput.focus();
}

function showAuth() {
  workspace.classList.add("is-hidden");
  authPanel.classList.remove("is-hidden");
  submitButton.disabled = false;
  statusPill.textContent = "待生成";
  usernameInput.focus();
}

function setAuthLoading(isLoading) {
  loginButton.disabled = isLoading;
  registerButton.disabled = isLoading;
  loginButton.textContent = isLoading ? "处理中..." : "登录";
}

function showAuthError(message) {
  authMessage.textContent = message;
  authMessage.classList.remove("is-hidden");
}

function clearAuthMessage() {
  authMessage.textContent = "";
  authMessage.classList.add("is-hidden");
}

function updateQuota(quota) {
  currentQuota = quota;

  if (!quota) {
    quotaLabel.textContent = "";
    quotaResetText.textContent = "";
    return;
  }

  quotaLabel.textContent = `剩余 ${quota.remaining} / ${quota.limit}`;

  if (quota.resetAt && quota.remaining < quota.limit) {
    const resetAt = new Date(quota.resetAt);
    quotaResetText.textContent = Number.isNaN(resetAt.getTime())
      ? ""
      : `最早一次额度将在 ${resetAt.toLocaleTimeString([], {
          hour: "2-digit",
          minute: "2-digit"
        })} 恢复。`;
  } else {
    quotaResetText.textContent = "";
  }
}

backgroundSelect.addEventListener("change", () => {
  if (backgroundSelect.value === "transparent" && formatSelect.value === "jpeg") {
    formatSelect.value = "png";
  }
});

formatSelect.addEventListener("change", () => {
  if (formatSelect.value === "jpeg" && backgroundSelect.value === "transparent") {
    backgroundSelect.value = "auto";
  }
});

promptInput.addEventListener("input", () => {
  if (promptInput.value.trim()) {
    statusPill.textContent = "准备生成";
  } else {
    statusPill.textContent = "待生成";
  }
});

function setLoading(isLoading) {
  submitButton.disabled = isLoading;
  submitButton.textContent = isLoading ? "正在生成..." : "✦ 生成图片";
  loadingState.classList.toggle("is-hidden", !isLoading);
  emptyState.classList.add("is-hidden");

  if (isLoading) {
    resultImage.classList.add("is-hidden");
    downloadButton.classList.add("is-hidden");
    statusPill.textContent = "生成中";
  }
}

function showImage(src, format) {
  resultImage.src = src;
  resultImage.classList.remove("is-hidden");
  emptyState.classList.add("is-hidden");
  prepareDownload(src, format);
}

async function prepareDownload(src, format) {
  if (lastObjectUrl) {
    URL.revokeObjectURL(lastObjectUrl);
    lastObjectUrl = "";
  }

  const extension = format === "jpeg" ? "jpg" : format;
  downloadButton.download = `ai-image-${Date.now()}.${extension}`;

  if (src.startsWith("data:image/")) {
    downloadButton.href = src;
  } else {
    try {
      const imageResponse = await fetch(src);
      const blob = await imageResponse.blob();
      lastObjectUrl = URL.createObjectURL(blob);
      downloadButton.href = lastObjectUrl;
    } catch {
      downloadButton.href = src;
    }
  }

  downloadButton.classList.remove("is-hidden");
}

function showError(message) {
  errorMessage.textContent = message;
  errorMessage.classList.remove("is-hidden");
  emptyState.classList.remove("is-hidden");
}

function showRevisedPrompt(text) {
  if (!text) {
    revisedPrompt.classList.add("is-hidden");
    return;
  }

  revisedPrompt.textContent = `优化后的提示词：${text}`;
  revisedPrompt.classList.remove("is-hidden");
}

function clearMessage() {
  errorMessage.textContent = "";
  errorMessage.classList.add("is-hidden");
  revisedPrompt.textContent = "";
  revisedPrompt.classList.add("is-hidden");
}

async function readResponseData(response) {
  const text = await response.text();

  try {
    return JSON.parse(text);
  } catch {
    if (response.status === 504 || looksLikeHtml(text)) {
      return {
        ok: false,
        error: "图片生成超过网关等待时间，服务器返回了 HTML 错误页。请稍后重试，或提高 nginx / 上游接口的超时时间。"
      };
    }

    return {
      ok: false,
      error: text.trim() || `请求失败，HTTP 状态码：${response.status}`
    };
  }
}

function looksLikeHtml(value) {
  return /^\s*<(?:!doctype\s+html|html|head|body|h\d|title)\b/i.test(value || "");
}
