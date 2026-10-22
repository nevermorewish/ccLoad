package responses

import (
	"regexp"
	"strconv"
	"strings"
)

var promptCacheModelVersion = regexp.MustCompile(`^gpt-([0-9]+)(?:\.([0-9]+))?(?:-|$)`)

// preservesExplicitPromptCache selects the GPT-5.6+ wire contract, including
// variants and dated snapshots. It does not claim a particular upstream
// account supports caching; that upstream still validates the intact request.
func preservesExplicitPromptCache(model string) bool {
	version := promptCacheModelVersion.FindStringSubmatch(strings.ToLower(strings.TrimSpace(model)))
	if version == nil {
		return false
	}
	major, err := strconv.Atoi(version[1])
	if err != nil {
		return false
	}
	if major != 5 {
		return major > 5
	}
	minor, err := strconv.Atoi(version[2])
	return err == nil && minor >= 6
}
