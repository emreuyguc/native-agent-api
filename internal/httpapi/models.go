package httpapi

import (
	"context"
	"slices"
	"strings"
	"sync"

	"github.com/emreuyguc/native-agent-api/internal/agent"
)

// modelCatalog caches the native model list so requests can be validated against each model's
// reasoning efforts without starting the CLI for every run.
type modelCatalog struct {
	mu     sync.Mutex
	models []agent.Model
}

func (c *modelCatalog) refresh(ctx context.Context, a agent.Agent) ([]agent.Model, error) {
	reporter, ok := a.(agent.ModelReporter)
	if !ok {
		return nil, agent.ErrUnsupported
	}
	models, err := reporter.Models(ctx)
	if err != nil {
		return nil, err
	}
	slices.SortFunc(models, func(a, b agent.Model) int { return strings.Compare(a.ID, b.ID) })
	for i := range models {
		if models[i].Efforts == nil {
			models[i].Efforts = []string{}
		}
	}
	c.mu.Lock()
	c.models = models
	c.mu.Unlock()

	return models, nil
}

// validateEffort rejects an effort the selected model does not list. The catalog is loaded on
// first use; a model the catalog does not contain (a full native model name, for example) or a
// catalog that cannot be loaded is left to the native CLI's own validation.
func (c *modelCatalog) validateEffort(ctx context.Context, a agent.Agent, model, effort string) error {
	c.mu.Lock()
	models := c.models
	c.mu.Unlock()
	if models == nil {
		loaded, err := c.refresh(ctx, a)
		if err != nil {
			return nil //nolint:nilerr // The native CLI still validates the request.
		}
		models = loaded
	}
	index := slices.IndexFunc(models, func(m agent.Model) bool { return m.ID == model })
	if index < 0 || len(models[index].Efforts) == 0 || slices.Contains(models[index].Efforts, effort) {
		return nil
	}

	return agent.Invalid("effort %q is not supported by model %s; supported: %s",
		effort, model, strings.Join(models[index].Efforts, ", "))
}
