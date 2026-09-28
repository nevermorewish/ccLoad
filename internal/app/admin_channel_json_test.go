package app

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"

	"ccLoad/internal/model"
)

func importJSONFixture(t *testing.T, server *Server, data []byte) (int, string) {
	t.Helper()
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, err := writer.CreateFormFile("file", "channels.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	req := newRequest(http.MethodPost, "/admin/channels/import.json", body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	c, w := newTestContext(t, req)
	server.HandleImportChannelsJSON(c)
	return w.Code, w.Body.String()
}

func TestAdminChannelJSONExportImportRoundTrip(t *testing.T) {
	source := newInMemoryServer(t)
	cfg, err := source.store.CreateConfig(context.Background(), &model.Config{
		Name: "json-backup", URLs: model.ChannelURLs{{URL: "https://api.example.com"}},
		AuthType: model.AuthTypeAPIKey, Enabled: true, Priority: 17,
		ModelEntries:          []model.ModelEntry{{Model: "test-model"}},
		ScheduledCheckEnabled: true, ScheduledCheckIntervalMinutes: 15, ScheduledCheckStartTime: "08:30",
		DailyCostLimit: 12.5, ProxyURL: "http://proxy.example.com:8080", AvailableTimeStart: "09:00", AvailableTimeEnd: "18:00",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := source.store.CreateAPIKeysBatch(context.Background(), []*model.APIKey{{ChannelID: cfg.ID, KeyIndex: 0, APIKey: "sk-json-secret", Note: "primary", AllowedModels: []string{"test-model"}, CostMultiplier: 1.2}}); err != nil {
		t.Fatal(err)
	}
	c, w := newTestContext(t, newRequest(http.MethodGet, "/admin/channels/export.json", nil))
	source.HandleExportChannelsJSON(c)
	if w.Code != http.StatusOK {
		t.Fatalf("export status %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "sk-json-secret") {
		t.Fatal("export omitted API key")
	}
	var doc channelJSONDocument
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Version != 1 || len(doc.Channels) != 1 {
		t.Fatalf("unexpected export: %+v", doc)
	}
	target := newInMemoryServer(t)
	status, body := importJSONFixture(t, target, w.Body.Bytes())
	if status != http.StatusOK {
		t.Fatalf("import status %d: %s", status, body)
	}
	configs, err := target.store.ListConfigs(context.Background())
	if err != nil || len(configs) != 1 {
		t.Fatalf("imported configs=%v, err=%v", configs, err)
	}
	got := configs[0]
	if got.Name != cfg.Name || got.DailyCostLimit != 12.5 || got.ProxyURL != cfg.ProxyURL || got.AvailableTimeStart != "09:00" || !got.ScheduledCheckEnabled {
		t.Fatalf("imported config lost settings: %+v", got)
	}
	keys, err := target.store.GetAPIKeys(context.Background(), got.ID)
	if err != nil || len(keys) != 1 || keys[0].APIKey != "sk-json-secret" || keys[0].Note != "primary" {
		t.Fatalf("imported keys=%v, err=%v", keys, err)
	}
	status, body = importJSONFixture(t, target, w.Body.Bytes())
	if status != http.StatusOK || !strings.Contains(body, `"updated":1`) {
		t.Fatalf("repeat import status %d: %s", status, body)
	}
}

func TestAdminChannelJSONImportRejectsInvalidDocument(t *testing.T) {
	server := newInMemoryServer(t)
	for _, data := range []string{`{"version":2,"channels":[]}`, `{"version":1,"channels":[{"config":{"name":"bad","urls":[]}}]}`} {
		status, _ := importJSONFixture(t, server, []byte(data))
		if status != http.StatusBadRequest {
			t.Fatalf("status=%d for %s", status, data)
		}
	}
}
