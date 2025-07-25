package manager

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEnclaveManager_AddEnclave(t *testing.T) {
	manager, err := NewEnclaveManager([]byte(`models:
  audio-processing: tinfoilsh/confidential-audio-processing
  deepseek-r1-70b: tinfoilsh/confidential-deepseek-r1-70b-prod
  llama3-3-70b-turbo: tinfoilsh/confidential-llama-mistral-qwen-turbo
  qwen2-5-72b-turbo: tinfoilsh/confidential-llama-mistral-qwen-turbo
  mistral-small-3-1-24b-turbo: tinfoilsh/confidential-llama-mistral-qwen-turbo
`), "")
	require.NoError(t, err)
	require.NotNil(t, manager)

	assert.Greater(t, len(manager.hardwareMeasurements), 1)

	numModels := 0
	manager.models.Range(func(key, value any) bool {
		numModels++
		return true
	})
	assert.Equal(t, 5, numModels)

	// Add TDX enclave
	assert.NoError(t, manager.AddEnclave("llama3-3-70b-turbo", "large.inf3.tinfoil.sh"))
	model, found := manager.GetModel("llama3-3-70b-turbo")
	require.True(t, found)
	assert.Equal(t, 1, len(model.Enclaves))
	assert.Equal(t, "large.inf3.tinfoil.sh", model.Enclaves[0].host)
}
