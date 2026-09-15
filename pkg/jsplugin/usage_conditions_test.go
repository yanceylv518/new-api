package jsplugin

import (
	"fmt"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 显示顺序必须通过宿主解析、克隆和 JSON 输出，且不能接受跨语言不稳定的数值。
func TestUsageDisplayOrderMetadata(t *testing.T) {
	for _, tc := range []struct {
		value string
		valid bool
	}{
		{"0", true}, {"-2147483648", true}, {"2147483647", true},
		{"1.5", false}, {`"1"`, false}, {"null", false},
		{"NaN", false}, {"Infinity", false}, {"2147483648", false}, {"-2147483649", false},
	} {
		t.Run(tc.value, func(t *testing.T) {
			source := routingTestPluginSource("ordered-pricing", 0, `["model"]`,
				`usageSchema:{seconds:{type:"number",unit:"second",displayOrder:`+tc.value+`}},`, "")
			plugin, err := CompilePlugin(source, Options{})
			if !tc.valid {
				require.ErrorContains(t, err, "displayOrder")
				return
			}
			require.NoError(t, err)
			encoded, err := common.Marshal(CloneUsageSchema(plugin.Meta.UsageSchema))
			require.NoError(t, err)
			if tc.value == "0" {
				assert.JSONEq(t, `{"seconds":{"type":"number","unit":"second"}}`, string(encoded))
			} else {
				assert.JSONEq(t, fmt.Sprintf(`{"seconds":{"type":"number","unit":"second","displayOrder":%s}}`, tc.value), string(encoded))
			}
		})
	}
}

// 定价条件必须随插件元数据往返，并在读取副本时保持注册快照隔离。
func TestUsageConditionsMetadataAndClone(t *testing.T) {
	plugin, err := CompilePlugin(routingTestPluginSource("conditional-pricing", 0, `["model"]`, `usageSchema: {
		operation: {enum: ["generation", "regeneration", "context_ir"]},
		resolution: {enum: ["768P", "2K"], when: [
			{field: "operation", values: ["generation"]},
			{field: "operation", values: ["regeneration"], enum: ["2K"]}
		]},
		seconds: {type: "number", unit: "second", when: [{field: "operation", values: ["generation", "regeneration"]}]}
	},`, ""), Options{})
	require.NoError(t, err)
	schema := plugin.Meta.UsageSchema
	encoded, err := common.Marshal(schema["resolution"])
	require.NoError(t, err)
	assert.JSONEq(t, `{"enum":["768P","2K"],"when":[{"field":"operation","values":["generation"]},{"field":"operation","values":["regeneration"],"enum":["2K"]}]}`, string(encoded))
	cloned := CloneUsageSchema(schema)
	cloned["resolution"].When[0].Values[0] = "changed"
	cloned["resolution"].When[1].Enum[0] = "changed"
	assert.Equal(t, "generation", schema["resolution"].When[0].Values[0])
	assert.Equal(t, "2K", schema["resolution"].When[1].Enum[0])
}

// 拒绝未声明取值、循环依赖和无效类型，防止价格界面收到不可解释的约束。
func TestUsageConditionsRejectInvalidDeclarations(t *testing.T) {
	for _, tc := range []struct{ name, declaration, want string }{
		{"empty", `{enum:["2K"],when:[]}`, "1 to 16"},
		{"unknown_selector", `{enum:["2K"],when:[{field:"missing",values:["generation"]}]}`, "unconditional enum"},
		{"self_reference", `{enum:["2K"],when:[{field:"resolution",values:["2K"]}]}`, "unconditional enum"},
		{"unknown_value", `{enum:["2K"],when:[{field:"operation",values:["unsupported"]}]}`, "declared selector values"},
		{"duplicate_value", `{enum:["2K"],when:[{field:"operation",values:["generation","generation"]}]}`, "unique declared selector values"},
		{"unknown_enum", `{enum:["2K"],when:[{field:"operation",values:["generation"],enum:["768P"]}]}`, "unique declared values"},
		{"numeric_enum", `{type:"number",unit:"second",when:[{field:"operation",values:["generation"],enum:["2K"]}]}`, "narrow an enum field"},
		{"unknown_property", `{enum:["2K"],when:[{field:"operation",values:["generation"],otherwise:true}]}`, "unknown property"},
		{"dependent_selector", `{enum:["2K"],when:[{field:"dependent",values:["x"]}]}`, "unconditional enum"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := routingTestPluginSource("invalid-condition", 0, `["model"]`, `usageSchema:{
				operation:{enum:["generation","regeneration"]},
				dependent:{enum:["x"],when:[{field:"operation",values:["generation"]}]},
				resolution:`+tc.declaration+`},`, "")
			_, err := CompilePlugin(source, Options{})
			require.ErrorContains(t, err, tc.want)
		})
	}
}
