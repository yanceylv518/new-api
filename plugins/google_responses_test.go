package plugins_test

import (
	"testing"

	"github.com/QuantumNous/new-api/pkg/jsplugin"
	builtinplugins "github.com/QuantumNous/new-api/plugins"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestVeoVideoProtocolPreservesReferenceAndResolution(t *testing.T) {
	for _, key := range []string{"google", "vertex-ai"} {
		t.Run(key, func(t *testing.T) {
			source, err := builtinplugins.Source(key)
			require.NoError(t, err)
			plugin, err := jsplugin.NewRegistry().RegisterFactory(source, jsplugin.Options{Key: key})
			require.NoError(t, err)
			for _, field := range []string{"input_reference", "image"} {
				t.Run(field, func(t *testing.T) {
					value, err := plugin.Engine.CallPath(t.Context(), "protocols", []string{"openai_video", "decodeRequest"}, map[string]any{
						"model": "veo-3.0-generate-001", "body": map[string]any{"kind": "json", "value": map[string]any{
							"prompt": "animate the supplied frame", "seconds": 8, "resolution": "1080P", field: "data:image/png;base64,aGVsbG8=",
						}},
					})
					require.NoError(t, err)
					intent := decodePluginValue(t, value)
					ctx := map[string]any{"baseUrl": "https://provider.example", "apiKey": "fixture", "upstreamModel": "veo-3.0-generate-001", "auth": map[string]any{"projectId": "fixture"}, "requestBody": intent["requestBody"]}
					value, err = plugin.Engine.Call(t.Context(), "buildSubmitRequest", ctx)
					require.NoError(t, err)
					request := decodePluginValue(t, value)
					body := request["body"].(map[string]any)
					instances := body["instances"].([]any)
					assert.NotEmpty(t, instances[0].(map[string]any)["image"])
					assert.Equal(t, "1080p", body["parameters"].(map[string]any)["resolution"])
					value, err = plugin.Engine.Call(t.Context(), "extractUsage", ctx)
					require.NoError(t, err)
					assert.Equal(t, "1080p", decodePluginValue(t, value)["resolution"])
				})
			}
		})
	}
}

func TestGoogleResponsesProtocol(t *testing.T) {
	testVideoResponsesProtocol(t, videoResponsesTestCase{
		pluginKey: "google",
		model:     "veo-3.1-fast-generate-preview",
		requestBody: map[string]any{
			"model": "veo-3.1-fast-generate-preview",
			"input": []any{map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "input_text", "text": "animate this frame"},
				map[string]any{"type": "input_image", "image_url": "data:image/png;base64,aGVsbG8="},
			}}},
			"seconds": 8,
			"size":    "1280x720",
		},
		wantAction: "image_to_video",
		wantRequest: map[string]any{
			"model":    "veo-3.1-fast-generate-preview",
			"prompt":   "animate this frame",
			"images":   []any{"data:image/png;base64,aGVsbG8="},
			"duration": float64(8),
			"size":     "1280x720",
			"metadata": map[string]any{},
		},
		wantUsageKeys:  []string{"resolution", "seconds"},
		wantVendorName: "gemini",
	})
}
