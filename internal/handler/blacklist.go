package handler

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	grokpkg "github.com/grok-fireworks-reg/internal/grok"
	fwpkg "github.com/grok-fireworks-reg/internal/fireworks"
	orpkg "github.com/grok-fireworks-reg/internal/openrouter"
	nvpkg "github.com/grok-fireworks-reg/internal/novita"
	"github.com/grok-fireworks-reg/pkg/tempmail"
)

// BlacklistEntry 黑名单条目
type BlacklistEntry struct {
	Domain   string `json:"domain"`
	BannedAt string `json:"banned_at"`
	Ago      string `json:"ago"`
}

func getBlacklist(platform string) *tempmail.DomainBlacklist {
	switch platform {
	case "grok":
		return grokpkg.GetBlacklist()
	case "fireworks":
		return fwpkg.GetBlacklist()
	case "openrouter":
		return orpkg.GetBlacklist()
	case "novita":
		return nvpkg.GetBlacklist()
	}
	return nil
}

// GetBlacklist GET /api/blacklist/:platform
func GetBlacklist(c *gin.Context) {
	platform := c.Param("platform")
	bl := getBlacklist(platform)
	if bl == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "unknown platform"})
		return
	}
	entries := formatBlacklist(bl.GetAll())
	c.JSON(http.StatusOK, gin.H{"platform": platform, "domains": entries, "count": len(entries)})
}

// ClearBlacklist DELETE /api/blacklist/:platform
func ClearBlacklist(c *gin.Context) {
	platform := c.Param("platform")
	bl := getBlacklist(platform)
	if bl == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "unknown platform"})
		return
	}
	bl.Clear()
	c.JSON(http.StatusOK, gin.H{"ok": true, "message": platform + " blacklist cleared"})
}

// AddBlacklist POST /api/blacklist/:platform — 添加域名到黑名单
// Body: {"domains": "domain1.com,domain2.com"} 或 {"domains": "domain1.com"}
func AddBlacklist(c *gin.Context) {
	platform := c.Param("platform")
	bl := getBlacklist(platform)
	if bl == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "unknown platform"})
		return
	}

	var req struct {
		Domains string `json:"domains"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Domains == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "domains required"})
		return
	}

	added := 0
	for _, d := range strings.Split(req.Domains, ",") {
		d = strings.TrimSpace(d)
		if d != "" {
			bl.Ban(d)
			added++
		}
	}

	c.JSON(http.StatusOK, gin.H{"ok": true, "added": added})
}

// RemoveBlacklistDomains POST /api/blacklist/:platform/remove — 移除指定域名
// Body: {"domains": ["domain1.com", "domain2.com"]}
func RemoveBlacklistDomains(c *gin.Context) {
	platform := c.Param("platform")
	bl := getBlacklist(platform)
	if bl == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "unknown platform"})
		return
	}

	var req struct {
		Domains []string `json:"domains"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || len(req.Domains) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "domains required"})
		return
	}

	bl.UnbanMultiple(req.Domains)
	c.JSON(http.StatusOK, gin.H{"ok": true, "removed": len(req.Domains)})
}

// 保留旧的独立函数（兼容）
func GetGrokBlacklist(c *gin.Context)      { c.Params = append(c.Params, gin.Param{Key: "platform", Value: "grok"}); GetBlacklist(c) }
func GetFireworksBlacklist(c *gin.Context)  { c.Params = append(c.Params, gin.Param{Key: "platform", Value: "fireworks"}); GetBlacklist(c) }
func GetOpenRouterBlacklist(c *gin.Context) { c.Params = append(c.Params, gin.Param{Key: "platform", Value: "openrouter"}); GetBlacklist(c) }
func GetNovitaBlacklist(c *gin.Context)     { c.Params = append(c.Params, gin.Param{Key: "platform", Value: "novita"}); GetBlacklist(c) }
func ClearGrokBlacklist(c *gin.Context)      { c.Params = append(c.Params, gin.Param{Key: "platform", Value: "grok"}); ClearBlacklist(c) }
func ClearFireworksBlacklist(c *gin.Context)  { c.Params = append(c.Params, gin.Param{Key: "platform", Value: "fireworks"}); ClearBlacklist(c) }
func ClearOpenRouterBlacklist(c *gin.Context) { c.Params = append(c.Params, gin.Param{Key: "platform", Value: "openrouter"}); ClearBlacklist(c) }
func ClearNovitaBlacklist(c *gin.Context)     { c.Params = append(c.Params, gin.Param{Key: "platform", Value: "novita"}); ClearBlacklist(c) }

func formatBlacklist(all map[string]time.Time) []BlacklistEntry {
	entries := make([]BlacklistEntry, 0, len(all))
	now := time.Now()
	for domain, bannedAt := range all {
		ago := now.Sub(bannedAt).Truncate(time.Minute)
		entries = append(entries, BlacklistEntry{
			Domain:   domain,
			BannedAt: bannedAt.Format(time.RFC3339),
			Ago:      ago.String(),
		})
	}
	return entries
}
