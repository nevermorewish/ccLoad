package app

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"ccLoad/internal/model"
	"github.com/gin-gonic/gin"
)

// channelJSONConfig keeps OAuthCredential in an explicit field because Config
// deliberately omits private credentials from its normal JSON representation.
type channelJSONConfig struct {
	*model.Config
	OAuthCredential string `json:"oauth_credential,omitempty"`
}

func (c channelJSONConfig) MarshalJSON() ([]byte, error) {
	type configAlias model.Config
	value := struct {
		*configAlias
		OAuthCredential string `json:"oauth_credential,omitempty"`
	}{configAlias: (*configAlias)(c.Config), OAuthCredential: c.OAuthCredential}
	return json.Marshal(value)
}

func (c *channelJSONConfig) UnmarshalJSON(data []byte) error {
	type configAlias model.Config
	var value struct {
		*configAlias
		OAuthCredential string `json:"oauth_credential,omitempty"`
	}
	value.configAlias = (*configAlias)(&model.Config{})
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	c.Config = (*model.Config)(value.configAlias)
	c.OAuthCredential = value.OAuthCredential
	c.Config.OAuthCredential = value.OAuthCredential
	return nil
}

type channelJSONRecord struct {
	Config  channelJSONConfig `json:"config"`
	APIKeys []model.APIKey    `json:"api_keys,omitempty"`
}

type channelJSONDocument struct {
	Version    int                 `json:"version"`
	ExportedAt time.Time           `json:"exported_at"`
	Channels   []channelJSONRecord `json:"channels"`
}

func (s *Server) HandleExportChannelsJSON(c *gin.Context) {
	selectedIDs, err := parseChannelIDsQuery(c.Query("ids"))
	if err != nil {
		RespondError(c, http.StatusBadRequest, err)
		return
	}
	configs, err := s.store.ListConfigs(c.Request.Context())
	if err != nil {
		RespondError(c, http.StatusInternalServerError, err)
		return
	}
	allKeys, err := s.store.GetAllAPIKeys(c.Request.Context())
	if err != nil {
		RespondError(c, http.StatusInternalServerError, err)
		return
	}
	doc := channelJSONDocument{Version: 1, ExportedAt: time.Now(), Channels: make([]channelJSONRecord, 0, len(configs))}
	for _, cfg := range configs {
		if selectedIDs != nil {
			if _, ok := selectedIDs[cfg.ID]; !ok {
				continue
			}
		}
		keys := make([]model.APIKey, 0, len(allKeys[cfg.ID]))
		for _, key := range allKeys[cfg.ID] {
			if key != nil {
				keys = append(keys, *key)
			}
		}
		doc.Channels = append(doc.Channels, channelJSONRecord{Config: channelJSONConfig{Config: cfg, OAuthCredential: cfg.OAuthCredential}, APIKeys: keys})
	}
	body, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		RespondError(c, http.StatusInternalServerError, err)
		return
	}
	filename := fmt.Sprintf("channels-%s.json", time.Now().Format("20060102-150405"))
	c.Header("Content-Type", "application/json; charset=utf-8")
	c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s\"", filename))
	c.Header("Cache-Control", "no-cache")
	c.Data(http.StatusOK, "application/json; charset=utf-8", body)
}

func (s *Server) HandleImportChannelsJSON(c *gin.Context) {
	file, err := c.FormFile("file")
	if err != nil {
		RespondErrorMsg(c, http.StatusBadRequest, "缺少上传文件")
		return
	}
	if file.Size > 20<<20 {
		RespondErrorMsg(c, http.StatusBadRequest, "JSON 文件不能超过 20MB")
		return
	}
	src, err := file.Open()
	if err != nil {
		RespondError(c, http.StatusInternalServerError, err)
		return
	}
	defer src.Close()
	data, err := io.ReadAll(io.LimitReader(src, 20<<20+1))
	if err != nil {
		RespondError(c, http.StatusBadRequest, err)
		return
	}
	var doc channelJSONDocument
	if err := json.Unmarshal(data, &doc); err != nil {
		RespondErrorMsg(c, http.StatusBadRequest, "JSON 格式无效: "+err.Error())
		return
	}
	if doc.Version != 1 {
		RespondErrorMsg(c, http.StatusBadRequest, "不支持的 JSON 版本")
		return
	}
	if len(doc.Channels) == 0 {
		RespondErrorMsg(c, http.StatusBadRequest, "JSON 中没有渠道")
		return
	}
	channels := make([]*model.ChannelWithKeys, 0, len(doc.Channels))
	for i := range doc.Channels {
		record := &doc.Channels[i]
		if record.Config.Config == nil {
			RespondErrorMsg(c, http.StatusBadRequest, fmt.Sprintf("第%d个渠道缺少 config", i+1))
			return
		}
		cfg := record.Config.Config
		cfg.ID = 0 // Portable backups match existing channels by name.
		cfg.OAuthCredential = record.Config.OAuthCredential
		cfg.AuthType = model.NormalizeAuthType(cfg.AuthType)
		if cfg.AuthType == "" {
			RespondErrorMsg(c, http.StatusBadRequest, fmt.Sprintf("渠道 %q 的 auth_type 无效", cfg.Name))
			return
		}
		if err := cfg.URLs.Normalize(); err != nil {
			RespondErrorMsg(c, http.StatusBadRequest, fmt.Sprintf("渠道 %q 的 URL 无效: %v", cfg.Name, err))
			return
		}
		if _, err := model.ValidateModelEntries(cfg.ModelEntries); err != nil {
			RespondErrorMsg(c, http.StatusBadRequest, fmt.Sprintf("渠道 %q 的模型无效: %v", cfg.Name, err))
			return
		}
		if err := cfg.NormalizeScheduledCheckSchedule(); err != nil {
			RespondErrorMsg(c, http.StatusBadRequest, fmt.Sprintf("渠道 %q 的监测计划无效: %v", cfg.Name, err))
			return
		}
		if cfg.AuthType != model.AuthTypeAPIKey && strings.TrimSpace(cfg.OAuthCredential) == "" {
			RespondErrorMsg(c, http.StatusBadRequest, fmt.Sprintf("渠道 %q 缺少 OAuth 凭证", cfg.Name))
			return
		}
		if cfg.AuthType != model.AuthTypeAPIKey && len(record.APIKeys) > 0 {
			RespondErrorMsg(c, http.StatusBadRequest, fmt.Sprintf("OAuth 渠道 %q 不能包含 API Keys", cfg.Name))
			return
		}
		for keyIndex := range record.APIKeys {
			record.APIKeys[keyIndex].KeyIndex = keyIndex
			record.APIKeys[keyIndex].ChannelID = 0
		}
		channels = append(channels, &model.ChannelWithKeys{Config: cfg, APIKeys: record.APIKeys, FullConfig: true})
	}
	if err := s.prepareExistingOAuthChannelUpdates(c.Request.Context(), channels); err != nil {
		RespondError(c, http.StatusBadRequest, err)
		return
	}
	created, updated, err := s.store.ImportChannelBatch(c.Request.Context(), channels)
	if err != nil {
		RespondError(c, http.StatusBadRequest, err)
		return
	}
	s.InvalidateChannelListCache()
	s.InvalidateAllAPIKeysCache()
	s.invalidateCooldownCache()
	RespondJSON(c, http.StatusOK, ChannelImportSummary{Created: created, Updated: updated, Processed: len(channels)})
}

// Keep strconv referenced in older Go build tags where the filename helper is inlined.
var _ = strconv.IntSize
