package app

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"ccLoad/internal/config"
	"ccLoad/internal/model"
	"ccLoad/internal/protocol"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Normalization is explicitly scoped to the selected URL and actual upstream
// protocol. Never infer this nonstandard accounting convention from token ratios.
func normalizeAnthropicCacheResponse(resp *http.Response, cfg *model.Config, baseURL string, upstream protocol.Protocol) *anthropicCacheBody {
	if resp == nil || resp.Body == nil || cfg == nil || upstream != protocol.Anthropic || resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil
	}
	enabled := false
	for _, endpoint := range cfg.URLs {
		if endpoint.RuntimeURL() == baseURL {
			enabled = endpoint.AnthropicInputTokensIncludeCache
			break
		}
	}
	if !enabled {
		return nil
	}
	body := &anthropicCacheBody{
		ReadCloser: resp.Body,
		reader:     bufio.NewReader(resp.Body),
		stream:     strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream"),
	}
	resp.Body = body
	resp.ContentLength = -1
	resp.Header.Del("Content-Length")
	resp.Header.Del("ETag")
	resp.Header.Del("Content-MD5")
	return body
}

// Read synchronously transforms one SSE event at a time. There is no producer
// goroutine; cancellation closes the original body through the forwarding layer.
// Raw debug capture sits below this wrapper and retains the original evidence.
type anthropicCacheBody struct {
	io.ReadCloser
	reader   *bufio.Reader
	stream   bool
	probed   bool
	pending  []byte
	err      error
	usage    usageAccumulator // raw cumulative snapshots, never normalized twice
	hasInput bool
}

func (b *anthropicCacheBody) inputTokens() int {
	// Sequential subtraction cannot overflow even for inconsistent upstream counts.
	return max(0, max(0, b.usage.InputTokens-b.usage.CacheReadInputTokens)-b.usage.CacheCreationInputTokens)
}

func (b *anthropicCacheBody) normalizeJSON(raw []byte) []byte {
	if !gjson.ValidBytes(raw) {
		return raw
	}
	path := "usage"
	if gjson.GetBytes(raw, "type").String() == "message_start" {
		path = "message.usage"
	}
	value := gjson.GetBytes(raw, path)
	if !value.IsObject() {
		return raw
	}
	var usage map[string]any
	if json.Unmarshal([]byte(value.Raw), &usage) != nil {
		return raw
	}
	if _, ok := usageTokenCount(usage["input_tokens"]); ok {
		b.hasInput = true
	}
	// Retain the existing handling of zero placeholders and cumulative cache
	// creation buckets. A later cache-only snapshot must also update input_tokens.
	b.usage.applyAnthropicOrResponsesUsage(usage)
	if !b.hasInput {
		return raw
	}
	out, err := sjson.SetBytes(raw, path+".input_tokens", b.inputTokens())
	if err != nil {
		return raw
	}
	// Emit canonical aggregate counts as well, including split-only cache writes.
	out, _ = sjson.SetBytes(out, path+".cache_read_input_tokens", b.usage.CacheReadInputTokens)
	out, _ = sjson.SetBytes(out, path+".cache_creation_input_tokens", b.usage.CacheCreationInputTokens)
	return out
}

func (b *anthropicCacheBody) normalizeEvent(raw []byte) []byte {
	lines := bytes.Split(raw, []byte("\n"))
	var data [][]byte
	for _, line := range lines {
		line = bytes.TrimSuffix(line, []byte("\r"))
		if bytes.HasPrefix(line, []byte("data:")) {
			data = append(data, bytes.TrimPrefix(line[5:], []byte(" ")))
		}
	}
	payload := bytes.Join(data, []byte("\n"))
	normalized := b.normalizeJSON(payload)
	if bytes.Equal(payload, normalized) {
		return raw
	}
	var out bytes.Buffer
	written := false
	for i, line := range lines {
		if bytes.HasPrefix(line, []byte("data:")) {
			if written {
				continue
			}
			out.WriteString("data: ")
			out.Write(normalized)
			written = true
		} else {
			out.Write(line)
		}
		if i < len(lines)-1 {
			out.WriteByte('\n')
		}
	}
	return out.Bytes()
}

func (b *anthropicCacheBody) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if !b.probed {
		b.probed = true
		// Some relays mislabel SSE as JSON, or return JSON to a streaming
		// request. Inspect the first non-space byte without consuming it.
		for n := 1; n <= 512; n++ {
			prefix, err := b.reader.Peek(n)
			if err != nil {
				break
			}
			switch prefix[n-1] {
			case ' ', '\t', '\r', '\n':
				continue
			case '{', '[':
				b.stream = false
			case 'e', 'd', ':':
				b.stream = true
			}
			break
		}
	}
	for len(b.pending) == 0 && b.err == nil {
		if !b.stream {
			raw, err := io.ReadAll(io.LimitReader(b.reader, int64(config.DefaultMaxBodyBytes)+1))
			if len(raw) > config.DefaultMaxBodyBytes {
				b.err = fmt.Errorf("Anthropic response exceeds normalization size limit")
				break
			}
			if err != nil {
				b.err = err
				break
			}
			b.pending, b.err = b.normalizeJSON(raw), io.EOF
			break
		}
		var event []byte
		continuedLine := false
		for {
			line, err := b.reader.ReadSlice('\n')
			if len(event)+len(line) > config.DefaultMaxBodyBytes {
				b.err = fmt.Errorf("Anthropic SSE event exceeds normalization size limit")
				break
			}
			event = append(event, line...)
			if err == bufio.ErrBufferFull {
				continuedLine = true
				continue
			}
			if err != nil {
				b.err = err
				break
			}
			if !continuedLine && (bytes.Equal(line, []byte("\n")) || bytes.Equal(line, []byte("\r\n"))) {
				break
			}
			continuedLine = false
		}
		if b.err == nil || b.err == io.EOF {
			b.pending = b.normalizeEvent(event)
		}
	}
	if len(b.pending) > 0 {
		n := copy(p, b.pending)
		b.pending = b.pending[n:]
		return n, nil
	}
	return 0, b.err
}
