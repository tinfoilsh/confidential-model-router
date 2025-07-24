package manager

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tinfoilsh/verifier/attestation"
)

func TestVerifyRepo(t *testing.T) {
	measurement, tag, hwMeasurements, err := verifyRepo("tinfoilsh/confidential-llama-mistral-qwen-turbo", "v0.0.5")
	require.NoError(t, err)
	require.NotNil(t, measurement)
	assert.NotEmpty(t, "v0.0.5", tag)
	assert.Greater(t, len(hwMeasurements), 1)
	assert.Equal(t, attestation.SnpTdxMultiPlatformV1, measurement.Type)
	assert.Len(t, measurement.Registers, 3)
	assert.Equal(t, "527087d8e19a1ac33f59f6e1c0d80840c17ac10fbfcd49a223f23535a273536e6c975657c7d8bb857cb4c0a307bcbbb1", measurement.Registers[0])
	assert.Equal(t, "10a05f3fba7d66babcc8a8143451443a564963ced77c7fa126f004857753f87c318720e29e9ed2f46c8753b44b01004d", measurement.Registers[1])
	assert.Equal(t, "dffcc198136047fdf9ce8ad7672ea3f9884e626d81cd12d25c1e5fa23c736a69afe9953ababb8432ceeb1c2701bea00b", measurement.Registers[2])
}
