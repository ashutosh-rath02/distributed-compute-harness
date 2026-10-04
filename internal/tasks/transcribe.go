package tasks

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"home-harness/internal/catalog"
)

// ---- audio.transcribe (whisper.cpp)

var whisperModelName = regexp.MustCompile(`^(?:` + catalog.WhisperModelPattern + `)$`)

// whisperModels are the models in the tools folder's models/, name ->
// path, from files named like whisper.cpp's own downloads:
// ggml-base.en.bin is "base.en". Its voice-detection models (silero) are
// not speech-to-text models.
func (t *tools) whisperModels() map[string]string {
	if t.dir == "" {
		return nil
	}
	dir := filepath.Join(t.dir, "models")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	out := map[string]string{}
	for _, e := range entries {
		name, ok := strings.CutPrefix(e.Name(), "ggml-")
		name, ok2 := strings.CutSuffix(name, ".bin")
		if !ok || !ok2 || strings.HasPrefix(name, "silero") || !whisperModelName.MatchString(name) {
			continue
		}
		p := filepath.Join(dir, e.Name())
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() {
			out[name] = p
		}
	}
	return out
}

// whisperThreads: whisper.cpp gains little past 8 threads.
func whisperThreads() int { return max(1, min(runtime.NumCPU(), 8)) }

// whisperArgs is whisper-cli's argument list: wav in, <base>.txt and
// <base>.srt out, only the transcript on stdout.
func whisperArgs(model, wav, language, base string) []string {
	return []string{"-m", model, "-f", wav, "-l", language, "-t", strconv.Itoa(whisperThreads()),
		"-otxt", "-osrt", "-of", base, "-np"}
}

// toWavArgs has ffmpeg turn any recording into what whisper.cpp reads:
// 16 kHz mono 16-bit WAV.
func toWavArgs(in, out string) []string {
	return append(ffmpegInput(in, 0, 0), "-map", "0:a:0", "-vn", "-ar", "16000", "-ac", "1", "-c:a", "pcm_s16le", "-f", "wav", "file:"+out)
}

type audioTranscribe struct{ t *tools }

func (a audioTranscribe) Available(context.Context) error {
	if a.t.find("whisper-cli") == "" {
		return errors.New("whisper.cpp's whisper-cli isn't installed (-tools-dir, or a standard place)")
	}
	if len(a.t.whisperModels()) == 0 {
		return errors.New("no whisper.cpp model (ggml-<name>.bin in the tools folder's models)")
	}
	return nil
}

func (a audioTranscribe) Attributes(context.Context) map[string]string {
	var names []string
	for n := range a.t.whisperModels() {
		names = append(names, n)
	}
	sort.Strings(names)
	return map[string]string{catalog.AttrWhisperModels: strings.Join(names, ",")}
}

func (a audioTranscribe) Run(ctx context.Context, env Env) error {
	cli := a.t.find("whisper-cli")
	if cli == "" {
		return errors.New("whisper-cli isn't installed on this device any more")
	}
	// Exactly a model the device listed: the name never becomes a path.
	model, ok := a.t.whisperModels()[env.param("model")]
	if !ok {
		return fmt.Errorf("this device has no whisper model %q", env.param("model"))
	}
	tmp, err := tempDir(env)
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	audio := env.in(0)
	if ffmpeg, _ := a.t.ffmpeg(ctx); ffmpeg != "" {
		wav := filepath.Join(tmp, "audio-16k.wav")
		if err := a.t.exec(ctx, env, ffmpeg, toWavArgs(audio, wav)...); err != nil {
			return fmt.Errorf("convert the recording: %w", err)
		}
		audio = wav
	} else if !hasExt(env.Inputs[0], "wav") {
		return fmt.Errorf("this device has no ffmpeg to convert %s for whisper.cpp: give it a 16 kHz .wav, or install ffmpeg here", env.Inputs[0])
	}
	base := filepath.Join(tmp, "transcript")
	if err := a.t.exec(ctx, env, cli, whisperArgs(model, audio, env.param("language"), base)...); err != nil {
		return err
	}
	for i, ext := range []string{".txt", ".srt"} {
		if err := os.Rename(base+ext, env.out(i)); err != nil {
			return fmt.Errorf("whisper.cpp wrote no %s file", ext)
		}
	}
	fi, err := os.Stat(env.out(0))
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(env.Stderr, "%s: %s of text (model %s, language %s)\n", env.Outputs[0], sizeText(fi.Size()), env.param("model"), env.param("language"))
	return err
}
