// Package tasks implements the agent side of the typed task catalog
// (internal/catalog): one native Go handler per built-in type, so a typed
// task runs the same way on every platform — no shell, no PATH, no
// external program. Handlers work only inside their workload's working
// directory: they read their inputs and write their declared outputs.
// Types backed by a local runtime (Ollama, ollama.go) are offered only
// where that runtime answers.
package tasks

import (
	"context"
	"io"
	"sort"
	"time"

	"home-harness/internal/catalog"
	"home-harness/internal/domain"
)

// Env is what a handler runs with.
type Env struct {
	Dir     string            // the workload's working directory ("" when it declares no files)
	Params  map[string]string // canonical, already validated by catalog.Compile
	Inputs  []string          // input names, relative to Dir, in submission order
	Outputs []string          // declared output names, relative to Dir
	Stdout  io.Writer         // captured like a process's stdout (capped)
	Stderr  io.Writer
}

// Handler runs one task type.
type Handler interface {
	// Available reports whether this device can run the type right now
	// (nil = yes). The agent advertises only available types, so a type
	// that depends on something external (a local model runtime) is
	// offered only where it actually works. It may do I/O: callers must
	// not hold locks across it.
	Available(ctx context.Context) error
	// Run does the work. It must honor ctx (cancel and timeout).
	Run(ctx context.Context, env Env) error
}

// Describer is a handler with per-node detail to advertise as capability
// attributes (e.g. the models a local runtime has), which placement can
// match parameters against (catalog.Param.ChoicesAttr).
type Describer interface {
	Attributes(ctx context.Context) map[string]string
}

// Options configure an agent's registry.
type Options struct {
	// OllamaURL is where this device's Ollama listens (the device owner's
	// setting, never the manager's). Empty means the default local one.
	OllamaURL string

	tagsTTL time.Duration // how long Ollama's model list is reused (tests shorten it)
}

// Registry is one agent's set of handlers.
type Registry struct {
	handlers map[domain.CapabilityName]Handler
}

func builtinHandlers() map[domain.CapabilityName]Handler {
	return map[domain.CapabilityName]Handler{
		"system.identity": identity{},
		"cpu.burn":        cpuBurn{},
		"image.resize":    imageResize{},
		"archive.zip":     archiveZip{},
		"file.hash":       fileHash{},
		"text.count":      textCount{},
		"render.fractal":  renderFractal{},
		"image.stack":     imageStack{},
	}
}

// Builtins is a registry with the self-contained built-in types only.
func Builtins() *Registry { return &Registry{handlers: builtinHandlers()} }

// NewRegistry is every type this agent implements: the built-ins plus
// the local-model types, which advertise themselves only while Ollama
// answers at opts.OllamaURL.
func NewRegistry(opts Options) *Registry {
	r := Builtins()
	o := newOllama(opts.OllamaURL)
	if opts.tagsTTL != 0 {
		o.ttl = opts.tagsTTL
	}
	r.handlers["llm.generate"] = ollamaGenerate{o}
	r.handlers["llm.chat"] = ollamaChat{o}
	r.handlers["llm.pull"] = ollamaPull{o}
	r.handlers["llm.remove"] = ollamaRemove{o}
	r.handlers["llm.inventory"] = ollamaInventory{o}
	return r
}

// Lookup returns the handler for a catalog type.
func (r *Registry) Lookup(name domain.CapabilityName) (Handler, bool) {
	h, ok := r.handlers[name]
	return h, ok
}

// Capabilities lists the catalog types this agent can run right now, at
// their catalog versions, with any per-node attributes — for the
// manifest and capability updates. Sorted, so equal sets compare equal.
func (r *Registry) Capabilities(ctx context.Context) []domain.Capability {
	var out []domain.Capability
	for _, t := range catalog.Types() {
		h, ok := r.handlers[t.Name]
		if !ok || h.Available(ctx) != nil {
			continue
		}
		c := domain.Capability{Name: t.Name, Version: t.Version}
		if d, ok := h.(Describer); ok {
			c.Attributes = d.Attributes(ctx)
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

var builtins = Builtins()

// Lookup returns a built-in handler (the self-contained types).
func Lookup(name domain.CapabilityName) (Handler, bool) { return builtins.Lookup(name) }

// Capabilities lists the built-in types (see Registry.Capabilities).
func Capabilities(ctx context.Context) []domain.Capability { return builtins.Capabilities(ctx) }
