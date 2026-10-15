package model

import "testing"

func TestAnthropicCacheConventionPersistsPerURL(t *testing.T) {
	urls := ChannelURLs{{URL: "https://inclusive.example", AnthropicInputTokensIncludeCache: true}, {URL: "https://standard.example"}}
	value, err := urls.Value()
	if err != nil {
		t.Fatal(err)
	}
	var restored ChannelURLs
	if err = restored.Scan(value); err != nil {
		t.Fatal(err)
	}
	cloned := restored.Clone()
	if !cloned[0].AnthropicInputTokensIncludeCache || cloned[1].AnthropicInputTokensIncludeCache {
		t.Fatalf("lost endpoint scope: %+v", cloned)
	}
}
