package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultImageURL         = "https://api.openai.com/v1/images/generations"
	maxRequestBodyBytes     = 96 * 1024
	defaultImageTimeout     = 4 * time.Minute
	defaultHeartbeat        = 15 * time.Second
	defaultQueueTimeout     = 3 * time.Minute
	defaultResponseMaxBytes = 64 * 1024 * 1024
	defaultUsageLimit       = 6
	defaultUsageWindow      = time.Hour
	sessionCookieName       = "gpt_image_session"
	sessionTTL              = 7 * 24 * time.Hour
	passwordHashIterations  = 120000
)

var (
	allowedSizes       = map[string]bool{"auto": true, "1024x1024": true, "1536x1024": true, "1024x1536": true}
	allowedQualities   = map[string]bool{"auto": true, "low": true, "medium": true, "high": true}
	allowedFormats     = map[string]bool{"png": true, "jpeg": true, "webp": true}
	allowedBackgrounds = map[string]bool{"auto": true, "opaque": true, "transparent": true}
)

type app struct {
	publicDir           string
	client              *http.Client
	queueSlots          chan struct{}
	workSlots           chan struct{}
	limiter             *rateLimiter
	auth                *authStore
	session             *sessionManager
	usageLimit          int
	usageWindow         time.Duration
	imageTimeout        time.Duration
	queueTimeout        time.Duration
	heartbeatInterval   time.Duration
	upstreamMaxBodySize int64
}

type generateRequest struct {
	Prompt     string `json:"prompt"`
	Size       string `json:"size"`
	Quality    string `json:"quality"`
	Format     string `json:"format"`
	Background string `json:"background"`
}

type authRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type upstreamPayload struct {
	Model        string `json:"model"`
	Prompt       string `json:"prompt"`
	N            int    `json:"n"`
	Size         string `json:"size"`
	Quality      string `json:"quality"`
	OutputFormat string `json:"output_format"`
	Background   string `json:"background"`
}

func main() {
	baseDir, err := os.Getwd()
	if err != nil {
		log.Fatalf("读取工作目录失败: %v", err)
	}

	loadEnvFile(filepath.Join(baseDir, ".env"))

	port := getenvInt("PORT", 3000)
	maxConcurrency := getenvInt("MAX_IMAGE_CONCURRENCY", 6)
	maxQueue := getenvInt("MAX_IMAGE_QUEUE", 120)
	if maxConcurrency < 1 {
		maxConcurrency = 1
	}
	if maxQueue < 0 {
		maxQueue = 0
	}

	imageTimeout := getenvDurationMillis("IMAGE_REQUEST_TIMEOUT_MS", defaultImageTimeout)
	heartbeat := getenvDurationMillis("RESPONSE_HEARTBEAT_MS", defaultHeartbeat)
	queueTimeout := getenvDurationMillis("QUEUE_TIMEOUT_MS", defaultQueueTimeout)
	usageLimit := getenvPositiveInt("USER_IMAGE_LIMIT", defaultUsageLimit)
	usageWindow := getenvDurationMillis("USER_IMAGE_WINDOW_MS", defaultUsageWindow)
	authDataFile := getenvString("AUTH_DATA_FILE", filepath.Join(baseDir, "data", "auth.json"))

	auth, err := newAuthStore(authDataFile)
	if err != nil {
		log.Fatalf("初始化用户数据失败: %v", err)
	}

	application := &app{
		publicDir:           filepath.Join(baseDir, "public"),
		client:              &http.Client{},
		queueSlots:          make(chan struct{}, maxConcurrency+maxQueue),
		workSlots:           make(chan struct{}, maxConcurrency),
		limiter:             newRateLimiter(getenvDurationMillis("RATE_LIMIT_WINDOW_MS", time.Minute), getenvInt("RATE_LIMIT_MAX_REQUESTS", 4)),
		auth:                auth,
		session:             newSessionManager(),
		usageLimit:          usageLimit,
		usageWindow:         usageWindow,
		imageTimeout:        imageTimeout,
		queueTimeout:        queueTimeout,
		heartbeatInterval:   heartbeat,
		upstreamMaxBodySize: int64(getenvPositiveInt("OPENAI_RESPONSE_MAX_BYTES", defaultResponseMaxBytes)),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/register", application.handleRegister)
	mux.HandleFunc("/api/login", application.handleLogin)
	mux.HandleFunc("/api/logout", application.handleLogout)
	mux.HandleFunc("/api/me", application.handleMe)
	mux.HandleFunc("/api/generate", application.handleGenerate)
	mux.HandleFunc("/", application.handleStatic)

	server := &http.Server{
		Addr:              fmt.Sprintf(":%d", port),
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       20 * time.Second,
		IdleTimeout:       90 * time.Second,
	}

	log.Printf("Image generation site running at http://localhost:%d", port)
	log.Printf("Image worker concurrency=%d queue=%d", maxConcurrency, maxQueue)
	log.Printf("User image limit=%d per %s", usageLimit, usageWindow)
	log.Fatal(server.ListenAndServe())
}

func (a *app) handleRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		sendJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "Method not allowed"})
		return
	}

	var input authRequest
	if err := decodeJSONBody(w, r, &input); err != nil {
		sendJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}

	user, err := a.auth.createUser(input.Username, input.Password)
	if err != nil {
		sendJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}

	a.setSessionCookie(w, r, user.ID)
	sendJSON(w, http.StatusOK, map[string]any{
		"ok":    true,
		"user":  publicUser(user),
		"quota": a.auth.usageInfo(user.ID, a.usageLimit, a.usageWindow),
	})
}

func (a *app) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		sendJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "Method not allowed"})
		return
	}

	var input authRequest
	if err := decodeJSONBody(w, r, &input); err != nil {
		sendJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}

	user, err := a.auth.authenticate(input.Username, input.Password)
	if err != nil {
		sendJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": err.Error()})
		return
	}

	a.setSessionCookie(w, r, user.ID)
	sendJSON(w, http.StatusOK, map[string]any{
		"ok":    true,
		"user":  publicUser(user),
		"quota": a.auth.usageInfo(user.ID, a.usageLimit, a.usageWindow),
	})
}

func (a *app) handleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		sendJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "Method not allowed"})
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   isSecureRequest(r),
	})
	sendJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *app) handleMe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		sendJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "Method not allowed"})
		return
	}

	user, ok := a.currentUser(r)
	if !ok {
		sendJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "authenticated": false})
		return
	}

	sendJSON(w, http.StatusOK, map[string]any{
		"ok":            true,
		"authenticated": true,
		"user":          publicUser(user),
		"quota":         a.auth.usageInfo(user.ID, a.usageLimit, a.usageWindow),
	})
}

func (a *app) handleGenerate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		sendJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "Method not allowed"})
		return
	}

	user, ok := a.currentUser(r)
	if !ok {
		sendJSON(w, http.StatusUnauthorized, map[string]any{
			"ok":    false,
			"error": "请先登录后再生成图片。",
		})
		return
	}

	apiKey := os.Getenv("OPENAI_API_KEY")
	if apiKey == "" {
		sendJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "服务端还没有配置 OPENAI_API_KEY。请在 .env 或服务器环境变量中设置它。",
		})
		return
	}

	if !a.limiter.allow(clientIP(r)) {
		sendJSON(w, http.StatusTooManyRequests, map[string]any{
			"ok":    false,
			"error": "请求过于频繁，请稍后再试。",
		})
		return
	}

	var input generateRequest
	if err := decodeJSONBody(w, r, &input); err != nil {
		sendJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	prompt := strings.TrimSpace(input.Prompt)
	size := normalizeOption(input.Size, allowedSizes, "1024x1024")
	quality := normalizeOption(input.Quality, allowedQualities, "auto")
	format := normalizeOption(input.Format, allowedFormats, "png")
	background := normalizeOption(input.Background, allowedBackgrounds, "auto")

	if prompt == "" {
		sendJSON(w, http.StatusBadRequest, map[string]string{"error": "请输入图片描述提示词。"})
		return
	}
	if len([]rune(prompt)) > 32000 {
		sendJSON(w, http.StatusBadRequest, map[string]string{"error": "提示词过长，请控制在 32000 个字符以内。"})
		return
	}
	if background == "transparent" && format == "jpeg" {
		sendJSON(w, http.StatusBadRequest, map[string]string{"error": "透明背景需要选择 PNG 或 WebP 格式。"})
		return
	}
	if quota := a.auth.usageInfo(user.ID, a.usageLimit, a.usageWindow); quota.Remaining <= 0 {
		sendJSON(w, http.StatusTooManyRequests, map[string]any{
			"ok":    false,
			"error": limitErrorMessage(quota.ResetAt),
			"quota": quota,
		})
		return
	}

	if !a.reserveQueueSlot() {
		sendJSON(w, http.StatusTooManyRequests, map[string]any{
			"ok":    false,
			"error": "当前生图排队人数较多，请稍后再试。",
		})
		return
	}
	defer a.releaseQueueSlot()

	stream := newJSONStream(w, a.heartbeatInterval)
	defer stream.close()

	if err := a.acquireWorker(r.Context()); err != nil {
		stream.send(map[string]any{
			"ok":    false,
			"error": queueErrorMessage(err, a.queueTimeout),
		})
		return
	}
	defer a.releaseWorker()

	quota, err := a.auth.consumeUsage(user.ID, a.usageLimit, a.usageWindow)
	if err != nil {
		stream.send(map[string]any{
			"ok":    false,
			"error": err.Error(),
			"quota": quota,
		})
		return
	}

	image, revisedPrompt, model, err := a.generateImage(r.Context(), apiKey, upstreamPayload{
		Model:        getenvString("OPENAI_IMAGE_MODEL", "gpt-image-2"),
		Prompt:       prompt,
		N:            1,
		Size:         size,
		Quality:      quality,
		OutputFormat: format,
		Background:   background,
	}, format)
	if err != nil {
		stream.send(map[string]any{
			"ok":    false,
			"error": err.Error(),
			"quota": quota,
		})
		return
	}

	stream.send(map[string]any{
		"ok":            true,
		"image":         image,
		"revisedPrompt": revisedPrompt,
		"model":         model,
		"quota":         quota,
	})
}

func (a *app) currentUser(r *http.Request) (*userRecord, bool) {
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil || cookie.Value == "" {
		return nil, false
	}

	userID, ok := a.session.verify(cookie.Value)
	if !ok {
		return nil, false
	}

	user, ok := a.auth.userByID(userID)
	return user, ok
}

func (a *app) setSessionCookie(w http.ResponseWriter, r *http.Request, userID string) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    a.session.sign(userID, time.Now().Add(sessionTTL)),
		Path:     "/",
		MaxAge:   int(sessionTTL.Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   isSecureRequest(r),
	})
}

func (a *app) reserveQueueSlot() bool {
	select {
	case a.queueSlots <- struct{}{}:
		return true
	default:
		return false
	}
}

func (a *app) releaseQueueSlot() {
	select {
	case <-a.queueSlots:
	default:
	}
}

func (a *app) acquireWorker(ctx context.Context) error {
	timer := time.NewTimer(a.queueTimeout)
	defer timer.Stop()

	select {
	case a.workSlots <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return errQueueTimeout
	}
}

func (a *app) releaseWorker() {
	select {
	case <-a.workSlots:
	default:
	}
}

func (a *app) generateImage(ctx context.Context, apiKey string, payload upstreamPayload, format string) (string, string, string, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return "", "", payload.Model, errors.New("图片生成请求无法序列化。")
	}

	requestCtx, cancel := context.WithTimeout(ctx, a.imageTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, getImageEndpoint(), bytes.NewReader(body))
	if err != nil {
		return "", "", payload.Model, errors.New("图片生成接口地址无效。")
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.client.Do(req)
	if err != nil {
		if errors.Is(requestCtx.Err(), context.DeadlineExceeded) {
			return "", "", payload.Model, fmt.Errorf("图片生成超过 %d 秒仍未完成，请稍后重试或调低质量。", int(a.imageTimeout.Seconds()))
		}
		if errors.Is(ctx.Err(), context.Canceled) {
			return "", "", payload.Model, errors.New("请求已取消。")
		}
		return "", "", payload.Model, errors.New("图片生成接口暂时无法连接，请稍后再试。")
	}
	defer resp.Body.Close()

	responseText, err := readLimited(resp.Body, a.upstreamMaxBodySize)
	if err != nil {
		return "", "", payload.Model, errors.New("图片生成接口返回内容过大，请调低尺寸或质量后重试。")
	}

	var data map[string]any
	if err := json.Unmarshal(responseText, &data); err != nil {
		if resp.StatusCode == http.StatusGatewayTimeout || looksLikeHTML(responseText) {
			return "", "", payload.Model, errors.New("图片生成接口返回了网关超时页面，而不是 JSON。请稍后重试，或提高上游/反向代理的超时时间。")
		}
		return "", "", payload.Model, errors.New("图片生成接口没有返回有效 JSON。请检查上游接口地址、模型名和服务状态。")
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", "", payload.Model, errors.New(extractUpstreamError(data, responseText, resp.StatusCode))
	}

	image := normalizeImageResult(data, format)
	if image == "" {
		return "", "", payload.Model, errors.New("图片生成接口没有返回可识别的图片数据。请检查上游接口返回格式。")
	}

	return image, extractRevisedPrompt(data), payload.Model, nil
}

func (a *app) handleStatic(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		sendJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "Method not allowed"})
		return
	}

	clean := filepath.Clean("/" + strings.TrimPrefix(r.URL.Path, "/"))
	if clean == "/" {
		clean = "/index.html"
	}

	target := filepath.Join(a.publicDir, filepath.FromSlash(clean))
	rel, err := filepath.Rel(a.publicDir, target)
	if err != nil || strings.HasPrefix(rel, "..") {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}

	content, err := os.ReadFile(target)
	if err != nil {
		target = filepath.Join(a.publicDir, "index.html")
		content, err = os.ReadFile(target)
		if err != nil {
			http.Error(w, "Not found", http.StatusNotFound)
			return
		}
	}

	ext := strings.ToLower(filepath.Ext(target))
	contentType := mime.TypeByExtension(ext)
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	w.Header().Set("Content-Type", contentType)
	if ext == ".html" {
		w.Header().Set("Cache-Control", "no-cache")
	} else {
		w.Header().Set("Cache-Control", "public, max-age=3600")
	}
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(content)
	}
}

func decodeJSONBody(w http.ResponseWriter, r *http.Request, target any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
	defer r.Body.Close()

	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(target); err != nil {
		if strings.Contains(err.Error(), "http: request body too large") {
			return errors.New("请求体过大。")
		}
		return errors.New("请求体不是有效的 JSON。")
	}

	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return errors.New("请求体只能包含一个 JSON 对象。")
	}

	return nil
}

type jsonStream struct {
	w          http.ResponseWriter
	flusher    http.Flusher
	mu         sync.Mutex
	done       chan struct{}
	closedOnce sync.Once
	sent       bool
}

func newJSONStream(w http.ResponseWriter, interval time.Duration) *jsonStream {
	flusher, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(" "))
	if flusher != nil {
		flusher.Flush()
	}

	stream := &jsonStream{
		w:       w,
		flusher: flusher,
		done:    make(chan struct{}),
	}

	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				stream.mu.Lock()
				_, _ = w.Write([]byte("\n "))
				if flusher != nil {
					flusher.Flush()
				}
				stream.mu.Unlock()
			case <-stream.done:
				return
			}
		}
	}()

	return stream
}

func (s *jsonStream) send(payload any) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.sent {
		return
	}
	s.sent = true
	s.close()
	_ = json.NewEncoder(s.w).Encode(payload)
	if s.flusher != nil {
		s.flusher.Flush()
	}
}

func (s *jsonStream) close() {
	s.closedOnce.Do(func() {
		close(s.done)
	})
}

type rateLimiter struct {
	mu       sync.Mutex
	window   time.Duration
	maxHits  int
	visitors map[string]*visitor
}

type visitor struct {
	count     int
	resetTime time.Time
}

func newRateLimiter(window time.Duration, maxHits int) *rateLimiter {
	if window <= 0 {
		window = time.Minute
	}
	if maxHits <= 0 {
		maxHits = 1
	}

	limiter := &rateLimiter{
		window:   window,
		maxHits:  maxHits,
		visitors: make(map[string]*visitor),
	}

	go limiter.cleanup()
	return limiter
}

func (r *rateLimiter) allow(key string) bool {
	now := time.Now()

	r.mu.Lock()
	defer r.mu.Unlock()

	current, ok := r.visitors[key]
	if !ok || now.After(current.resetTime) {
		r.visitors[key] = &visitor{count: 1, resetTime: now.Add(r.window)}
		return true
	}

	if current.count >= r.maxHits {
		return false
	}

	current.count++
	return true
}

func (r *rateLimiter) cleanup() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()

	for range ticker.C {
		now := time.Now()
		r.mu.Lock()
		for key, current := range r.visitors {
			if now.After(current.resetTime) {
				delete(r.visitors, key)
			}
		}
		r.mu.Unlock()
	}
}

type authStore struct {
	mu       sync.Mutex
	filePath string
	data     authData
}

type authData struct {
	NextUserID int64                  `json:"nextUserId"`
	Users      map[string]*userRecord `json:"users"`
}

type userRecord struct {
	ID           string    `json:"id"`
	Username     string    `json:"username"`
	PasswordHash string    `json:"passwordHash"`
	Salt         string    `json:"salt"`
	CreatedAt    time.Time `json:"createdAt"`
	Usage        []int64   `json:"usage"`
}

type quotaInfo struct {
	Limit     int    `json:"limit"`
	Used      int    `json:"used"`
	Remaining int    `json:"remaining"`
	ResetAt   string `json:"resetAt,omitempty"`
}

func newAuthStore(filePath string) (*authStore, error) {
	store := &authStore{
		filePath: filePath,
		data: authData{
			NextUserID: 1,
			Users:      make(map[string]*userRecord),
		},
	}

	content, err := os.ReadFile(filePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return store, store.saveLocked()
		}
		return nil, err
	}

	if len(bytes.TrimSpace(content)) == 0 {
		return store, nil
	}
	if err := json.Unmarshal(content, &store.data); err != nil {
		return nil, err
	}
	if store.data.NextUserID < 1 {
		store.data.NextUserID = 1
	}
	if store.data.Users == nil {
		store.data.Users = make(map[string]*userRecord)
	}
	return store, nil
}

func (s *authStore) createUser(username, password string) (*userRecord, error) {
	normalized, err := normalizeUsername(username)
	if err != nil {
		return nil, err
	}
	if err := validatePassword(password); err != nil {
		return nil, err
	}

	salt, err := randomBytes(16)
	if err != nil {
		return nil, errors.New("无法生成密码盐，请稍后重试。")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.data.Users[normalized]; exists {
		return nil, errors.New("这个用户名已经被注册。")
	}

	user := &userRecord{
		ID:           strconv.FormatInt(s.data.NextUserID, 10),
		Username:     normalized,
		PasswordHash: hashPassword(password, salt),
		Salt:         base64.RawURLEncoding.EncodeToString(salt),
		CreatedAt:    time.Now(),
		Usage:        []int64{},
	}
	s.data.NextUserID++
	s.data.Users[normalized] = user

	if err := s.saveLocked(); err != nil {
		return nil, errors.New("保存用户数据失败，请稍后重试。")
	}
	return cloneUser(user), nil
}

func (s *authStore) authenticate(username, password string) (*userRecord, error) {
	normalized, err := normalizeUsername(username)
	if err != nil {
		return nil, errors.New("用户名或密码不正确。")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	user, exists := s.data.Users[normalized]
	if !exists {
		return nil, errors.New("用户名或密码不正确。")
	}

	salt, err := base64.RawURLEncoding.DecodeString(user.Salt)
	if err != nil {
		return nil, errors.New("用户数据异常，请联系管理员。")
	}
	if !constantTimeEqual(user.PasswordHash, hashPassword(password, salt)) {
		return nil, errors.New("用户名或密码不正确。")
	}

	return cloneUser(user), nil
}

func (s *authStore) userByID(userID string) (*userRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, user := range s.data.Users {
		if user.ID == userID {
			return cloneUser(user), true
		}
	}
	return nil, false
}

func (s *authStore) usageInfo(userID string, limit int, window time.Duration) quotaInfo {
	now := time.Now()

	s.mu.Lock()
	defer s.mu.Unlock()

	user := s.userByIDLocked(userID)
	if user == nil {
		return quotaInfo{Limit: limit, Used: 0, Remaining: limit}
	}

	user.Usage = filterUsage(user.Usage, now.Add(-window))
	return buildQuotaInfo(user.Usage, limit, window, now)
}

func (s *authStore) consumeUsage(userID string, limit int, window time.Duration) (quotaInfo, error) {
	now := time.Now()

	s.mu.Lock()
	defer s.mu.Unlock()

	user := s.userByIDLocked(userID)
	if user == nil {
		return quotaInfo{Limit: limit}, errors.New("登录状态已失效，请重新登录。")
	}

	user.Usage = filterUsage(user.Usage, now.Add(-window))
	if len(user.Usage) >= limit {
		quota := buildQuotaInfo(user.Usage, limit, window, now)
		return quota, errors.New(limitErrorMessage(quota.ResetAt))
	}

	user.Usage = append(user.Usage, now.Unix())
	quota := buildQuotaInfo(user.Usage, limit, window, now)
	if err := s.saveLocked(); err != nil {
		return quota, errors.New("保存用量失败，请稍后再试。")
	}

	return quota, nil
}

func (s *authStore) userByIDLocked(userID string) *userRecord {
	for _, user := range s.data.Users {
		if user.ID == userID {
			return user
		}
	}
	return nil
}

func (s *authStore) saveLocked() error {
	if err := os.MkdirAll(filepath.Dir(s.filePath), 0700); err != nil {
		return err
	}

	content, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return err
	}

	tempPath := s.filePath + ".tmp"
	if err := os.WriteFile(tempPath, content, 0600); err != nil {
		return err
	}
	return os.Rename(tempPath, s.filePath)
}

func filterUsage(values []int64, cutoff time.Time) []int64 {
	cutoffUnix := cutoff.Unix()
	next := values[:0]
	for _, value := range values {
		if value > cutoffUnix {
			next = append(next, value)
		}
	}
	return next
}

func buildQuotaInfo(usage []int64, limit int, window time.Duration, now time.Time) quotaInfo {
	used := len(usage)
	remaining := limit - used
	if remaining < 0 {
		remaining = 0
	}

	info := quotaInfo{
		Limit:     limit,
		Used:      used,
		Remaining: remaining,
	}
	if used > 0 {
		resetAt := time.Unix(usage[0], 0).Add(window)
		if resetAt.Before(now) {
			resetAt = now
		}
		info.ResetAt = resetAt.Format(time.RFC3339)
	}
	return info
}

func publicUser(user *userRecord) map[string]any {
	return map[string]any{
		"id":        user.ID,
		"username":  user.Username,
		"createdAt": user.CreatedAt.Format(time.RFC3339),
	}
}

func cloneUser(user *userRecord) *userRecord {
	if user == nil {
		return nil
	}
	next := *user
	next.Usage = append([]int64(nil), user.Usage...)
	return &next
}

func normalizeUsername(username string) (string, error) {
	normalized := strings.ToLower(strings.TrimSpace(username))
	if len(normalized) < 3 || len(normalized) > 32 {
		return "", errors.New("用户名长度需要在 3 到 32 个字符之间。")
	}
	for _, char := range normalized {
		if (char >= 'a' && char <= 'z') || (char >= '0' && char <= '9') || char == '_' || char == '-' {
			continue
		}
		return "", errors.New("用户名只能包含英文、数字、下划线或短横线。")
	}
	return normalized, nil
}

func validatePassword(password string) error {
	if len(password) < 8 {
		return errors.New("密码至少需要 8 个字符。")
	}
	if len(password) > 128 {
		return errors.New("密码不能超过 128 个字符。")
	}
	return nil
}

func hashPassword(password string, salt []byte) string {
	hashBytes := pbkdf2Key(sha256.New, []byte(password), salt, passwordHashIterations, 32)
	return base64.RawURLEncoding.EncodeToString(hashBytes)
}

func pbkdf2Key(hashFunc func() hash.Hash, password, salt []byte, iter, keyLen int) []byte {
	prf := hmac.New(hashFunc, password)
	hashLen := prf.Size()
	numBlocks := (keyLen + hashLen - 1) / hashLen
	var output []byte

	for block := 1; block <= numBlocks; block++ {
		prf.Reset()
		_, _ = prf.Write(salt)
		_, _ = prf.Write([]byte{byte(block >> 24), byte(block >> 16), byte(block >> 8), byte(block)})
		u := prf.Sum(nil)
		t := append([]byte(nil), u...)

		for i := 1; i < iter; i++ {
			prf.Reset()
			_, _ = prf.Write(u)
			u = prf.Sum(nil)
			for j := range t {
				t[j] ^= u[j]
			}
		}
		output = append(output, t...)
	}

	return output[:keyLen]
}

func constantTimeEqual(left, right string) bool {
	return hmac.Equal([]byte(left), []byte(right))
}

type sessionManager struct {
	secret []byte
}

type sessionPayload struct {
	UserID string `json:"uid"`
	Exp    int64  `json:"exp"`
}

func newSessionManager() *sessionManager {
	secretText := strings.TrimSpace(os.Getenv("SESSION_SECRET"))
	if secretText == "" {
		randomSecret, err := randomBytes(32)
		if err != nil {
			log.Fatalf("无法生成会话密钥: %v", err)
		}
		log.Print("SESSION_SECRET 未设置，已使用临时会话密钥；生产环境请配置固定强随机值。")
		return &sessionManager{secret: randomSecret}
	}
	return &sessionManager{secret: []byte(secretText)}
}

func (s *sessionManager) sign(userID string, expiresAt time.Time) string {
	payload := sessionPayload{UserID: userID, Exp: expiresAt.Unix()}
	content, _ := json.Marshal(payload)
	encodedPayload := base64.RawURLEncoding.EncodeToString(content)
	signature := s.signature(encodedPayload)
	return encodedPayload + "." + signature
}

func (s *sessionManager) verify(token string) (string, bool) {
	payloadText, signature, ok := strings.Cut(token, ".")
	if !ok || !constantTimeEqual(signature, s.signature(payloadText)) {
		return "", false
	}

	content, err := base64.RawURLEncoding.DecodeString(payloadText)
	if err != nil {
		return "", false
	}

	var payload sessionPayload
	if err := json.Unmarshal(content, &payload); err != nil {
		return "", false
	}
	if payload.UserID == "" || time.Now().Unix() > payload.Exp {
		return "", false
	}
	return payload.UserID, true
}

func (s *sessionManager) signature(payload string) string {
	mac := hmac.New(sha256.New, s.secret)
	_, _ = mac.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func randomBytes(size int) ([]byte, error) {
	value := make([]byte, size)
	_, err := rand.Read(value)
	return value, err
}

func limitErrorMessage(resetAt string) string {
	if resetAt == "" {
		return "你这一小时的生图次数已用完，请稍后再试。"
	}
	return "你这一小时的生图次数已用完，请在 " + resetAt + " 后再试。"
}

func isSecureRequest(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func normalizeOption(value string, allowed map[string]bool, fallback string) string {
	next := strings.TrimSpace(value)
	if next == "" {
		next = fallback
	}
	if allowed[next] {
		return next
	}
	return fallback
}

func getImageEndpoint() string {
	if value := strings.TrimSpace(os.Getenv("OPENAI_IMAGE_URL")); value != "" {
		return value
	}
	if value := strings.TrimSpace(os.Getenv("OPENAI_BASE_URL")); value != "" {
		return strings.TrimRight(value, "/") + "/images/generations"
	}
	return defaultImageURL
}

func normalizeImageResult(data map[string]any, requestedFormat string) string {
	item := firstMapFromArray(data["data"])
	if item == nil {
		item = firstResultMap(data["output"])
	}

	base64Value := firstString(
		stringFromMap(item, "b64_json"),
		stringFromMap(item, "result"),
		stringFromMap(data, "b64_json"),
		stringFromMap(data, "image_base64"),
	)
	url := firstString(
		stringFromMap(item, "url"),
		stringFromMap(data, "url"),
		stringFromMap(data, "image_url"),
	)

	if url != "" {
		return url
	}
	if base64Value == "" {
		return ""
	}
	if strings.HasPrefix(base64Value, "data:image/") {
		return base64Value
	}

	mimeType := "image/" + requestedFormat
	if requestedFormat == "jpeg" {
		mimeType = "image/jpeg"
	}
	return "data:" + mimeType + ";base64," + base64Value
}

func extractRevisedPrompt(data map[string]any) string {
	item := firstMapFromArray(data["data"])
	return firstString(
		stringFromMap(item, "revised_prompt"),
		stringFromMap(data, "revised_prompt"),
	)
}

func firstMapFromArray(value any) map[string]any {
	items, ok := value.([]any)
	if !ok || len(items) == 0 {
		return nil
	}
	item, _ := items[0].(map[string]any)
	return item
}

func firstResultMap(value any) map[string]any {
	items, ok := value.([]any)
	if !ok {
		return nil
	}
	for _, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if stringFromMap(item, "result") != "" || stringFromMap(item, "b64_json") != "" {
			return item
		}
	}
	return nil
}

func stringFromMap(value map[string]any, key string) string {
	if value == nil {
		return ""
	}
	text, ok := value[key].(string)
	if !ok {
		return ""
	}
	return text
}

func firstString(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func extractUpstreamError(data map[string]any, fallback []byte, status int) string {
	if errorValue, ok := data["error"].(map[string]any); ok {
		if message := stringFromMap(errorValue, "message"); message != "" {
			return message
		}
	}
	if message := stringFromMap(data, "message"); message != "" {
		return message
	}
	if text := strings.TrimSpace(string(fallback)); text != "" && !looksLikeHTML(fallback) {
		return text
	}
	if status == http.StatusGatewayTimeout || looksLikeHTML(fallback) {
		return "图片生成接口返回了网关超时页面，而不是 JSON。请稍后重试，或提高上游/反向代理的超时时间。"
	}
	return "图片生成接口没有返回有效 JSON。请检查上游接口地址、模型名和服务状态。"
}

func looksLikeHTML(value []byte) bool {
	text := strings.TrimSpace(strings.ToLower(string(value)))
	return strings.HasPrefix(text, "<!doctype html") ||
		strings.HasPrefix(text, "<html") ||
		strings.HasPrefix(text, "<head") ||
		strings.HasPrefix(text, "<body") ||
		strings.HasPrefix(text, "<title")
}

func readLimited(reader io.Reader, limit int64) ([]byte, error) {
	limited := io.LimitReader(reader, limit+1)
	content, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	if int64(len(content)) > limit {
		return nil, errors.New("response too large")
	}
	return content, nil
}

func sendJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func clientIP(r *http.Request) string {
	if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" {
		parts := strings.Split(forwarded, ",")
		if ip := strings.TrimSpace(parts[0]); ip != "" {
			return ip
		}
	}
	if realIP := strings.TrimSpace(r.Header.Get("X-Real-IP")); realIP != "" {
		return realIP
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func getenvString(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func getenvInt(key string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func getenvPositiveInt(key string, fallback int) int {
	value := getenvInt(key, fallback)
	if value <= 0 {
		return fallback
	}
	return value
}

func getenvDurationMillis(key string, fallback time.Duration) time.Duration {
	value := getenvInt(key, int(fallback/time.Millisecond))
	if value <= 0 {
		return fallback
	}
	return time.Duration(value) * time.Millisecond
}

func loadEnvFile(filePath string) {
	content, err := os.ReadFile(filePath)
	if err != nil {
		return
	}

	for _, rawLine := range strings.Split(string(content), "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		separator := strings.Index(line, "=")
		if separator == -1 {
			continue
		}

		key := strings.TrimSpace(line[:separator])
		value := strings.TrimSpace(line[separator+1:])
		value = strings.Trim(value, `"'`)
		if key != "" && os.Getenv(key) == "" {
			_ = os.Setenv(key, value)
		}
	}
}

var errQueueTimeout = errors.New("queue timeout")

func queueErrorMessage(err error, timeout time.Duration) string {
	if errors.Is(err, errQueueTimeout) {
		return fmt.Sprintf("当前生图排队超过 %d 秒，请稍后再试。", int(timeout.Seconds()))
	}
	return "请求已取消。"
}
