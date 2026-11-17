package harness

import (
	"fmt"
	"maps"
	"os"

	"github.com/angelnicolasc/graymatter/internal/providerurl"
)

// PreparedRun contains the agent definition and endpoint resolved before a run
// opens a store or launches a background process.
type PreparedRun struct {
	cfg     RunConfig
	def     *AgentDef
	baseURL string
}

// Prepare validates the definition and snapshots the URL without opening a store.
func Prepare(cfg RunConfig) (*PreparedRun, error) {
	if cfg.DataDir == "" {
		cfg.DataDir = ".graymatter"
	}
	if cfg.MaxRetries <= 0 {
		cfg.MaxRetries = 3
	}
	if cfg.Stdout == nil {
		cfg.Stdout = os.Stdout
	}
	if cfg.Stderr == nil {
		cfg.Stderr = os.Stderr
	}
	cfg.Inputs = maps.Clone(cfg.Inputs)
	cfg.Store = nil // The caller supplies the store to PreparedRun.Run.

	def, err := ParseAgentFile(cfg.AgentFile, cfg.Inputs)
	if err != nil {
		return nil, fmt.Errorf("parse agent file: %w", err)
	}
	prepared := &PreparedRun{cfg: cfg, def: def}
	if cfg.llmDoer == nil {
		base := providerurl.Select("", providerurl.AnthropicEnv, providerurl.AnthropicDefault)
		endpoint, err := providerurl.Parse("anthropic", base)
		if err != nil {
			return nil, err
		}
		prepared.baseURL = endpoint.String()
	}
	return prepared, nil
}

// AnthropicBaseURL is the effective URL passed to a background child.
func (p *PreparedRun) AnthropicBaseURL() string { return p.baseURL }
