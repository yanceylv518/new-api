package common

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTaskSecretRejectsEphemeralSecrets(t *testing.T) {
	previous := CryptoSecret
	t.Cleanup(func() { CryptoSecret = previous })
	for _, secret := range []string{"", generatedSessionSecret, generatedCryptoSecret} {
		CryptoSecret = secret
		assert.False(t, HasPersistentTaskSecret())
		sealed, err := SealTaskSecret("fixture-upstream-key")
		assert.Error(t, err)
		assert.Empty(t, sealed)
		sealed, err = SealTaskSecret("")
		assert.NoError(t, err)
		assert.Empty(t, sealed)
	}
	CryptoSecret = "fixture-persistent-task-secret"
	assert.True(t, HasPersistentTaskSecret())
}

func TestTaskSecretSurvivesProcessRestart(t *testing.T) {
	executable, err := os.Executable()
	require.NoError(t, err)
	run := func(mode, secret, ciphertext string) ([]byte, error) {
		ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
		defer cancel()
		command := exec.CommandContext(ctx, executable, "-test.run=^TestTaskSecretRestartHelper$")
		command.Env = os.Environ()
		command.Env = append(command.Env, "TASK_SECRET_RESTART_TEST="+mode, "TASK_SECRET_FIXTURE="+secret, "TASK_SECRET_CIPHERTEXT="+ciphertext)
		return command.CombinedOutput()
	}
	sealed, err := run("seal", "fixture-persistent-task-secret", "")
	require.NoError(t, err, "%s", sealed)
	ciphertext := strings.TrimSuffix(string(sealed), "PASS\n")
	require.True(t, strings.HasPrefix(ciphertext, taskSealedPrefix))
	assert.NotContains(t, ciphertext, "fixture-upstream-key")
	output, err := run("open", "fixture-persistent-task-secret", ciphertext)
	require.NoError(t, err, "%s", output)
	_, err = run("open", "different-fixture-task-secret", ciphertext)
	assert.Error(t, err)
}

func TestTaskSecretRestartHelper(t *testing.T) {
	mode := os.Getenv("TASK_SECRET_RESTART_TEST")
	if mode == "" {
		return
	}
	CryptoSecret = os.Getenv("TASK_SECRET_FIXTURE")
	if mode == "seal" {
		sealed, err := SealTaskSecret("fixture-upstream-key")
		require.NoError(t, err)
		fmt.Print(sealed)
		return
	}
	opened, err := OpenTaskSecret(os.Getenv("TASK_SECRET_CIPHERTEXT"))
	require.NoError(t, err)
	assert.Equal(t, "fixture-upstream-key", opened)
}
