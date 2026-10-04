// Package catalog is the typed task catalog (roadmap item 7): the
// allow-listed task types every agent implements natively (in Go, so the
// same on every platform) and the parameter schema each one takes. The
// manager and the agent link the same definitions: the manager validates
// and compiles a submission against them, and the agent re-validates what
// it is assigned before running it, so a version mismatch is refused
// rather than misread.
package catalog

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"

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
	// Needs names a capability attribute a node must carry to be given
	// this parameter: an agent feature newer than the type itself, so an
	// older agent (which would refuse the unknown parameter) is never
	// placed work that sets it.
	Needs string `json:"needs,omitempty"`
	// AnswerFormat: the value is "json" or a JSON object (a JSON schema
	// the answer must follow).
	AnswerFormat bool `json:"answerFormat,omitempty"`
	// LabelList: the value is a comma-separated list of labels
	// (ParseLabels), kept as their canonical form.
	LabelList bool `json:"labelList,omitempty"`
	// Image: the value is a container image reference, canonicalized
	// with its registry explicit (container.go). Argv: the value is a
	// JSON list of strings, a container's command line.
	Image bool `json:"image,omitempty"`
	Argv  bool `json:"argv,omitempty"`
	// MatchAttr names a capability attribute that must equal the value
	// exactly, when one is given (e.g. a container's platform); empty
	// matches every node.
	MatchAttr string `json:"matchAttr,omitempty"`
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
	// TargetRequired: the task is about one particular device (its own
	// models), so a submission must name it — never placed anywhere.
	TargetRequired bool `json:"targetRequired,omitempty"`
	// Internal: a part of something the manager runs (a split session),
	// not offered in the task form or "harnessctl tasks".
	Internal bool `json:"internal,omitempty"`
	// Parts: one run does one part of a whole (int params "part", from
	// 0, and "parts"), so a job runs it once per part.
	Parts bool `json:"parts,omitempty"`
	// DefaultMaxRuntimeSeconds is the per-attempt limit when policy sets
	// none for this type.
	DefaultMaxRuntimeSeconds int `json:"defaultMaxRuntimeSeconds,omitempty"`
	// PartNames: as a job's reduce, the type also gets TaskIndexName, which
	// names each task, so its result can say which file a part came from
	// (a part's path, parts/<task>/<name>, only numbers it).
	PartNames bool `json:"partNames,omitempty"`
	// OptIn: off until the operator's policy turns it on, like raw
	// commands (it runs programs the catalog doesn't define).
	OptIn bool `json:"optIn,omitempty"`
	// OutputsFrom names a parameter holding more output names
	// (comma-separated), for a type whose outputs the submitter chooses.
	OutputsFrom string `json:"outputsFrom,omitempty"`
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
		Name: "render.fractal", Version: "1", Title: "Render part of a fractal image",
		Description: "Draw one horizontal strip of a Mandelbrot fractal, using every CPU core. Split a big image into many parts as a job and every device renders strips; image.stack joins them into one picture.",
		Params: []Param{
			{Name: "width", Type: Int, Title: "Image width (pixels)", Default: "3200", Min: num(16), Max: num(8000)},
			{Name: "height", Type: Int, Title: "Image height (pixels)", Default: "2400", Min: num(16), Max: num(8000)},
			{Name: "part", Type: Int, Title: "Which strip (0 = top)", Default: "0", Min: num(0), Max: num(999)},
			{Name: "parts", Type: Int, Title: "Strips in the whole image", Default: "1", Min: num(1), Max: num(1000)},
			{Name: "iterations", Type: Int, Title: "Detail (iterations)", Default: "2000", Min: num(16), Max: num(50000)},
			{Name: "scene", Type: Enum, Title: "Where to look", Default: "seahorse", Enum: []string{"seahorse", "spiral", "classic"}},
		},
		Outputs:                  []string{"fractal-{{part}}.png"},
		Parts:                    true,
		Requirements:             domain.ResourceRequirements{MinMemoryBytes: 256 << 20},
		DefaultMaxRuntimeSeconds: 600,
	},
	{
		Name: "image.stack", Version: "1", Title: "Stack image strips",
		Description: "Join images top to bottom, in name order, into one PNG: the last step of a job that renders a picture in strips.",
		Params: []Param{
			{Name: "name", Type: String, Title: "Result name", Default: "image.png", Pattern: `[A-Za-z0-9._-]{1,60}\.png`},
		},
		Inputs:       Inputs{Min: 1, Max: domain.MaxWorkloadInputs, Extensions: []string{"png", "jpg", "jpeg", "gif"}, Description: "the strips, top first by name"},
		Outputs:      []string{"{{name}}"},
		Reduce:       true,
		Requirements: domain.ResourceRequirements{MinMemoryBytes: 512 << 20},
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
		Name: "llm.chat", Version: "1", Title: "Chat with a local AI model",
		Description: "One turn of a conversation with a local model (Ollama) on whichever device has it: the messages so far in, the reply streamed back. What AI apps use through the manager's OpenAI-compatible API (/v1).",
		Params: []Param{
			{Name: "model", Type: String, Title: "Model", Required: true, Pattern: ModelPattern, MaxLength: 128, ChoicesAttr: AttrModels},
			{Name: "messages", Type: String, Title: "Messages (JSON: [{role, content}])", Required: true, MaxLength: 64 << 10, Multiline: true, AllowDash: true},
			// No defaults: unset means the model's own.
			{Name: "temperature", Type: Number, Title: "Temperature (empty = the model's default)", Min: num(0), Max: num(2)},
			{Name: "max_tokens", Type: Int, Title: "Max tokens (empty = no limit)", Min: num(1), Max: num(131072)},
			{Name: "seed", Type: Int, Title: "Seed (0 = random)", Default: "0", Min: num(0), Max: num(2147483647)},
			{Name: "format", Type: String, Title: "Answer format (empty = free text; json, or a JSON schema)", MaxLength: 16 << 10, Multiline: true, AllowDash: true, AnswerFormat: true, Needs: AttrChatFormat},
		},
		Outputs:                  []string{"response.txt"},
		Requirements:             domain.ResourceRequirements{MinMemoryBytes: 256 << 20},
		MaxPerNode:               1,
		Streams:                  true,
		DefaultMaxRuntimeSeconds: 600,
	},
	{
		Name: "llm.pull", Version: "1", Title: "Download a local AI model",
		Description: "Download a model into one device's own Ollama (ollama pull), showing its progress. Models are stored per device, so name the device.",
		Params: []Param{
			{Name: "model", Type: String, Title: "Model (e.g. llama3.2:1b)", Required: true, Pattern: ModelPattern, MaxLength: 128},
		},
		TargetRequired: true,
		MaxPerNode:     1,
		Streams:        true,
		// A few GB over home Wi-Fi.
		DefaultMaxRuntimeSeconds: 4 * 3600,
	},
	{
		Name: "llm.remove", Version: "1", Title: "Remove a local AI model",
		Description: "Delete a model from one device's own Ollama (ollama rm), freeing its disk space.",
		Params: []Param{
			{Name: "model", Type: String, Title: "Model", Required: true, Pattern: ModelPattern, MaxLength: 128, ChoicesAttr: AttrModels},
		},
		TargetRequired:           true,
		DefaultMaxRuntimeSeconds: 120,
	},
	{
		Name: "llm.split-helper", Version: "1", Title: "Lend memory to a split model",
		Description: "Part of a split session (a model too big for one device): runs llama.cpp's ggml-rpc-server on this device's loopback only, reached through the session's tunnel. Started and stopped by the manager.",
		Params: []Param{
			{Name: "session", Type: String, Title: "Session", Required: true, Pattern: SessionPattern},
			{Name: "index", Type: Int, Title: "Helper number", Required: true, Min: num(0), Max: num(15)},
		},
		TargetRequired:           true,
		Internal:                 true,
		MaxPerNode:               1,
		Streams:                  true,
		DefaultMaxRuntimeSeconds: 12 * 3600,
	},
	{
		Name: "llm.split-main", Version: "1", Title: "Run a model split across devices",
		Description: "Part of a split session: runs llama.cpp's llama-server with the model file on this device, its layers spread over the session's helpers through their tunnels, and answers the session's chats. Started and stopped by the manager.",
		Params: []Param{
			{Name: "session", Type: String, Title: "Session", Required: true, Pattern: SessionPattern},
			{Name: "model", Type: String, Title: "Model file (.gguf) on this device", Required: true, MaxLength: 1024, Pattern: `.+\.(gguf|GGUF)`},
			{Name: "name", Type: String, Title: "Name to chat with it by", Required: true, Pattern: ModelPattern, MaxLength: 128},
			{Name: "helpers", Type: Int, Title: "Helpers", Required: true, Min: num(1), Max: num(15)},
		},
		TargetRequired:           true,
		Internal:                 true,
		MaxPerNode:               1,
		Streams:                  true,
		DefaultMaxRuntimeSeconds: 12 * 3600,
	},
	{
		Name: "llm.inventory", Version: "1", Title: "List local AI models",
		Description: "List the models the device's local runtime (Ollama) has.",
	},
	// Home media and documents (roadmap-after-9 item 14). image.upscale is
	// pure Go; the others run a program the device owner installed
	// (ffmpeg, whisper.cpp, poppler, tesseract) with arguments built by the
	// agent, and are offered only where it is found (tasks/tools.go).
	{
		Name: "image.upscale", Version: "1", Title: "Upscale an image",
		Description: "Enlarge a JPEG, PNG or GIF 2, 3 or 4 times with a smooth, sharp resampler (Catmull-Rom). The result may have at most 40 million pixels.",
		Params: []Param{
			{Name: "scale", Type: Int, Title: "Scale (times)", Default: "2", Min: num(2), Max: num(4)},
			{Name: "format", Type: Enum, Title: "Output format", Default: "png", Enum: []string{"png", "jpg"}},
			{Name: "quality", Type: Int, Title: "JPEG quality", Default: "92", Min: num(1), Max: num(100)},
		},
		Inputs:                   Inputs{Min: 1, Max: 1, Extensions: []string{"jpg", "jpeg", "png", "gif"}, Description: "one image"},
		Outputs:                  []string{"{{in.stem}}-x{{scale}}.{{format}}"},
		Requirements:             domain.ResourceRequirements{MinMemoryBytes: 512 << 20},
		DefaultMaxRuntimeSeconds: 600,
	},
	{
		Name: "media.transcode", Version: "1", Title: "Convert a video or audio file",
		Description: "Convert with ffmpeg to a fixed preset: 720p.mp4 or 1080p.mp4 (H.264 video, never enlarged), audio.mp3, audio.m4a, or clip.gif (a short animated GIF, at most 30 seconds). Optionally only a part: start and length in seconds. Offered by devices that have ffmpeg.",
		Params: []Param{
			{Name: "preset", Type: Enum, Title: "Convert to", Default: "720p.mp4", Enum: TranscodePresets},
			{Name: "start", Type: Int, Title: "Start at (seconds)", Default: "0", Min: num(0), Max: num(86400)},
			{Name: "duration", Type: Int, Title: "Length (seconds, 0 = to the end; a GIF: 10)", Default: "0", Min: num(0), Max: num(14400)},
		},
		Inputs:       Inputs{Min: 1, Max: 1, Extensions: MediaExtensions, Description: "one video or audio file"},
		Outputs:      []string{"{{in.stem}}-{{preset}}"},
		Requirements: domain.ResourceRequirements{MinMemoryBytes: 512 << 20},
		// ffmpeg uses every core: one at a time per device.
		MaxPerNode:               1,
		DefaultMaxRuntimeSeconds: 3600,
	},
	{
		Name: "audio.transcribe", Version: "1", Title: "Transcribe speech to text",
		Description: "Write down what is said in a recording with whisper.cpp, on the device: transcript.txt and subtitles (transcript.srt). A .wav works anywhere it is offered; other audio and video files need ffmpeg on that device too. Offered by devices with whisper.cpp and a model.",
		Params: []Param{
			{Name: "model", Type: String, Title: "Model", Required: true, Pattern: WhisperModelPattern, MaxLength: 64, ChoicesAttr: AttrWhisperModels},
			{Name: "language", Type: String, Title: "Spoken language (auto, or a code like en, de, fr)", Default: "auto", Pattern: `auto|[a-z]{2,3}`},
		},
		Inputs:       Inputs{Min: 1, Max: 1, Extensions: AudioExtensions, Description: "one recording"},
		Outputs:      []string{"transcript.txt", "transcript.srt"},
		Requirements: domain.ResourceRequirements{MinMemoryBytes: 1 << 30},
		MaxPerNode:   1,
		// Segments are reported as they are recognized.
		Streams:                  true,
		DefaultMaxRuntimeSeconds: 3600,
	},
	{
		Name: "doc.text", Version: "1", Title: "Get the text out of a PDF or picture",
		Description: "Write a PDF's or a scanned page's text to text.txt. text: the PDF's own text (poppler's pdftotext); ocr: recognize it in the picture or the PDF's pages (tesseract); auto: the PDF's text where it has some, recognition otherwise. Offered by devices with pdftotext or tesseract.",
		Params: []Param{
			{Name: "method", Type: Enum, Title: "How to read it", Default: "auto", Enum: []string{"auto", "text", "ocr"}, ChoicesAttr: AttrDocMethods},
			{Name: "language", Type: String, Title: "Language for recognition (e.g. eng, deu, eng+fra)", Default: "eng", Pattern: OCRLanguagePattern},
		},
		Inputs:                   Inputs{Min: 1, Max: 1, Extensions: []string{"pdf", "png", "jpg", "jpeg"}, Description: "one PDF or picture"},
		Outputs:                  []string{"text.txt"},
		Requirements:             domain.ResourceRequirements{MinMemoryBytes: 512 << 20},
		DefaultMaxRuntimeSeconds: 1800,
	},
	// Batch AI over a folder (roadmap-after-9 item 11): run once per file
	// as a job, then report.collect joins the results.
	{
		Name: "llm.classify", Version: "1", Title: "Sort a text file into one of your labels",
		Description: "A local AI model (Ollama) reads one text file and picks the one label that fits it best from yours (e.g. invoice, receipt, letter); writes label.json. Run it once per file as a job and collect the labels into one table with report.collect.",
		Params: []Param{
			{Name: "model", Type: String, Title: "Model", Required: true, Pattern: ModelPattern, MaxLength: 128, ChoicesAttr: AttrModels},
			{Name: "labels", Type: String, Title: "Labels (2-32, separated by commas)", Required: true, MaxLength: 2048, LabelList: true},
		},
		Inputs:                   Inputs{Min: 1, Max: 1, Description: "one text file (UTF-8, at most 32 KiB)"},
		Outputs:                  []string{"label.json"},
		Requirements:             domain.ResourceRequirements{MinMemoryBytes: 256 << 20},
		MaxPerNode:               1,
		DefaultMaxRuntimeSeconds: 600,
	},
	{
		Name: "llm.embed", Version: "1", Title: "Embed text with a local AI model",
		Description: "Turn text files into embeddings: lists of numbers that place texts with similar meaning close together, for search and grouping. Needs an embedding model (Ollama, e.g. embeddinggemma or nomic-embed-text) on some device; writes embeddings.json. Each file must fit the model's context.",
		Params: []Param{
			// The llm.embed capability lists only the device's embedding
			// models, so placement matches the model against those.
			{Name: "model", Type: String, Title: "Embedding model", Required: true, Pattern: ModelPattern, MaxLength: 128, ChoicesAttr: AttrModels},
		},
		Inputs:                   Inputs{Min: 1, Max: 16, Description: "text files (UTF-8, at most 32 KiB each)"},
		Outputs:                  []string{"embeddings.json"},
		Requirements:             domain.ResourceRequirements{MinMemoryBytes: 256 << 20},
		MaxPerNode:               1,
		Streams:                  true,
		DefaultMaxRuntimeSeconds: 600,
	},
	{
		Name: "report.collect", Version: "1", Title: "Collect results into one report",
		Description: "Join a job's per-file results into one file, by the file each came from: answers and summaries (.txt), labels (label.json) and embeddings (embeddings.json). The report's extension picks the format: .md (Markdown), .csv (a table) or .json; embeddings join into .json only. Use it as the combine step of a per-file AI job.",
		Params: []Param{
			{Name: "name", Type: String, Title: "Report file (.md, .csv or .json)", Default: "report.md", Pattern: `[A-Za-z0-9._-]{1,60}\.(md|csv|json)`},
		},
		Inputs:    Inputs{Min: 1, Max: domain.MaxWorkloadInputs, Extensions: []string{"txt", "md", "json"}, Description: "per-file results: .txt answers, label.json, embeddings.json"},
		Outputs:   []string{"{{name}}"},
		Reduce:    true,
		PartNames: true,
	},
	{
		Name: ContainerRun, Version: "1", Title: "Run a container",
		Description: "Run a container image with the device's own Docker or Podman: no network unless allowed, a read-only filesystem, no privileges, CPU, memory and process limits. Input files are at /in (read-only); the files it writes to /out that you name come back. Off until policy turns it on and allows the image.",
		Params: []Param{
			{Name: "image", Type: String, Title: "Image (pinned: name@sha256:...)", Required: true, Pattern: ImagePattern, MaxLength: 512, Image: true},
			{Name: "args", Type: String, Title: `Command as a JSON list, e.g. ["sh","-c","wc -l /in/*"] (empty = the image's own)`, MaxLength: MaxContainerArgv, AllowDash: true, Argv: true},
			{Name: "outputs", Type: String, Title: "Files to bring back from /out (comma-separated)", Pattern: `[A-Za-z0-9._/-]+(?:,[A-Za-z0-9._/-]+)*`, MaxLength: 2200},
			{Name: "cpus", Type: Number, Title: "CPUs", Default: "1", Min: num(0.1), Max: num(64)},
			{Name: "memory_mb", Type: Int, Title: "Memory limit (MB)", Default: "512", Min: num(16), Max: num(65536)},
			{Name: "network", Type: Enum, Title: "Network", Default: "none", Enum: []string{"none", "bridge"}},
			{Name: "timeout", Type: Int, Title: "Time limit (seconds)", Default: "600", Min: num(1), Max: num(86400)},
			{Name: "platform", Type: String, Title: "Platform, e.g. linux/arm64 (empty = any)", Pattern: PlatformPattern, MaxLength: 32, MatchAttr: AttrPlatform},
		},
		Inputs:                   Inputs{Min: 0, Max: domain.MaxWorkloadInputs, Description: "files for /in (read-only)"},
		OutputsFrom:              "outputs",
		Streams:                  true,
		OptIn:                    true,
		DefaultMaxRuntimeSeconds: 3600,
	},
}

// TaskIndexName is the extra input a PartNames reduce gets: JSON
// {"tasks": [{"key": "0000", "name": "a.txt"}, ...]}, each task's key and
// the name it was given (the file it ran on, for a job run per file).
const TaskIndexName = "parts/tasks.json"

// Label bounds (llm.classify's labels).
const (
	MaxLabels     = 32
	MaxLabelBytes = 60
)

// ParseLabels reads a comma-separated list of labels: 2-MaxLabels of
// them, distinct (ignoring case), each up to MaxLabelBytes of letters,
// digits, spaces and _.-'&/(), starting with a letter or digit — so a
// label is never mistaken for an option or a spreadsheet formula.
func ParseLabels(v string) ([]string, error) {
	var labels []string
	seen := map[string]bool{}
	for _, l := range strings.Split(v, ",") {
		l = strings.TrimSpace(l)
		if l == "" {
			return nil, errors.New("an empty label (two commas in a row?)")
		}
		if len(l) > MaxLabelBytes {
			return nil, fmt.Errorf("label %q is longer than %d bytes", l, MaxLabelBytes)
		}
		for i, r := range l {
			ok := unicode.IsLetter(r) || unicode.IsDigit(r) || (i > 0 && strings.ContainsRune(" _.-'&/()", r))
			if !ok {
				return nil, fmt.Errorf("label %q: use letters, digits, spaces and _.-'&/(), starting with a letter or digit", l)
			}
		}
		if seen[strings.ToLower(l)] {
			return nil, fmt.Errorf("label %q is given twice", l)
		}
		seen[strings.ToLower(l)] = true
		labels = append(labels, l)
	}
	if len(labels) < 2 || len(labels) > MaxLabels {
		return nil, fmt.Errorf("give 2-%d labels separated by commas, not %d", MaxLabels, len(labels))
	}
	return labels, nil
}

// TranscodePresets are media.transcode's fixed conversions, each named
// after the file it makes (so the output name carries its extension).
var TranscodePresets = []string{"720p.mp4", "1080p.mp4", "audio.mp3", "audio.m4a", "clip.gif"}

// MediaExtensions are the files media.transcode takes; AudioExtensions
// the recordings audio.transcribe takes (video too: its sound).
var (
	MediaExtensions = []string{"mp4", "m4v", "mov", "mkv", "webm", "avi", "mpg", "mpeg", "ts", "mts", "m2ts", "flv", "wmv", "3gp", "gif",
		"mp3", "m4a", "aac", "wav", "flac", "ogg", "opus", "wma"}
	AudioExtensions = []string{"wav", "mp3", "m4a", "aac", "flac", "ogg", "opus", "wma", "webm", "mp4", "m4v", "mov", "mkv"}
)

// AttrFFmpegVersion is the ffmpeg build a device's media.transcode runs.
const AttrFFmpegVersion = "ffmpegVersion"

// AttrWhisperModels lists the whisper.cpp models a device has, by name
// (ggml-<name>.bin in its tools folder's models/). Matched exactly.
const AttrWhisperModels = "whisperModels"

// WhisperModelPattern matches a whisper.cpp model name ("base.en",
// "large-v3-turbo-q5_0").
const WhisperModelPattern = `[A-Za-z0-9][A-Za-z0-9._-]{0,63}`

// AttrDocMethods lists the doc.text methods a device can run (auto, text,
// ocr), so work that needs recognition goes where tesseract is;
// AttrDocTools names the programs it found, for people.
const (
	AttrDocMethods = "docMethods"
	AttrDocTools   = "docTools"
)

// OCRLanguagePattern matches tesseract languages: "eng", "chi_sim",
// "eng+deu" (up to four).
const OCRLanguagePattern = `[a-z]{3}(_[a-z]{3,4})?(\+[a-z]{3}(_[a-z]{3,4})?){0,3}`

// AttrModels is the capability attribute listing a node's local models
// (comma-separated, normalized by NormalizeModel).
const AttrModels = "models"

// SessionPattern matches a split session's id.
const SessionPattern = `[a-f0-9]{16}`

// Markers a split session's parts print once ready; the manager waits for
// them in the parts' streamed output.
const (
	SplitHelperReady = "split-helper ready"
	SplitMainReady   = "split-main ready"
)

// AttrLlamaVersion is the llama.cpp build a device's split parts run: every
// part of a session must run the same one (its RPC protocol is versioned).
const AttrLlamaVersion = "llamaVersion"

// AttrChatFormat marks an agent whose llm.chat takes the "format"
// parameter (structured answers).
const AttrChatFormat = "chatFormat"

// AttrModelSizes lists each model's size on disk, "name=bytes,...":
// placement prefers a device whose dedicated GPU memory fits the model.
const AttrModelSizes = "modelSizes"

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
// params for t: every parameter with ChoicesAttr must be in that list
// (models compared in their normal form, other values exactly). (False
// with the missing value as the reason.)
func (t Type) Offers(attrs map[string]string, params map[string]string) (bool, string) {
	for _, p := range t.Params {
		if p.Needs != "" && params[p.Name] != "" && attrs[p.Needs] == "" {
			return false, fmt.Sprintf("its agent predates the %s parameter (update it)", p.Name)
		}
		if p.MatchAttr != "" && params[p.Name] != "" && attrs[p.MatchAttr] != params[p.Name] {
			return false, fmt.Sprintf("its %s is %q, not %q", p.MatchAttr, attrs[p.MatchAttr], params[p.Name])
		}
		if p.ChoicesAttr == "" {
			continue
		}
		norm := func(v string) string { return v }
		if p.ChoicesAttr == AttrModels {
			norm = NormalizeModel
		}
		want := norm(params[p.Name])
		have := false
		for _, v := range strings.Split(attrs[p.ChoicesAttr], ",") {
			have = have || (v != "" && norm(v) == want)
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
		if p.ChoicesAttr != "" || p.Needs != "" || p.MatchAttr != "" {
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
	if names := out[t.OutputsFrom]; t.OutputsFrom != "" && names != "" {
		list := strings.Split(names, ",")
		if len(list) > domain.MaxWorkloadOutputs {
			return nil, nil, fmt.Errorf("%w: %s: at most %d %s", ErrInvalid, t.Name, domain.MaxWorkloadOutputs, t.OutputsFrom)
		}
		for _, name := range list {
			if err := domain.ValidArtifactName(name); err != nil {
				return nil, nil, fmt.Errorf("%w: %s: %s: %v", ErrInvalid, t.Name, t.OutputsFrom, err)
			}
		}
		outputs = append(outputs, list...)
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
		if p.AnswerFormat {
			return answerFormat(v)
		}
		if p.LabelList {
			labels, err := ParseLabels(v)
			if err != nil {
				return "", err
			}
			return strings.Join(labels, ","), nil
		}
		if p.Image {
			return canonImage(v)
		}
		if p.Argv {
			return canonArgv(v)
		}
		return v, nil
	}
	return "", fmt.Errorf("unknown parameter type %q", p.Type)
}

// answerFormat checks an answer format: "json", or a JSON object (a
// schema), compacted with its keys kept in order (a model writes them in
// the schema's order).
func answerFormat(v string) (string, error) {
	v = strings.TrimSpace(v)
	if v == "json" {
		return v, nil
	}
	if !strings.HasPrefix(v, "{") || !json.Valid([]byte(v)) {
		return "", errors.New(`must be "json" or a JSON schema object`)
	}
	var b bytes.Buffer
	if err := json.Compact(&b, []byte(v)); err != nil {
		return "", err
	}
	return b.String(), nil
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
