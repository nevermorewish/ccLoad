package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/protocol"
)

const maxVideoBytes = 64 << 20

// Bound CPU and temporary-file usage independently of relay concurrency.
var videoSlots = make(chan struct{}, 2)

type videoFrame struct {
	Seconds float64
	JPEG    []byte
}

type videoExtractor func(context.Context, string) ([]videoFrame, error)

// Transform the final wire body, including passthrough and parameter overrides.
// Only message content is traversed: URLs inside tool arguments are not media.
func convertVideoBody(ctx context.Context, body []byte, format protocol.Protocol, extract videoExtractor) ([]byte, error) {
	var request map[string]any
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&request); err != nil {
		return nil, fmt.Errorf("invalid JSON for video conversion")
	}
	ctx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	cache := make(map[string][]videoFrame)
	changed := false
	videoCount := 0
	for _, field := range []string{"messages", "input", "contents"} {
		messages, _ := request[field].([]any)
		for _, raw := range messages {
			message, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			contentField := "content"
			if field == "contents" {
				contentField = "parts"
			}
			parts, ok := message[contentField].([]any)
			if !ok {
				continue
			}
			var output []any
			for _, part := range parts {
				block, _ := part.(map[string]any)
				ref, isVideo, err := videoReference(block)
				if err != nil {
					return nil, err
				}
				if !isVideo {
					output = append(output, part)
					continue
				}
				videoCount++
				if videoCount > 2 {
					return nil, fmt.Errorf("video conversion supports at most 2 videos per request")
				}
				frames, found := cache[ref]
				if !found {
					frames, err = extract(ctx, ref)
					if err != nil {
						return nil, fmt.Errorf("video %d: %w", videoCount, err)
					}
					cache[ref] = frames
				}
				if len(frames) == 0 {
					return nil, fmt.Errorf("video contains no decodable frames")
				}
				textType := "text"
				if format == protocol.Codex {
					textType = "input_text"
				}
				summary := map[string]any{"type": textType, "text": fmt.Sprintf("[Video %d: %d sampled frames in chronological order. Audio is not included.]", videoCount, len(frames))}
				if format == protocol.Gemini {
					delete(summary, "type")
				}
				output = append(output, summary)
				for _, frame := range frames {
					encoded := base64.StdEncoding.EncodeToString(frame.JPEG)
					label := map[string]any{"type": textType, "text": fmt.Sprintf("[Video %d at %.2f seconds]", videoCount, frame.Seconds)}
					if format == protocol.Gemini {
						delete(label, "type")
					}
					output = append(output, label)
					image := map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/jpeg;base64," + encoded}}
					switch format {
					case protocol.Gemini:
						image = map[string]any{"inlineData": map[string]any{"mimeType": "image/jpeg", "data": encoded}}
					case protocol.Anthropic:
						image = map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/jpeg", "data": encoded}}
					case protocol.Codex:
						image = map[string]any{"type": "input_image", "image_url": "data:image/jpeg;base64," + encoded}
					}
					output = append(output, image)
				}
				changed = true
			}
			message[contentField] = output
		}
	}
	if !changed {
		return body, nil
	}
	return json.Marshal(request)
}

func videoReference(block map[string]any) (string, bool, error) {
	for _, field := range []string{"inlineData", "inline_data", "fileData", "file_data"} {
		source, _ := block[field].(map[string]any)
		mime, _ := source["mimeType"].(string)
		if mime == "" {
			mime, _ = source["mime_type"].(string)
		}
		if !strings.HasPrefix(mime, "video/") {
			continue
		}
		if data, _ := source["data"].(string); data != "" {
			return "data:" + mime + ";base64," + data, true, nil
		}
		for _, name := range []string{"fileUri", "file_uri"} {
			if ref, _ := source[name].(string); ref != "" {
				return ref, true, nil
			}
		}
		return "", true, fmt.Errorf("video block requires an HTTP(S) URL or base64 video data")
	}
	kind, _ := block["type"].(string)
	var source any
	switch kind {
	case "video_url":
		source = block["video_url"]
	case "input_video", "video":
		source = block["video_url"]
		if source == nil {
			source = block["source"]
		}
		if source == nil {
			source = block["url"]
		}
	case "document":
		s, _ := block["source"].(map[string]any)
		mime, _ := s["media_type"].(string)
		if !strings.HasPrefix(mime, "video/") {
			return "", false, nil
		}
		source = s
	default:
		return "", false, nil
	}
	if ref, ok := source.(string); ok && ref != "" {
		return ref, true, nil
	}
	if s, ok := source.(map[string]any); ok {
		if ref, ok := s["url"].(string); ok && ref != "" {
			return ref, true, nil
		}
		data, _ := s["data"].(string)
		mime, _ := s["media_type"].(string)
		if data != "" && strings.HasPrefix(mime, "video/") {
			return "data:" + mime + ";base64," + data, true, nil
		}
	}
	return "", true, fmt.Errorf("video block requires an HTTP(S) URL or base64 video data")
}

func loadVideo(ctx context.Context, ref string) ([]byte, error) {
	if strings.HasPrefix(ref, "data:") {
		header, data, ok := strings.Cut(ref, ",")
		if !ok || !strings.HasPrefix(header, "data:video/") || !strings.HasSuffix(header, ";base64") {
			return nil, fmt.Errorf("video data must be a base64 video data URL")
		}
		if len(data) > base64.StdEncoding.EncodedLen(maxVideoBytes) {
			return nil, fmt.Errorf("video exceeds 64 MiB")
		}
		decoded, err := base64.StdEncoding.DecodeString(data)
		if err != nil {
			return nil, fmt.Errorf("invalid video base64")
		}
		if len(decoded) > maxVideoBytes {
			return nil, fmt.Errorf("video exceeds 64 MiB")
		}
		return decoded, nil
	}
	u, err := url.Parse(ref)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil {
		return nil, fmt.Errorf("video URL must use HTTP(S) without embedded credentials")
	}
	if err := validateVideoURL(u); err != nil {
		return nil, fmt.Errorf("video URL is blocked by fetch policy")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ref, nil)
	if err != nil {
		return nil, fmt.Errorf("invalid video URL")
	}
	resp, err := videoHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("video download failed or timed out")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("video download returned HTTP %d", resp.StatusCode)
	}
	if resp.ContentLength > maxVideoBytes {
		return nil, fmt.Errorf("video exceeds 64 MiB")
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxVideoBytes+1))
	if err != nil {
		return nil, fmt.Errorf("could not read video download")
	}
	if len(data) > maxVideoBytes {
		return nil, fmt.Errorf("video exceeds 64 MiB")
	}
	return data, nil
}

func extractVideoFrames(ctx context.Context, ref string) ([]videoFrame, error) {
	select {
	case videoSlots <- struct{}{}:
		defer func() { <-videoSlots }()
	case <-ctx.Done():
		return nil, fmt.Errorf("video processing timed out or was cancelled")
	}
	data, err := loadVideo(ctx, ref)
	if err != nil {
		return nil, err
	}
	// Pin the demuxer and forbid network protocols. Playlists must never cause
	// ffmpeg to fetch additional URLs or open arbitrary local resources.
	demuxer := ""
	switch {
	case len(data) >= 12 && bytes.Equal(data[4:8], []byte("ftyp")):
		demuxer = "mov"
	case bytes.HasPrefix(data, []byte{0x1a, 0x45, 0xdf, 0xa3}):
		demuxer = "matroska"
	case len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "AVI ":
		demuxer = "avi"
	default:
		return nil, fmt.Errorf("unsupported video container; use MP4, MOV, WebM, MKV or AVI")
	}
	dir, err := os.MkdirTemp("", "ccload-video-")
	if err != nil {
		return nil, fmt.Errorf("cannot create video workspace")
	}
	defer func() { _ = os.RemoveAll(dir) }()
	input := filepath.Join(dir, "input")
	if err := os.WriteFile(input, data, 0600); err != nil {
		return nil, fmt.Errorf("cannot save video")
	}
	probe := exec.CommandContext(ctx, "ffprobe", "-v", "error", "-protocol_whitelist", "file", "-f", demuxer, "-show_entries", "format=duration", "-of", "default=noprint_wrappers=1:nokey=1", input)
	durationBytes, err := probe.Output()
	if err != nil {
		return nil, fmt.Errorf("cannot probe video; ensure ffprobe is installed and the video is valid")
	}
	duration, err := strconv.ParseFloat(strings.TrimSpace(string(durationBytes)), 64)
	if err != nil || math.IsNaN(duration) || math.IsInf(duration, 0) || duration <= 0 || duration > 7200 {
		return nil, fmt.Errorf("video duration must be between 0 and 7200 seconds")
	}
	count := min(16, max(1, int(math.Ceil(duration/2))))
	interval := duration / float64(count)
	filter := fmt.Sprintf("fps=%.9f:start_time=0:round=up,scale=768:768:force_original_aspect_ratio=decrease", 1/interval)
	cmd := exec.CommandContext(ctx, "ffmpeg", "-nostdin", "-hide_banner", "-loglevel", "error", "-threads", "1", "-protocol_whitelist", "file", "-f", demuxer, "-i", input, "-map", "0:v:0", "-an", "-vf", filter, "-frames:v", strconv.Itoa(count), "-threads", "1", "-filter_threads", "1", "-q:v", "4", filepath.Join(dir, "frame-%02d.jpg"))
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("cannot extract video frames; ensure ffmpeg is installed and the video is valid")
	}
	var frames []videoFrame
	for i := range count {
		jpeg, err := os.ReadFile(filepath.Join(dir, fmt.Sprintf("frame-%02d.jpg", i+1)))
		if os.IsNotExist(err) {
			break
		}
		if err != nil || len(jpeg) == 0 || len(jpeg) > 1<<20 {
			return nil, fmt.Errorf("invalid extracted video frame")
		}
		frames = append(frames, videoFrame{Seconds: float64(i) * interval, JPEG: jpeg})
	}
	if len(frames) == 0 {
		return nil, fmt.Errorf("video contains no decodable frames")
	}
	return frames, nil
}

// The opt-in setting is stored in existing channel JSON; absent means disabled.
func prepareChannelVideoBody(ctx context.Context, cfg *model.Config, body []byte, format protocol.Protocol) ([]byte, error) {
	if cfg == nil || cfg.CustomRequestRules == nil || !cfg.CustomRequestRules.VideoToImages {
		return body, nil
	}
	// Multipart uploads and non-JSON endpoints are outside message conversion.
	if !json.Valid(body) {
		return body, nil
	}
	converted, err := convertVideoBody(ctx, body, format, extractVideoFrames)
	if err != nil {
		return nil, &requestValidationError{message: err.Error()}
	}
	return converted, nil
}

// A dedicated client never inherits upstream credentials, proxies or private
// host overrides. Validate every resolved address at connection time (including
// redirects) to prevent DNS rebinding into internal networks.
var videoHTTPClient = &http.Client{
	Transport: &http.Transport{
		Proxy:                 nil,
		DialContext:           dialVideoURL,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 20 * time.Second,
		IdleConnTimeout:       30 * time.Second,
		MaxIdleConns:          4,
	},
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return fmt.Errorf("too many video redirects")
		}
		return validateVideoURL(req.URL)
	},
	Timeout: 90 * time.Second,
}

func validateVideoURL(u *url.URL) error {
	if u == nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" || u.User != nil {
		return fmt.Errorf("invalid video URL")
	}
	if port := u.Port(); port != "" && port != "80" && port != "443" && port != "8080" && port != "8443" {
		return fmt.Errorf("video URL port is not allowed")
	}
	return nil
}

func publicVideoIP(ip net.IP) bool {
	if ip == nil || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return false
	}
	if ipv4 := ip.To4(); ipv4 != nil {
		return ipv4[0] != 0 && ipv4[0] < 224 && (ipv4[0] != 100 || ipv4[1]&192 != 64)
	}
	return true
}

func dialVideoURL(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("video DNS lookup failed")
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("video host has no addresses")
	}
	for _, ip := range ips {
		if !publicVideoIP(ip.IP) {
			return nil, fmt.Errorf("video URL points to a non-public address")
		}
	}
	dialer := net.Dialer{Timeout: 15 * time.Second}
	for _, ip := range ips {
		conn, dialErr := dialer.DialContext(ctx, network, net.JoinHostPort(ip.IP.String(), port))
		if dialErr == nil {
			return conn, nil
		}
		err = dialErr
	}
	return nil, err
}
