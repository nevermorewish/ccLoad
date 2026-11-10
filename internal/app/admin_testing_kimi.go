package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"ccLoad/internal/model"
	"ccLoad/internal/testutil"
)

type kimiRunRequest struct {
	Model              string   `json:"model"`
	KeyIndex           *int     `json:"key_index"`
	CaseIDs            []string `json:"case_ids"`
	Concurrency        int      `json:"concurrency"`
	TimeoutSeconds     int      `json:"timeout_seconds"`
	UploadMiB          int      `json:"upload_mib"`
	UploadLimitSeconds int      `json:"upload_limit_seconds"`
	ImageURL           string   `json:"image_url"`
}

func (r *kimiRunRequest) validate() ([]kimiCase, error) {
	if strings.TrimSpace(r.Model) == "" || len(r.Model) > 191 {
		return nil, fmt.Errorf("请选择有效模型")
	}
	if r.Concurrency == 0 {
		r.Concurrency = 6
	}
	if r.TimeoutSeconds == 0 {
		r.TimeoutSeconds = 180
	}
	if r.UploadMiB == 0 {
		r.UploadMiB = 20
	}
	if r.UploadLimitSeconds == 0 {
		r.UploadLimitSeconds = 60
	}
	if r.Concurrency < 1 || r.Concurrency > 6 || r.TimeoutSeconds < 10 || r.TimeoutSeconds > 600 || r.UploadMiB < 1 || r.UploadMiB > 32 || r.UploadLimitSeconds < 1 || r.UploadLimitSeconds > 600 {
		return nil, fmt.Errorf("并发须为 1～6，超时 10～600 秒，上传大小 1～32 MiB，上传阈值 1～600 秒")
	}
	wanted := map[string]bool{}
	for _, id := range r.CaseIDs {
		wanted[id] = true
	}
	selected := []kimiCase{}
	for _, item := range kimiCases {
		if len(r.CaseIDs) == 0 || wanted[item.ID] {
			selected = append(selected, item)
			delete(wanted, item.ID)
		}
	}
	if len(wanted) != 0 {
		return nil, fmt.Errorf("存在未知用例")
	}
	for _, item := range selected {
		if item.ID == "kimi.k3.image_url_probe" {
			u, err := url.Parse(r.ImageURL)
			if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || len(r.ImageURL) > 2048 {
				return nil, fmt.Errorf("请填写上游可以访问的 HTTP(S) 蓝色测试图片 URL")
			}
		}
	}
	return selected, nil
}

func kimiEndpoint(raw string) (string, error) {
	exact := model.HasExactUpstreamURLMarker(raw)
	raw = strings.TrimRight(model.StripExactUpstreamURLMarker(raw), "/")
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return "", fmt.Errorf("渠道 URL 无效")
	}
	if !exact && !strings.HasSuffix(u.Path, "/chat/completions") {
		if strings.HasSuffix(u.Path, "/v1") {
			u.Path += "/chat/completions"
		} else {
			u.Path += "/v1/chat/completions"
		}
	}
	return u.String(), nil
}

// HandleKimiCases returns the configured Kimi verification cases.
func (s *Server) HandleKimiCases(c *gin.Context) {
	RespondJSON(c, http.StatusOK, gin.H{"version": kimiSuiteVersion, "cases": kimiCases})
}

// HandleKimiBlueFixture serves the image fixture used by Kimi checks.
func (s *Server) HandleKimiBlueFixture(c *gin.Context) {
	data, _ := kimiFixtures.ReadFile("kimi_fixtures/blue.png")
	c.Header("Cache-Control", "public, max-age=86400")
	c.Data(http.StatusOK, "image/png", data)
}

type kimiResult struct {
	ID           string  `json:"id"`
	Status       string  `json:"status"`
	Expected     string  `json:"expected"`
	Actual       string  `json:"actual"`
	DurationMS   float64 `json:"duration_ms"`
	UploadMS     float64 `json:"upload_ms"`
	RequestBytes int     `json:"request_bytes"`
	HTTPStatus   int     `json:"http_status"`
	Request      any     `json:"request"`
	Response     string  `json:"response"`
}

type kimiEvent struct {
	Type    string      `json:"type"`
	Time    string      `json:"time"`
	Message string      `json:"message"`
	ID      string      `json:"id,omitempty"`
	Result  *kimiResult `json:"result,omitempty"`
	Meta    any         `json:"meta,omitempty"`
}

// HandleChannelKimiTest runs the selected Kimi checks for a channel.
func (s *Server) HandleChannelKimiTest(c *gin.Context) {
	id, err := ParseInt64Param(c, "id")
	if err != nil {
		RespondErrorMsg(c, 400, "invalid channel id")
		return
	}
	var options kimiRunRequest
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 16<<10)
	if err = c.ShouldBindJSON(&options); err != nil {
		RespondErrorMsg(c, 400, "invalid test request")
		return
	}
	cases, err := options.validate()
	if err != nil {
		RespondErrorMsg(c, 400, err.Error())
		return
	}
	ctx, cancel := context.WithCancel(c.Request.Context())
	defer cancel()
	cfg, err := s.store.GetConfig(ctx, id)
	if err != nil {
		RespondErrorMsg(c, 404, "渠道不存在")
		return
	}
	if cfg.GetAuthType() != model.AuthTypeAPIKey {
		RespondErrorMsg(c, 400, "Kimi K3 原生兼容性测试仅支持 API Key 渠道")
		return
	}
	cfg, err = s.selectChannelTestModel(cfg, &testutil.TestChannelRequest{Model: options.Model, ClientProtocol: "openai"}, false)
	if err != nil {
		RespondErrorMsg(c, 400, err.Error())
		return
	}
	keys, err := s.store.GetAPIKeys(ctx, id)
	if err != nil {
		RespondErrorMsg(c, 500, "读取渠道凭证失败")
		return
	}
	cfg, key, err := s.prepareChannelTestAuth(ctx, cfg, keys, options.Model, "openai", options.KeyIndex, "", oauthCredentialUseCurrent)
	if err != nil {
		RespondErrorMsg(c, 400, err.Error())
		return
	}
	endpoint := ""
	for _, entry := range cfg.URLs {
		if entry.SupportsProtocol("openai") {
			endpoint, err = kimiEndpoint(entry.RuntimeURL())
			break
		}
	}
	if err != nil || endpoint == "" {
		RespondErrorMsg(c, 400, "渠道没有可用的 OpenAI Chat URL")
		return
	}
	selected, _ := s.firstModelRow(cfg, options.Model)
	upstreamModel := s.resolveFinalUpstreamModel(cfg, selected, "openai")
	// Strip query credentials from display metadata. The actual configured URL is used upstream.
	displayURL, _ := url.Parse(endpoint)
	displayURL.RawQuery = ""
	displayURL.Fragment = ""
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("X-Accel-Buffering", "no")
	write := func(event kimiEvent) bool {
		event.Time = time.Now().UTC().Format(time.RFC3339Nano)
		data, _ := json.Marshal(event)
		if _, err := fmt.Fprintf(c.Writer, "data: %s\n\n", data); err != nil {
			return false
		}
		c.Writer.Flush()
		return true
	}
	meta := gin.H{"version": kimiSuiteVersion, "channel_id": cfg.ID, "channel": cfg.Name, "key_index": key.keyIndex, "model": options.Model, "upstream_model": upstreamModel, "url": displayURL.String(), "total": len(cases), "concurrency": options.Concurrency, "timeout_seconds": options.TimeoutSeconds, "upload_mib": options.UploadMiB, "upload_limit_seconds": options.UploadLimitSeconds}
	if !write(kimiEvent{Type: "queued", Message: "兼容性测试已进入本次运行队列", Meta: meta}) {
		return
	}
	if !write(kimiEvent{Type: "running", Message: "开始执行兼容性用例"}) {
		return
	}
	events := make(chan kimiEvent, 24)
	jobs := make(chan kimiCase, len(cases))
	for _, item := range cases {
		jobs <- item
	}
	close(jobs)
	emit := func(e kimiEvent) bool {
		select {
		case events <- e:
			return true
		case <-ctx.Done():
			return false
		}
	}
	var workers sync.WaitGroup
	for i := 0; i < options.Concurrency; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for item := range jobs {
				if ctx.Err() != nil {
					return
				}
				if !emit(kimiEvent{Type: "case_started", ID: item.ID, Message: "开始测试：" + item.Name}) {
					return
				}
				result := s.runKimiCase(ctx, cfg, key.requestCredential, endpoint, upstreamModel, item, options)
				if !emit(kimiEvent{Type: "case_finished", ID: item.ID, Message: item.Name + " · " + result.Status, Result: &result}) {
					return
				}
			}
		}()
	}
	go func() { workers.Wait(); close(events) }()
	heartbeat := time.NewTicker(10 * time.Second)
	defer heartbeat.Stop()
	passed, failed := 0, 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-heartbeat.C:
			if _, err := io.WriteString(c.Writer, ": heartbeat\n\n"); err != nil {
				return
			}
			c.Writer.Flush()
		case event, ok := <-events:
			if !ok {
				verdict := "passed"
				if failed > 0 {
					verdict = "failed"
				}
				write(kimiEvent{Type: "completed", Message: fmt.Sprintf("运行完成：通过 %d，失败 %d", passed, failed), Meta: gin.H{"passed": passed, "failed": failed, "verdict": verdict}})
				return
			}
			if event.Result != nil {
				if event.Result.Status == "passed" {
					passed++
				} else {
					failed++
				}
			}
			if !write(event) {
				return
			}
		}
	}
}

func (s *Server) runKimiCase(parent context.Context, cfg *model.Config, key, endpoint, modelName string, item kimiCase, options kimiRunRequest) (result kimiResult) {
	start := time.Now()
	result = kimiResult{ID: item.ID, Status: "failed", Expected: item.Expected, UploadMS: -1}
	defer func() {
		result.DurationMS = float64(time.Since(start).Microseconds()) / 1000
		if key != "" {
			result.Response = strings.ReplaceAll(result.Response, key, "[REDACTED]")
			result.Actual = strings.ReplaceAll(result.Actual, key, "[REDACTED]")
		}
	}()
	ctx, cancel := context.WithTimeout(parent, time.Duration(options.TimeoutSeconds)*time.Second)
	defer cancel()
	body := kimiPayload(item, modelName, options)
	result.Request = kimiPreview(body)
	data, err := json.Marshal(body)
	if err != nil {
		result.Actual = "构造请求失败"
		return
	}
	result.RequestBytes = len(data)
	var traceMu sync.Mutex
	var uploadStart time.Time
	uploadMS := -1.0
	trace := &httptrace.ClientTrace{
		GotConn: func(httptrace.GotConnInfo) { traceMu.Lock(); uploadStart = time.Now(); traceMu.Unlock() },
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			traceMu.Lock()
			defer traceMu.Unlock()
			if info.Err == nil && !uploadStart.IsZero() {
				uploadMS = float64(time.Since(uploadStart).Microseconds()) / 1000
			}
		},
	}
	req, err := http.NewRequestWithContext(httptrace.WithClientTrace(ctx, trace), http.MethodPost, endpoint, bytes.NewReader(data))
	if err != nil {
		result.Actual = "构造上游请求失败"
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	applyHeaderRules(req.Header, cfg.HeaderRules())
	injectAPIKeyHeaders(req, key, "openai")
	release, waitErr := s.waitForUpstreamRequest(ctx, cfg)
	resp, err := s.doReservedUpstreamRequest(cfg, req, release, waitErr)
	traceMu.Lock()
	result.UploadMS = uploadMS
	traceMu.Unlock()
	if err != nil {
		if ctx.Err() != nil {
			result.Actual = "请求已取消或超时：" + ctx.Err().Error()
		} else {
			result.Actual = "上游连接或传输失败"
		}
		return
	}
	defer func() { _ = resp.Body.Close() }()
	result.HTTPStatus = resp.StatusCode
	response, err := io.ReadAll(io.LimitReader(resp.Body, (2<<20)+1))
	traceMu.Lock()
	result.UploadMS = uploadMS
	traceMu.Unlock()
	if err != nil {
		result.Actual = "读取上游响应失败（连接中断或超时）"
		return
	}
	preview := response
	if len(preview) > 32<<10 {
		preview = preview[:32<<10]
	}
	result.Response = strings.ToValidUTF8(string(preview), "�")
	if len(response) > len(preview) {
		result.Response += "\n[响应预览已截断]"
	}
	if len(response) > 2<<20 {
		result.Actual = "上游响应超过 2 MiB 限制"
		return
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		result.Actual = fmt.Sprintf("上游返回 HTTP %d", resp.StatusCode)
		return
	}
	passed, actual := kimiAssert(item, response, result.UploadMS, options.UploadLimitSeconds)
	result.Actual = actual
	if passed {
		result.Status = "passed"
	}
	return
}
