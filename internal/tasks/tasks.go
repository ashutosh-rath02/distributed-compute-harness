// Package tasks implements the agent side of the typed task catalog
// (internal/catalog): one native Go handler per built-in type, so a typed
// task runs the same way on every platform — no shell, no PATH, no
// external program. Handlers work only inside their workload's working
// directory: they read their inputs and write their declared outputs.
package tasks

import (
	"context"
	"io"

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
	// that depends on something external (a local model runtime, say) is
	// offered only where it actually works.
	Available(ctx context.Context) error
	// Run does the work. It must honor ctx (cancel and timeout).
	Run(ctx context.Context, env Env) error
}

var registry = map[domain.CapabilityName]Handler{
	"system.identity": identity{},
	"cpu.burn":        cpuBurn{},
	"image.resize":    imageResize{},
	"archive.zip":     archiveZip{},
	"file.hash":       fileHash{},
	"text.count":      textCount{},
}

// Lookup returns the handler for a catalog type.
func Lookup(name domain.CapabilityName) (Handler, bool) {
	h, ok := registry[name]
	return h, ok
}

// Register adds a handler (used for types whose availability depends on
// the device, and by tests).
func Register(name domain.CapabilityName, h Handler) { registry[name] = h }

// Capabilities lists the catalog types this agent can run right now, at
// their catalog versions, for the manifest.
func Capabilities(ctx context.Context) []domain.Capability {
	var out []domain.Capability
	for _, t := range catalog.Types() {
		if h, ok := registry[t.Name]; ok && h.Available(ctx) == nil {
			out = append(out, domain.Capability{Name: t.Name, Version: t.Version})
		}
	}
	return out
}
