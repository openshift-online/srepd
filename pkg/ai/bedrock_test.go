package ai

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewBedrockProvider_AuthPanicRecovery(t *testing.T) {
	cfg := Config{
		Provider: "anthropic-bedrock",
		Region:   "us-east-1",
	}
	_, err := newBedrockProvider(cfg)
	if err != nil {
		assert.Contains(t, err.Error(), "auth failed")
	}
}

func TestNewBedrockProvider_DefaultModel(t *testing.T) {
	assert.Equal(t, "us.anthropic.claude-sonnet-4-6", bedrockDefaultModel)
}

// noAWSConfigFiles points AWS_CONFIG_FILE and AWS_SHARED_CREDENTIALS_FILE at
// nonexistent paths so the AWS SDK config chain never reads the developer's
// (or CI runner's) real ~/.aws/config or ~/.aws/credentials.
func noAWSConfigFiles(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(dir, "no-such-config"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(dir, "no-such-credentials"))
	t.Setenv("AWS_PROFILE", "")
	t.Setenv("AWS_REGION", "")
	t.Setenv("AWS_DEFAULT_REGION", "")
}

func TestResolveBedrockRegion_ConfigTakesPrecedence(t *testing.T) {
	noAWSConfigFiles(t)
	t.Setenv("AWS_REGION", "us-west-2")
	t.Setenv("AWS_DEFAULT_REGION", "eu-west-1")

	cfg := Config{Region: "ap-southeast-2"}

	assert.Equal(t, "ap-southeast-2", resolveBedrockRegion(cfg))
}

func TestResolveBedrockRegion_FromEnvAWSRegion(t *testing.T) {
	noAWSConfigFiles(t)
	t.Setenv("AWS_REGION", "us-west-2")
	t.Setenv("AWS_DEFAULT_REGION", "eu-west-1")

	cfg := Config{}

	assert.Equal(t, "us-west-2", resolveBedrockRegion(cfg))
}

func TestResolveBedrockRegion_FromEnvAWSDefaultRegion(t *testing.T) {
	noAWSConfigFiles(t)
	t.Setenv("AWS_DEFAULT_REGION", "eu-west-1")

	cfg := Config{}

	assert.Equal(t, "eu-west-1", resolveBedrockRegion(cfg))
}

// TestResolveBedrockRegion_FromSDKConfigChain is the one case that actually
// exercises aws-sdk-go-v2/config's LoadDefaultConfig — the exact call a
// version bump of that module could silently break.
func TestResolveBedrockRegion_FromSDKConfigChain(t *testing.T) {
	noAWSConfigFiles(t)

	dir := t.TempDir()
	configFile := filepath.Join(dir, "config")
	require.NoError(t, os.WriteFile(configFile, []byte("[default]\nregion = ap-northeast-1\n"), 0o600))

	t.Setenv("AWS_CONFIG_FILE", configFile)
	t.Setenv("AWS_PROFILE", "default")
	// Leave AWS_SHARED_CREDENTIALS_FILE pointed at a nonexistent path (set
	// by noAWSConfigFiles) so this never reads real credentials.

	cfg := Config{}

	assert.Equal(t, "ap-northeast-1", resolveBedrockRegion(cfg))
}

func TestResolveBedrockRegion_NoneFound(t *testing.T) {
	noAWSConfigFiles(t)

	cfg := Config{}

	assert.NotPanics(t, func() {
		assert.Equal(t, "", resolveBedrockRegion(cfg))
	})
}
