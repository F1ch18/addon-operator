package global_hooks

import "github.com/flant/addon-operator/sdk"

func init() {
	sdk.Register(&GoHook{})
}

type GoHook struct {
	sdk.CommonGoHook
}

func (h *GoHook) Metadata() sdk.HookMetadata {
	return h.CommonMetadataFromRuntime()
}

func (h *GoHook) Config() *sdk.HookConfig {
	return h.CommonGoHook.Config(&sdk.HookConfig{
		YamlConfig: `
configVersion: v1
`,
	})
}
