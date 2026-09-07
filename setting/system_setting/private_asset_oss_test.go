package system_setting

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNormalizeAndValidatePrivateAssetOSSSettings(t *testing.T) {
	settings, err := NormalizeAndValidatePrivateAssetOSSSettings(PrivateAssetOSSSettings{
		Region:          " cn-hangzhou ",
		Endpoint:        "https://oss-cn-hangzhou.aliyuncs.com/",
		Bucket:          "private-assets",
		Prefix:          "/tenant/assets/",
		AccessKeyID:     "test-id",
		AccessKeySecret: "test-secret",
	})
	require.NoError(t, err)
	assert.Equal(t, "cn-hangzhou", settings.Region)
	assert.Equal(t, "https://oss-cn-hangzhou.aliyuncs.com", settings.Endpoint)
	assert.Equal(t, "tenant/assets/", settings.Prefix)
}

func TestNormalizeAndValidatePrivateAssetOSSSettingsRejectsUnsafeEndpoint(t *testing.T) {
	_, err := NormalizeAndValidatePrivateAssetOSSSettings(PrivateAssetOSSSettings{
		Region:          "cn-hangzhou",
		Endpoint:        "http://oss-cn-hangzhou.aliyuncs.com",
		Bucket:          "private-assets",
		Prefix:          "private-assets/",
		AccessKeyID:     "test-id",
		AccessKeySecret: "test-secret",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "HTTPS")
}

func TestNormalizeAndValidatePrivateAssetOSSSettingsRejectsParentPrefix(t *testing.T) {
	_, err := NormalizeAndValidatePrivateAssetOSSSettings(PrivateAssetOSSSettings{
		Region:          "cn-hangzhou",
		Endpoint:        "https://oss-cn-hangzhou.aliyuncs.com",
		Bucket:          "private-assets",
		Prefix:          "private-assets/../other",
		AccessKeyID:     "test-id",
		AccessKeySecret: "test-secret",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid path segments")
}
