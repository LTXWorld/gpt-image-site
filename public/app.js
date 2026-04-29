const form = document.querySelector("#generateForm");
const promptInput = document.querySelector("#prompt");
const submitButton = document.querySelector("#submitButton");
const statusPill = document.querySelector("#statusPill");
const emptyState = document.querySelector("#emptyState");
const loadingState = document.querySelector("#loadingState");
const resultImage = document.querySelector("#resultImage");
const errorMessage = document.querySelector("#errorMessage");
const revisedPrompt = document.querySelector("#revisedPrompt");
const downloadButton = document.querySelector("#downloadButton");
const formatSelect = document.querySelector("#format");
const backgroundSelect = document.querySelector("#background");

let lastObjectUrl = "";

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

    const data = await response.json();

    if (!response.ok) {
      throw new Error(data.error || "图片生成失败。");
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
