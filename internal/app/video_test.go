package app

import (
	"bytes"
	"ccLoad/internal/model"
	"ccLoad/internal/protocol"
	"context"
	"encoding/base64"
	"fmt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"image/jpeg"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestVideoBodyFormats(t *testing.T) {
	for _, tc := range []struct {
		name, field, block, imagePath, imageKind string
		format                                   protocol.Protocol
	}{
		{"chat URL", "messages", `{"type":"video_url","video_url":{"url":"https://example.com/a.mp4"}}`, "image_url.url", "image_url", protocol.OpenAI},
		{"chat string URL", "messages", `{"type":"video_url","video_url":"https://example.com/a.mp4"}`, "image_url.url", "image_url", protocol.OpenAI},
		{"responses", "input", `{"type":"input_video","video_url":"https://example.com/a.mp4"}`, "image_url", "input_image", protocol.Codex},
		{"claude upload", "messages", `{"type":"video","source":{"type":"base64","media_type":"video/mp4","data":"YWJj"}}`, "source.data", "image", protocol.Anthropic},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(fmt.Sprintf(`{"seed":9007199254740993,"%s":[{"role":"user","content":[{"type":"text","text":"original"},%s,{"type":"image_url","image_url":{"url":"original.jpg"}}]}]}`, tc.field, tc.block))
			out, err := convertVideoBody(context.Background(), body, tc.format, func(ctx context.Context, ref string) ([]videoFrame, error) {
				require.NotEmpty(t, ref)
				return []videoFrame{{Seconds: 0, JPEG: []byte("first")}, {Seconds: 5, JPEG: []byte("last")}}, nil
			})
			require.NoError(t, err)
			assert.Equal(t, "9007199254740993", gjson.GetBytes(out, "seed").Raw)
			parts := gjson.GetBytes(out, tc.field+".0.content").Array()
			require.Len(t, parts, 7)
			assert.Equal(t, "original", parts[0].Get("text").String())
			assert.Equal(t, tc.imageKind, parts[3].Get("type").String())
			assert.Contains(t, parts[3].Get(tc.imagePath).String(), base64.StdEncoding.EncodeToString([]byte("first")))
			assert.Contains(t, parts[4].Get("text").String(), "5.00 seconds")
			assert.Equal(t, "original.jpg", parts[6].Get("image_url.url").String())
		})
	}
}

func TestVideoBodyPreservationAndErrors(t *testing.T) {
	untouched := []byte(`{ "messages": [{"role":"user","content":"https://example.com/video.mp4"}], "tools": [] }`)
	out, err := convertVideoBody(context.Background(), untouched, protocol.OpenAI, nil)
	require.NoError(t, err)
	assert.Equal(t, untouched, out)
	_, err = convertVideoBody(context.Background(), []byte(`{"messages":[{"content":[{"type":"video_url"}]}]}`), protocol.OpenAI, nil)
	assert.ErrorContains(t, err, "requires")
	_, err = convertVideoBody(context.Background(), []byte(`{"messages":[{"content":[{"type":"video_url","video_url":"x"}]}]}`), protocol.OpenAI, func(context.Context, string) ([]videoFrame, error) { return nil, fmt.Errorf("decode failed") })
	assert.ErrorContains(t, err, "decode failed")
	for _, ref := range []string{"file:///etc/passwd", "ftp://example.com/video", "data:text/plain;base64,YQ==", "data:video/mp4;base64,!!!"} {
		_, err := loadVideo(context.Background(), ref)
		assert.Error(t, err)
	}
}

func TestVideoProxyConversion(t *testing.T) {
	for _, tool := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip(tool + " unavailable")
		}
	}
	file := filepath.Join(t.TempDir(), "clip.mp4")
	output, err := exec.Command("ffmpeg", "-v", "error", "-f", "lavfi", "-i", "color=c=red:s=96x64:r=2:d=2", "-c:v", "mpeg4", file).CombinedOutput()
	require.NoError(t, err, string(output))
	data, err := os.ReadFile(file)
	require.NoError(t, err)
	ref := "data:video/mp4;base64," + base64.StdEncoding.EncodeToString(data)
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprint(enabled), func(t *testing.T) {
			env := setupProxyTestEnv(t, []testChannel{{name: "video", upstreamProtocol: "openai", models: "kimi-k3", apiKey: "upstream-key", customRequestRules: &model.CustomRequestRules{VideoToImages: enabled}}}, map[int]string{0: "https://gateway.example"})
			calls := 0
			body := fmt.Sprintf(`{"model":"kimi-k3","messages":[{"role":"user","content":[{"type":"video_url","video_url":{"url":%q}}]}],"seed":9007199254740993}`, ref)
			env.server.client = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
				calls++
				wire, err := io.ReadAll(req.Body)
				require.NoError(t, err)
				assert.Equal(t, "9007199254740993", gjson.GetBytes(wire, "seed").Raw)
				if enabled {
					assert.NotContains(t, string(wire), `"video_url"`)
					images := gjson.GetBytes(wire, "messages.0.content").Array()
					require.Len(t, images, 3)
					imageRef := images[2].Get("image_url.url").String()
					jpg, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(imageRef, "data:image/jpeg;base64,"))
					require.NoError(t, err)
					img, err := jpeg.Decode(bytes.NewReader(jpg))
					require.NoError(t, err)
					r, g, b, _ := img.At(10, 10).RGBA()
					assert.Greater(t, r, g*2)
					assert.Greater(t, r, b*2)
				} else {
					assert.JSONEq(t, body, string(wire))
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"id":"test","object":"chat.completion","model":"kimi-k3","choices":[{"index":0,"message":{"role":"assistant","content":"red"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":1,"total_tokens":11}}`))}, nil
			})}
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
			req.Header.Set("Authorization", "Bearer test-api-key")
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			env.engine.ServeHTTP(w, req)
			assert.Equal(t, 200, w.Code, w.Body.String())
			assert.Equal(t, 1, calls)
		})
	}
}

func TestVideoFetchPolicyAndLimits(t *testing.T) {
	for _, ip := range []string{"127.0.0.1", "10.1.2.3", "169.254.169.254", "::1", "fc00::1", "100.64.0.1"} {
		assert.False(t, publicVideoIP(net.ParseIP(ip)), ip)
	}
	_, err := loadVideo(context.Background(), "http://127.0.0.1/private.mp4")
	assert.Error(t, err)
	calls := 0
	extract := func(context.Context, string) ([]videoFrame, error) {
		calls++
		return []videoFrame{{JPEG: []byte("frame")}}, nil
	}
	block := `{"type":"video_url","video_url":"https://example.com/a.mp4"}`
	_, err = convertVideoBody(context.Background(), []byte(`{"messages":[{"content":[`+block+`,`+block+`]}]}`), protocol.OpenAI, extract)
	require.NoError(t, err)
	assert.Equal(t, 1, calls)
	_, err = convertVideoBody(context.Background(), []byte(`{"messages":[{"content":[`+block+`,`+block+`,`+block+`]}]}`), protocol.OpenAI, extract)
	assert.ErrorContains(t, err, "at most 2")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = extractVideoFrames(ctx, "https://example.com/video.mp4")
	assert.Error(t, err)
	rules := &model.CustomRequestRules{VideoToImages: true}
	assert.False(t, rules.IsEmpty())
	assert.True(t, rules.Clone().VideoToImages)
}

func TestVideoGeminiAndPersistence(t *testing.T) {
	body := []byte(`{"contents":[{"role":"user","parts":[{"text":"describe"},{"inlineData":{"mimeType":"video/mp4","data":"YWJj"}}]}]}`)
	out, err := convertVideoBody(context.Background(), body, protocol.Gemini, func(context.Context, string) ([]videoFrame, error) { return []videoFrame{{JPEG: []byte("jpeg")}}, nil })
	require.NoError(t, err)
	assert.Equal(t, "image/jpeg", gjson.GetBytes(out, "contents.0.parts.3.inlineData.mimeType").String())
	assert.False(t, gjson.GetBytes(out, "contents.0.parts.1.type").Exists())
	srv := newInMemoryServer(t)
	cfg, err := srv.store.CreateConfig(context.Background(), &model.Config{Name: "video-setting", URLs: model.ChannelURLs{{URL: "https://example.com"}}, Enabled: true})
	require.NoError(t, err)
	for _, enabled := range []bool{false, true, false} {
		cfg.CustomRequestRules = &model.CustomRequestRules{VideoToImages: enabled}
		_, err = srv.store.UpdateConfig(context.Background(), cfg.ID, cfg)
		require.NoError(t, err)
		saved, err := srv.store.GetConfig(context.Background(), cfg.ID)
		require.NoError(t, err)
		actual := saved.CustomRequestRules != nil && saved.CustomRequestRules.VideoToImages
		assert.Equal(t, enabled, actual)
	}
}
