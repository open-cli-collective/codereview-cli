package llmadapters

import (
	"fmt"
	"strings"
)

func validateFastMode(adapter string, supportedModels []string, req Request) error {
	if !req.Fast {
		return nil
	}
	model := strings.TrimSpace(req.Model)
	for _, supported := range supportedModels {
		if model == supported {
			return nil
		}
	}
	if len(supportedModels) == 0 {
		return fmt.Errorf("llm: fast mode is unsupported by adapter %s", adapter)
	}
	return fmt.Errorf("llm: fast mode is unsupported by adapter %s for model %q; supported models: %s", adapter, model, strings.Join(supportedModels, ", "))
}
