package app

import (
	"embed"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/tidwall/gjson"
)

//go:embed kimi_fixtures/blue.png kimi_fixtures/flower.mp4 kimi_fixtures/black.mp4
var kimiFixtures embed.FS

const kimiSuiteVersion = "ccload-kimi-k3/1.0.1"

type kimiCase struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Expected string `json:"expected"`
}

var kimiCases = []kimiCase{
	{"kimi.k3.dynamic_tool_loading_with_global_tool", "动态工具与顶层全局工具共存", "调用 get_weather，city=北京，finish_reason=tool_calls"},
	{"kimi.k3.video_base64_upload_speed", "大体积 Base64 视频请求体上传速度", "请求上传完成且不超过设定阈值，HTTP 2xx 并返回有效文本"},
	{"kimi.k3.thinking_default", "默认启用思考", "返回非空思考内容和最终回答"},
	{"kimi.k3.tool_call_ability", "默认天气工具调用能力", "调用 get_weather，city=北京，finish_reason=tool_calls"},
	{"kimi.k3.dynamic_tool_loading", "通过 system 消息动态加载天气工具", "调用 get_weather，city=北京，finish_reason=tool_calls"},
	{"kimi.k3.video_base64", "Base64 视频花朵识别", "回答包含 花 或 flower"},
	{"kimi.k3.video_base64_2", "Base64 视频黑色识别", "回答包含 黑 或 black"},
	{"kimi.k3.image_base64", "Base64 图片蓝色识别", "回答包含 蓝 或 blue"},
	{"kimi.k3.thinking_off_chat_template_enable_thinking", "chat_template_kwargs.enable_thinking=false 关闭思考", "返回非空回答，不包含思考内容"},
	{"kimi.k3.thinking_off_enable_thinking", "enable_thinking=false 关闭思考", "返回非空回答，不包含思考内容"},
	{"kimi.k3.thinking_off_chat_template_thinking", "chat_template_kwargs.thinking=false 关闭思考", "返回非空回答，不包含思考内容"},
	{"kimi.k3.image_url_probe", "image_url 兼容性探测", "读取公网蓝色测试图片，回答包含 蓝 或 blue"},
}

func kimiTool(name, property, description string) map[string]any {
	return map[string]any{"type": "function", "function": map[string]any{"name": name, "description": description, "parameters": map[string]any{"type": "object", "properties": map[string]any{property: map[string]any{"type": "string"}}, "required": []string{property}}}}
}

func kimiPayload(item kimiCase, model string, options kimiRunRequest) map[string]any {
	id := strings.TrimPrefix(item.ID, "kimi.k3.")
	prompt := "计算 17×23+46，给出最终答案。"
	body := map[string]any{"model": model, "stream": false, "max_tokens": 4096}
	messages := []map[string]any{}
	if strings.Contains(id, "tool") {
		prompt = "请查询北京现在的天气，使用提供的天气工具，city 参数使用中文城市名。"
		weather := kimiTool("get_weather", "city", "查询指定城市的当前天气")
		if id == "tool_call_ability" {
			body["tools"] = []any{weather}
		} else {
			// Kimi rejects non-empty content alongside dynamic tools in the same
			// system message. Instructions belong in a separate message.
			messages = append(messages, map[string]any{"role": "system", "content": "", "tools": []any{weather}})
			body["tool_choice"] = "required"
			if id == "dynamic_tool_loading_with_global_tool" {
				body["tools"] = []any{kimiTool("get_stock_price", "symbol", "查询股票价格")}
			}
		}
	}
	var content any = prompt
	if strings.HasPrefix(id, "video_") || strings.HasPrefix(id, "image_") {
		kind, asset, mime, prompt := "image_url", "blue.png", "image/png", "只回答图片的主要颜色。"
		if strings.HasPrefix(id, "video_") {
			kind, asset, mime, prompt = "video_url", "black.mp4", "video/mp4", "只回答视频画面的主要颜色。"
			if id == "video_base64" {
				asset, prompt = "flower.mp4", "描述视频中的主要物体。"
			}
		}
		mediaURL := options.ImageURL
		if id != "image_url_probe" {
			media, _ := kimiFixtures.ReadFile("kimi_fixtures/" + asset)
			if id == "video_base64_upload_speed" {
				// A valid ISO-BMFF free atom grows the transport payload without changing decoded frames.
				target := options.UploadMiB * 1024 * 1024 * 3 / 4
				padding := make([]byte, target-len(media))
				binary.BigEndian.PutUint32(padding[:4], uint32(len(padding)))
				copy(padding[4:8], "free")
				media = append(media, padding...)
			}
			mediaURL = "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(media)
		}
		content = []any{map[string]any{"type": "text", "text": prompt}, map[string]any{"type": kind, kind: map[string]any{"url": mediaURL}}}
	}
	messages = append(messages, map[string]any{"role": "user", "content": content})
	body["messages"] = messages
	switch id {
	case "thinking_off_chat_template_enable_thinking":
		body["chat_template_kwargs"] = map[string]any{"enable_thinking": false}
	case "thinking_off_enable_thinking":
		body["enable_thinking"] = false
	case "thinking_off_chat_template_thinking":
		body["chat_template_kwargs"] = map[string]any{"thinking": false}
	}
	return body
}

func kimiAssert(item kimiCase, raw []byte, uploadMS float64, limitSeconds int) (bool, string) {
	if !json.Valid(raw) {
		return false, "响应不是合法 JSON"
	}
	root := gjson.ParseBytes(raw)
	msg := root.Get("choices.0.message")
	if root.Get("error").Exists() || !msg.IsObject() {
		return false, "缺少 choices[0].message 或响应含 error"
	}
	content := msg.Get("content").String()
	thinking := false
	for _, name := range []string{"reasoning_content", "reasoning", "thinking", "reasoning_details"} {
		v := msg.Get(name)
		if v.Exists() && v.Type != gjson.Null && strings.TrimSpace(v.String()) != "" && v.Raw != "[]" {
			thinking = true
		}
	}
	if strings.Contains(strings.ToLower(content), "<think>") || strings.Contains(strings.ToLower(content), "<thinking>") {
		thinking = true
	}
	id := strings.TrimPrefix(item.ID, "kimi.k3.")
	if strings.Contains(id, "tool") {
		for _, call := range msg.Get("tool_calls").Array() {
			args := call.Get("function.arguments").String()
			if call.Get("type").String() == "function" && call.Get("function.name").String() == "get_weather" && json.Valid([]byte(args)) && gjson.Get(args, "city").String() == "北京" && root.Get("choices.0.finish_reason").String() == "tool_calls" {
				return true, "get_weather(city=北京)，finish_reason=tool_calls"
			}
		}
		return false, "未返回符合要求的 get_weather 工具调用（city=北京、finish_reason=tool_calls）"
	}
	if strings.TrimSpace(content) == "" {
		return false, "最终回答为空"
	}
	if id == "thinking_default" || strings.HasPrefix(id, "thinking_off_") {
		expected := id == "thinking_default"
		return thinking == expected, fmt.Sprintf("实际包含思考内容：%t；预期：%t", thinking, expected)
	}
	if id == "video_base64_upload_speed" {
		return uploadMS >= 0 && uploadMS <= float64(limitSeconds)*1000, fmt.Sprintf("上传 %.2f ms；阈值 %d 秒（-1 表示未观测到完整上传）", uploadMS, limitSeconds)
	}
	keywords := []string{"蓝", "blue"}
	if id == "video_base64" {
		keywords = []string{"花", "flower"}
	}
	if id == "video_base64_2" {
		keywords = []string{"黑", "black"}
	}
	for _, keyword := range keywords {
		if strings.Contains(strings.ToLower(content), keyword) {
			return true, "回答匹配关键词：" + keyword
		}
	}
	return false, "回答未匹配关键词：" + strings.Join(keywords, " / ")
}

// Compact media only in the report; upstream receives the original full payload.
func kimiPreview(value any) any {
	switch v := value.(type) {
	case string:
		if strings.HasPrefix(v, "data:") {
			prefix, _, _ := strings.Cut(v, ",")
			return fmt.Sprintf("%s,[omitted; %d characters]", prefix, len(v))
		}
		return v
	case map[string]any:
		result := map[string]any{}
		for k, item := range v {
			result[k] = kimiPreview(item)
		}
		return result
	case []map[string]any:
		result := make([]any, len(v))
		for i, item := range v {
			result[i] = kimiPreview(item)
		}
		return result
	case []any:
		result := make([]any, len(v))
		for i, item := range v {
			result[i] = kimiPreview(item)
		}
		return result
	default:
		return value
	}
}
