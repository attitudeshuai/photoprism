package commands

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestThumbsCommand_Flags(t *testing.T) {
	names := make(map[string]bool)

	for _, f := range ThumbsCommand.Flags {
		for _, n := range f.Names() {
			names[n] = true
		}
	}

	assert.True(t, names["force"], "force flag must be registered")
	assert.True(t, names["f"], "force alias must be registered")
	assert.True(t, names["originals"], "originals flag must be registered")
	assert.True(t, names["o"], "originals alias must be registered")
	assert.True(t, names["pending"], "pending flag must be registered")
	assert.True(t, names["p"], "pending alias must be registered")
}
