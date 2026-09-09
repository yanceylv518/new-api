package controller

import (
	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
)

// GetUserModelPricingOverview 返回管理员可见的用户折扣汇总，页面可一次展示每个用户的全部规则。
func GetUserModelPricingOverview(c *gin.Context) {
	pageInfo := common.GetPageQuery(c)
	result, err := model.GetUserModelPricingOverview(
		c.Request.Context(),
		c.Query("keyword"),
		c.GetInt("role"),
		pageInfo.GetStartIdx(),
		pageInfo.GetPageSize(),
		c.Query("summary") == "true",
	)
	if err != nil {
		common.ApiError(c, err)
		return
	}

	common.ApiSuccess(c, gin.H{
		"items":        result.Items,
		"total":        result.TotalUsers,
		"page":         pageInfo.GetPage(),
		"page_size":    pageInfo.GetPageSize(),
		"total_rules":  result.TotalRules,
		"total_models": result.TotalModels,
	})
}

// GetUserModelPricingRulePage 仅供管理员按需查看规则，不加载编辑目录或运行时计费缓存。
func GetUserModelPricingRulePage(c *gin.Context) {
	user, ok := getManageableUser(c)
	if !ok {
		return
	}
	page := common.GetPageQuery(c).GetPage()
	items, total, err := model.GetUserModelPricingRulePage(c.Request.Context(), user.Id, c.GetInt("role"), c.Query("keyword"), page)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	common.ApiSuccess(c, gin.H{"items": items, "total": total, "page": page, "page_size": 20})
}
