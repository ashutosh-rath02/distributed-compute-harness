// Package catalog is the typed task catalog (roadmap item 7): the
// allow-listed task types every agent implements natively (in Go, so the
// same on every platform) and the parameter schema each one takes. The
// manager and the agent link the same definitions: the manager validates
// and compiles a submission against them, and the agent re-validates what
// it is assigned before running it, so a version mismatch is refused
// rather than misread.
package catalog

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"home-harness/internal/domain"
)

// ErrInvalid is a submission that doesn't match its task type's schema.
var ErrInvalid = errors.New("catalog: invalid task")

// ParamType is a parameter's value type. Every value travels as a
// string (Workload.Params) in its canonical form.
type ParamType string

const (
	String ParamType = "string"
	Int    ParamType = "int"
	Number ParamType = "number"
	Bool   ParamType = "bool"
	Enum   ParamType = "enum"
)

// Param describes one parameter of a task type.
type Param struct {
	Name        string    `json:"name"`
	Type        ParamType `json:"type"`
	Title       string    `json:"title,omitempty"`
	Description string    `json:"description,omitempty"`
	Required    bool      `json:"required,omitempty"`
	Default     string    `json:"default,omitempty"`
	Min         *float64  `json:"min,omitempty"`
	Max         *float64  `json:"max,omitempty"`
	Enum        []string  `json:"enum,omitempty"`
	// Pattern (anchored) and MaxLength (default 256) bound string values,
	// which may never start with '-' unless AllowDash: a value is never
	// allowed to look like an option to whatever reads it.
	Pattern   string `json:"pattern,omitempty"`
	MaxLength int    `json:"maxLength,omitempty"`
	AllowDash bool   `json:"allowDash,omitempty"`
	// Multiline allows newlines and tabs (free text such as a prompt —
	// never an argv element).
	Multiline bool `json:"multiline,omitempty"`
	// ChoicesAttr names the capability attribute that lists this
	// parameter's valid values per node (e.g. "models"): placement only
	// picks a node whose list holds the value.
	ChoicesAttr string `json:"choicesAttr,omitempty"`
}

// Inputs bounds the files a task type takes.
type Inputs struct {
	Min         int      `json:"min"`
	Max         int      `json:"max"`
	Extensions  []string `json:"extensions,omitempty"` // lowercase, no dot; empty = any
	Description string   `json:"description,omitempty"`
}

// Type is one allow-listed task type.
type Type struct {
	Name        domain.CapabilityName `json:"name"`
	Version     string                `json:"version"`
	Title       string                `json:"title"`
	Description string                `json:"description"`
	Params      []Param               `json:"params,omitempty"`
	Inputs      Inputs                `json:"inputs"`
	// Outputs are output file name templates: {{param}} is a parameter's
	// value, {{in.stem}} the single input's name without its extension.
	Outputs []string `json:"outputs,omitempty"`
	// Reduce marks a type that makes sense as a job's fan-in step (it
	// takes many inputs and combines them).
	Reduce bool `json:"reduce,omitempty"`
	// Requirements are applied when a submission doesn't ask for more —
	// e.g. image decoding needs memory, which item 4 then reserves.
	Requirements domain.ResourceRequirements `json:"requirements,omitempty"`
	// OutputAtMostInputs: the output is about as big as all inputs
	// together (an archive), so the manager can refuse up front what
	// would exceed the upload budget.
	OutputAtMostInputs bool `json:"outputAtMostInputs,omitempty"`
	// MaxPerNode bounds how many of this type one node runs at once
	// (0 = only its slots): one local model generation at a time.
	MaxPerNode int `json:"maxPerNode,omitempty"`
	// Streams: the agent reports output while the task runs, not only at
	// the end (a model's answer, token by token).
	Streams bool `json:"streams,omitempty"`
	// DefaultMaxRuntimeSeconds is the per-attempt limit when policy sets
	// none for this type.
	DefaultMaxRuntimeSeconds int `json:"defaultMaxRuntimeSeconds,omitempty"`
}

func num(v float64) *float64 { return &v }

// Image limits: decoding allocates per pixel, and a tiny file can claim
// enormous dimensions (a decompression bomb), so both the source and the
// result are capped before any pixel is allocated.
const (
	MaxImagePixels = 40_000_000
)

var builtins = []Type{
	{
		Name: "system.identity", Version: "1", Title: "Device identity",
		Description: "Report the device's hostname, OS, architecture and CPU count. A quick check that a device runs work.",
	},
	{
		Name: "cpu.burn", Version: "1", Title: "CPU load test",
		Description: "Keep the device's CPUs busy for a while, to watch load move across the fleet.",
		Params: []Param{
			{Name: "seconds", Type: Int, Title: "Seconds", Default: "20", Min: num(1), Max: num(600)},
			{Name: "threads", Type: Int, Title: "Threads (0 = every CPU)", Default: "0", Min: num(0), Max: num(256)},
		},
	},
	{
		Name: "image.resize", Version: "1", Title: "Resize an image",
		Description: "Scale a JPEG, PNG or GIF to a new width (height follows the aspect ratio unless given). Run it once per photo as a job to spread a batch over the fleet.",
		Params: []Param{
			{Name: "width", Type: Int, Title: "Width (pixels)", Required: true, Default: "1024", Min: num(1), Max: num(10000)},
			{Name: "height", Type: Int, Title: "Height (0 = keep aspect)", Default: "0", Min: num(0), Max: num(10000)},
			{Name: "format", Type: Enum, Title: "Output format", Default: "jpg", Enum: []string{"jpg", "png"}},
			{Name: "quality", Type: Int, Title: "JPEG quality", Default: "85", Min: num(1), Max: num(100)},
		},
		Inputs:       Inputs{Min: 1, Max: 1, Extensions: []string{"jpg", "jpeg", "png", "gif"}, Description: "one image"},
		Outputs:      []string{"{{in.stem}}-{{width}}.{{format}}"},
		Requirements: domain.ResourceRequirements{MinMemoryBytes: 512 << 20},
	},
	{
		Name: "archive.zip", Version: "1", Title: "Zip files together",
		Description: "Pack every input into one zip archive (by file name). Use it as a job's final step to get one download.",
		Params: []Param{
			{Name: "name", Type: String, Title: "Archive name", Default: "archive.zip", Pattern: `[A-Za-z0-9._-]{1,60}\.zip`},
		},
		Inputs:             Inputs{Min: 1, Max: domain.MaxWorkloadInputs, Description: "the files to pack"},
		Outputs:            []string{"{{name}}"},
		Reduce:             true,
		OutputAtMostInputs: true,
	},
	{
		Name: "file.hash", Version: "1", Title: "Checksum files",
		Description: "Write a checksum line for every input to hashes.txt.",
		Params: []Param{
			{Name: "algorithm", Type: Enum, Title: "Algorithm", Default: "sha256", Enum: []string{"sha256", "sha1", "md5"}},
		},
		Inputs:  Inputs{Min: 1, Max: domain.MaxWorkloadInputs, Description: "the files to checksum"},
		Outputs: []string{"hashes.txt"},
		Reduce:  true,
	},
	{
		Name: "text.count", Version: "1", Title: "Count lines and words",
		Description: "Count lines, words and bytes in every input (like wc) and write counts.json.",
		Inputs:      Inputs{Min: 1, Max: domain.MaxWorkloadInputs, Description: "text files"},
		Outputs:     []string{"counts.json"},
		Reduce:      true,
	},
	{
		Name: "llm.generate", Version: "1", Title: "Ask a local AI model",
		Description: "Run a prompt on a local model (Ollama) on whichever device has it, streaming the answer back. Text files given as input are included as context.",
		Params: []Param{
			{Name: "model", Type: String, Title: "Model", Required: true, Pattern: ModelPattern, MaxLength: 128, ChoicesAttr: AttrModels},
			{Name: "prompt", Type: String, Title: "Prompt", Required: true, MaxLength: 16 << 10, Multiline: true, AllowDash: true},
			{Name: "system", Type: String, Title: "System instructions (optional)", MaxLength: 4 << 10, Multiline: true, AllowDash: true},
			{Name: "temperature", Type: Number, Title: "Temperature", Default: "0.7", Min: num(0), Max: num(2)},
			{Name: "max_tokens", Type: Int, Title: "Max tokens", Default: "512", Min: num(1), Max: num(8192)},
			{Name: "seed", Type: Int, Title: "Seed (0 = random)", Default: "0", Min: num(0), Max: num(2147483647)},
		},
		Inputs:  Inputs{Min: 0, Max: 16, Description: "text files to use as context (at most 32 KiB together)"},
		Outputs: []string{"response.txt"},
		// The model stays loaded in the runtime, so live free memory
		// already reflects it: one generation per node is the real guard,
		// the reservation only keeps room for the request itself.
		Requirements:             domain.ResourceRequirements{MinMemoryBytes: 256 << 20},
		MaxPerNode:               1,
		Streams:                  true,
		DefaultMaxRuntimeSeconds: 600,
	},
	{
		Name: "llm.inventory", Version: "1", Title: "List local AI models",
		Description: "List the models the device's local runtime (Ollama) has.",
	},
}

// AttrModels is the capability attribute listing a node's local models
// (comma-separated, normalized by NormalizeModel).
const AttrModels = "models"

// ModelPattern matches an Ollama model reference ("llama3.2",
// "qwen2.5:7b-instruct-q4_K_M", "hf.co/user/repo:tag").
const ModelPattern = `[A-Za-z0-9][A-Za-z0-9._:/-]{0,127}`

// NormalizeModel gives a model reference its canonical form: lowercase,
// with the implicit ":latest" tag made explicit.
func NormalizeModel(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return ""
	}
	if !strings.Contains(name[strings.LastIndex(name, "/")+1:], ":") {
		name += ":latest"
	}
	return name
}

// Offers reports whether a node whose capability carries attrs can take
// params for t: every parameter with ChoicesAttr must be in that list.
// (False with the missing value as the reason.)
func (t Type) Offers(attrs map[string]string, params map[string]string) (bool, string) {
	for _, p := range t.Params {
		if p.ChoicesAttr == "" {
			continue
		}
		want := NormalizeModel(params[p.Name])
		have := false
		for _, v := range strings.Split(attrs[p.ChoicesAttr], ",") {
			have = have || (v != "" && NormalizeModel(v) == want)
		}
		if !have {
			return false, fmt.Sprintf("has no %s %q", p.Name, params[p.Name])
		}
	}
	return true, ""
}

// HasChoices reports whether placement must match some parameter
// against node attributes.
func (t Type) HasChoices() bool {
	for _, p := range t.Params {
		if p.ChoicesAttr != "" {
			return true
		}
	}
	return false
}

var byName = func() map[domain.CapabilityName]Type {
	m := make(map[domain.CapabilityName]Type, len(builtins))
	for _, t := range builtins {
		m[t.Name] = t
	}
	return m
}()

// Types returns every catalog type, sorted by name.
func Types() []Type {
	out := append([]Type(nil), builtins...)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Lookup returns the catalog type named name.
func Lookup(name domain.CapabilityName) (Type, bool) {
	t, ok := byName[name]
	return t, ok
}

// Compile validates params and inputs against t and returns the
// canonical parameters (defaults filled in) and the rendered output
// names. Unknown parameters are refused.
func (t Type) Compile(params map[string]string, inputs []domain.ArtifactRef) (map[string]string, []string, error) {
	known := make(map[string]bool, len(t.Params))
	out := make(map[string]string, len(t.Params))
	for _, p := range t.Params {
		known[p.Name] = true
		v, given := params[p.Name]
		if !given || v == "" {
			if p.Required && p.Default == "" {
				return nil, nil, fmt.Errorf("%w: %s: %s is required", ErrInvalid, t.Name, p.Name)
			}
			if p.Default == "" {
				continue
			}
			v = p.Default
		}
		canon, err := p.check(v)
		if err != nil {
			return nil, nil, fmt.Errorf("%w: %s: %s: %v", ErrInvalid, t.Name, p.Name, err)
		}
		out[p.Name] = canon
	}
	for name := range params {
		if !known[name] {
			return nil, nil, fmt.Errorf("%w: %s takes no parameter %q", ErrInvalid, t.Name, name)
		}
	}
	if n := len(inputs); n < t.Inputs.Min || n > t.Inputs.Max {
		if t.Inputs.Max == 0 {
			return nil, nil, fmt.Errorf("%w: %s takes no input files", ErrInvalid, t.Name)
		}
		return nil, nil, fmt.Errorf("%w: %s takes %d-%d input files, got %d", ErrInvalid, t.Name, t.Inputs.Min, t.Inputs.Max, n)
	}
	if len(t.Inputs.Extensions) > 0 {
		for _, in := range inputs {
			ext := strings.ToLower(strings.TrimPrefix(path.Ext(in.Name), "."))
			ok := false
			for _, e := range t.Inputs.Extensions {
				ok = ok || e == ext
			}
			if !ok {
				return nil, nil, fmt.Errorf("%w: %s accepts .%s files, not %s", ErrInvalid, t.Name, strings.Join(t.Inputs.Extensions, ", ."), in.Name)
			}
		}
	}
	outputs := make([]string, 0, len(t.Outputs))
	for _, tmpl := range t.Outputs {
		name := tmpl
		if strings.Contains(name, "{{in.stem}}") {
			if t.Inputs.Max != 1 || len(inputs) != 1 {
				return nil, nil, fmt.Errorf("%w: %s: {{in.stem}} needs exactly one input", ErrInvalid, t.Name)
			}
			base := path.Base(inputs[0].Name)
			name = strings.ReplaceAll(name, "{{in.stem}}", strings.TrimSuffix(base, path.Ext(base)))
		}
		for k, v := range out {
			name = strings.ReplaceAll(name, "{{"+k+"}}", v)
		}
		if strings.Contains(name, "{{") {
			return nil, nil, fmt.Errorf("%w: %s: output %q has an unfilled placeholder", ErrInvalid, t.Name, tmpl)
		}
		outputs = append(outputs, name)
	}
	return out, outputs, nil
}

func (p Param) check(v string) (string, error) {
	switch p.Type {
	case Int:
		n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		if err != nil {
			return "", fmt.Errorf("%q is not a whole number", v)
		}
		if err := p.inRange(float64(n)); err != nil {
			return "", err
		}
		return strconv.FormatInt(n, 10), nil
	case Number:
		f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		if err != nil {
			return "", fmt.Errorf("%q is not a number", v)
		}
		if err := p.inRange(f); err != nil {
			return "", err
		}
		return strconv.FormatFloat(f, 'g', -1, 64), nil
	case Bool:
		b, err := strconv.ParseBool(strings.TrimSpace(v))
		if err != nil {
			return "", fmt.Errorf("%q is not true or false", v)
		}
		return strconv.FormatBool(b), nil
	case Enum:
		for _, e := range p.Enum {
			if v == e {
				return v, nil
			}
		}
		return "", fmt.Errorf("%q is not one of %s", v, strings.Join(p.Enum, ", "))
	case String:
		limit := p.MaxLength
		if limit == 0 {
			limit = 256
		}
		if len(v) > limit {
			return "", fmt.Errorf("longer than %d bytes", limit)
		}
		if !p.AllowDash && strings.HasPrefix(v, "-") {
			return "", errors.New("may not start with '-'")
		}
		for _, r := range v {
			if p.Multiline && (r == '\n' || r == '\r' || r == '\t') {
				continue
			}
			if r < 0x20 || r == 0x7f {
				return "", errors.New("contains a control character")
			}
		}
		if p.Pattern != "" && !regexp.MustCompile(`^(?:`+p.Pattern+`)$`).MatchString(v) {
			return "", fmt.Errorf("%q doesn't match %s", v, p.Pattern)
		}
		return v, nil
	}
	return "", fmt.Errorf("unknown parameter type %q", p.Type)
}

func (p Param) inRange(f float64) error {
	if p.Min != nil && f < *p.Min {
		return fmt.Errorf("must be at least %g", *p.Min)
	}
	if p.Max != nil && f > *p.Max {
		return fmt.Errorf("must be at most %g", *p.Max)
	}
	return nil
}

// IsRaw reports whether name is one of the unsandboxed capabilities
// (arbitrary programs, arbitrary host files) that policy keeps behind an
// explicit opt-in.
func IsRaw(name domain.CapabilityName) bool {
	return name == domain.CapabilitySystemExecute || name == domain.CapabilityFilesystemRead
}
