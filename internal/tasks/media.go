package tasks

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"home-harness/internal/catalog"
)

// ---- media.transcode (ffmpeg)

// ffmpegInput is how every ffmpeg run here starts: quiet, never reading
// stdin, and reading its one input file only as a plain media file — no
// protocol but file (the input is named "file:<path>", so no name reads
// as a URL), and only these container formats, so a "video" that is
// really a playlist (HLS) or a concat list can't pull other files on the
// device into the output.
func ffmpegInput(in string, start, duration int) []string {
	args := []string{"-hide_banner", "-nostdin", "-nostats", "-loglevel", "error", "-y",
		"-protocol_whitelist", "file", "-format_whitelist", ffmpegFormats}
	if start > 0 {
		args = append(args, "-ss", strconv.Itoa(start))
	}
	args = append(args, "-i", "file:"+in)
	if duration > 0 {
		args = append(args, "-t", strconv.Itoa(duration))
	}
	return args
}

// ffmpegFormats are the container formats (ffmpeg demuxer names) an input
// may be: those of catalog.MediaExtensions.
const ffmpegFormats = "mov,mp4,matroska,webm,avi,mpeg,mpegts,flv,asf,gif,mp3,aac,wav,flac,ogg"

// transcodePresets are the fixed ffmpeg settings behind each preset
// (catalog.TranscodePresets), ending with the output format.
var transcodePresets = map[string][]string{
	// H.264 + AAC, never enlarged (an even height, as H.264 needs);
	// the index at the front so it plays while still downloading.
	"720p.mp4": {"-map", "0:v:0", "-map", "0:a:0?", "-vf", "scale=-2:'min(720,trunc(ih/2)*2)'",
		"-c:v", "libx264", "-preset", "veryfast", "-crf", "23", "-pix_fmt", "yuv420p",
		"-c:a", "aac", "-b:a", "128k", "-movflags", "+faststart", "-f", "mp4"},
	"1080p.mp4": {"-map", "0:v:0", "-map", "0:a:0?", "-vf", "scale=-2:'min(1080,trunc(ih/2)*2)'",
		"-c:v", "libx264", "-preset", "veryfast", "-crf", "23", "-pix_fmt", "yuv420p",
		"-c:a", "aac", "-b:a", "160k", "-movflags", "+faststart", "-f", "mp4"},
	"audio.mp3": {"-map", "0:a:0", "-vn", "-c:a", "libmp3lame", "-q:a", "2", "-f", "mp3"},
	"audio.m4a": {"-map", "0:a:0", "-vn", "-c:a", "aac", "-b:a", "160k", "-movflags", "+faststart", "-f", "ipod"},
	// 12 frames a second, at most 480 wide, with a palette made for the
	// clip (GIF has 256 colors).
	"clip.gif": {"-map", "0:v:0", "-an", "-vf", "fps=12,scale='min(480,iw)':-1:flags=lanczos,split[a][b];[a]palettegen[p];[b][p]paletteuse",
		"-loop", "0", "-f", "gif"},
}

// GIF clips: 10 seconds unless asked otherwise, at most 30.
const (
	gifDefaultSeconds = 10
	gifMaxSeconds     = 30
)

// transcodeArgs is ffmpeg's whole argument list for one conversion.
func transcodeArgs(preset string, start, duration int, in, out string) ([]string, error) {
	settings, ok := transcodePresets[preset]
	if !ok {
		return nil, fmt.Errorf("unknown preset %q", preset)
	}
	if preset == "clip.gif" {
		if duration == 0 {
			duration = gifDefaultSeconds
		}
		if duration > gifMaxSeconds {
			return nil, fmt.Errorf("a GIF clip is at most %d seconds (asked for %d)", gifMaxSeconds, duration)
		}
	}
	args := ffmpegInput(in, start, duration)
	args = append(args, settings...)
	return append(args, "file:"+out), nil
}

type mediaTranscode struct{ t *tools }

func (m mediaTranscode) Available(ctx context.Context) error {
	if p, _ := m.t.ffmpeg(ctx); p == "" {
		return errors.New("ffmpeg isn't installed (-tools-dir, or a standard place)")
	}
	return nil
}

func (m mediaTranscode) Attributes(ctx context.Context) map[string]string {
	_, version := m.t.ffmpeg(ctx)
	return map[string]string{catalog.AttrFFmpegVersion: version}
}

func (m mediaTranscode) Run(ctx context.Context, env Env) error {
	ffmpeg, _ := m.t.ffmpeg(ctx)
	if ffmpeg == "" {
		return errors.New("ffmpeg isn't installed on this device any more")
	}
	preset := env.param("preset")
	args, err := transcodeArgs(preset, env.intParam("start"), env.intParam("duration"), env.in(0), env.out(0))
	if err != nil {
		return err
	}
	if err := m.t.exec(ctx, env, ffmpeg, args...); err != nil {
		return err
	}
	fi, err := os.Stat(env.out(0))
	if err != nil || fi.Size() == 0 {
		return errors.New("ffmpeg finished without writing anything")
	}
	_, err = fmt.Fprintf(env.Stdout, "%s -> %s (%s, %s)\n", env.Inputs[0], env.Outputs[0], preset, sizeText(fi.Size()))
	return err
}

func sizeText(n int64) string {
	switch {
	case n >= 10<<20:
		return strconv.FormatInt(n>>20, 10) + " MB"
	case n >= 10<<10:
		return strconv.FormatInt(n>>10, 10) + " KB"
	}
	return strconv.FormatInt(n, 10) + " bytes"
}

// hasExt reports whether name ends in one of exts (lowercase, no dot).
func hasExt(name string, exts ...string) bool {
	name = strings.ToLower(name)
	for _, e := range exts {
		if strings.HasSuffix(name, "."+e) {
			return true
		}
	}
	return false
}
