package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// SlateClip holds a pre-compiled MPEG-TS fallback scene sliced by video frame in RAM.
// Frame 0 always starts with PSI tables (SDT/PAT/PMT) + HEVC VPS/SPS/PPS + IDR Keyframe,
// allowing 0ms failover with zero runtime FFmpeg processes.
type SlateClip struct {
	SceneID  string
	Width    int
	Height   int
	RawBytes int
	Frames   [][]byte // Each entry represents ~1 video frame (33.333ms @ 30fps)
}

// ParseFallbackResolution returns (width, height) for "720p" or "1080p" (default).
func ParseFallbackResolution(res string) (int, int) {
	switch strings.ToLower(strings.TrimSpace(res)) {
	case "720p", "1280x720":
		return 1280, 720
	default:
		return 1920, 1080
	}
}

func isImageExtension(ext string) bool {
	switch strings.ToLower(ext) {
	case ".jpg", ".jpeg", ".png", ".webp", ".bmp":
		return true
	default:
		return false
	}
}

// CompileSceneSlate normalizes an uploaded image or short video into a broadcast-compliant
// H.265 + AAC 48kHz MPEG-TS clip with fixed PIDs (PMT=0x1000, Video=256, Audio=257)
// and generates a 320x180 JPEG thumbnail for the Web UI.
//
// Duration math:
//   - Static image: 1.6s = 48 video frames (30fps) = 75 AAC frames (48kHz / 1024 samples) -> 0.000ms A/V drift!
//   - Video clip: up to 9.6s = 288 video frames = 450 AAC frames (6 * 1.6s) -> exact loop boundary!
func CompileSceneSlate(inputPath, outTSPath, thumbPath string, width, height int) error {
	ext := strings.ToLower(filepath.Ext(inputPath))
	vfScalePad := fmt.Sprintf(
		"scale=%d:%d:force_original_aspect_ratio=decrease,pad=%d:%d:(ow-iw)/2:(oh-ih)/2:black,setsar=1,format=yuv420p",
		width, height, width, height,
	)

	// 1. Generate 320x180 thumbnail for dashboard UI (best-effort)
	if thumbPath != "" {
		thumbVF := "scale=320:180:force_original_aspect_ratio=decrease,pad=320:180:(ow-iw)/2:(oh-ih)/2:black"
		thumbCmd := exec.Command("ffmpeg",
			"-hide_banner", "-loglevel", "error", "-y",
			"-i", inputPath,
			"-frames:v", "1",
			"-vf", thumbVF,
			"-q:v", "4",
			thumbPath,
		)
		if err := thumbCmd.Run(); err != nil {
			log.Printf("[slate] Thumbnail generation warning for %s: %v", inputPath, err)
		}
	}

	tmpOut := outTSPath + ".tmp.ts"
	defer os.Remove(tmpOut)

	x265Params := "keyint=48:min-keyint=48:bframes=0:scenecut=0:repeat-headers=1:pools=1:frame-threads=1"

	var args []string
	if isImageExtension(ext) {
		args = []string{
			"-hide_banner", "-loglevel", "error", "-y",
			"-loop", "1", "-framerate", "30", "-t", "1.6", "-i", inputPath,
			"-f", "lavfi", "-t", "1.6", "-i", "anullsrc=channel_layout=stereo:sample_rate=48000",
			"-map", "0:v:0", "-map", "1:a:0",
			"-vf", vfScalePad,
			"-r", "30", "-frames:v", "48",
			"-c:v", "libx265", "-preset", "ultrafast",
			"-x265-params", x265Params,
			"-c:a", "aac", "-b:a", "128k", "-ar", "48000", "-ac", "2",
			"-mpegts_pmt_start_pid", "0x1000",
			"-mpegts_start_pid", "0x0100",
			"-pes_payload_size", "0",
			"-f", "mpegts", tmpOut,
		}
	} else {
		hasAudio := probeHasAudio(inputPath)
		if hasAudio {
			args = []string{
				"-hide_banner", "-loglevel", "error", "-y",
				"-t", "9.6", "-i", inputPath,
				"-map", "0:v:0", "-map", "0:a:0",
				"-vf", vfScalePad,
				"-r", "30",
				"-c:v", "libx265", "-preset", "ultrafast",
				"-x265-params", x265Params,
				"-c:a", "aac", "-b:a", "128k", "-ar", "48000", "-ac", "2",
				"-mpegts_pmt_start_pid", "0x1000",
				"-mpegts_start_pid", "0x0100",
				"-pes_payload_size", "0",
				"-f", "mpegts", tmpOut,
			}
		} else {
			args = []string{
				"-hide_banner", "-loglevel", "error", "-y",
				"-t", "9.6", "-i", inputPath,
				"-f", "lavfi", "-t", "9.6", "-i", "anullsrc=channel_layout=stereo:sample_rate=48000",
				"-map", "0:v:0", "-map", "1:a:0",
				"-vf", vfScalePad,
				"-r", "30", "-shortest",
				"-c:v", "libx265", "-preset", "ultrafast",
				"-x265-params", x265Params,
				"-c:a", "aac", "-b:a", "128k", "-ar", "48000", "-ac", "2",
				"-mpegts_pmt_start_pid", "0x1000",
				"-mpegts_start_pid", "0x0100",
				"-pes_payload_size", "0",
				"-f", "mpegts", tmpOut,
			}
		}
	}

	cmd := exec.Command("ffmpeg", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("ffmpeg slate compile failed: %w (%s)", err, strings.TrimSpace(string(out)))
	}

	if err := os.Rename(tmpOut, outTSPath); err != nil {
		return fmt.Errorf("rename slate ts: %w", err)
	}
	return nil
}

func probeHasAudio(inputPath string) bool {
	cmd := exec.Command("ffprobe",
		"-v", "error",
		"-select_streams", "a:0",
		"-show_entries", "stream=codec_type",
		"-of", "csv=p=0",
		inputPath,
	)
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) != ""
}

// EnsureBuiltInSlate generates a fast (<0.5s) dark 1.6s fallback slate + thumbnail
// when the server starts on a fresh machine with no user-uploaded image yet.
func EnsureBuiltInSlate(scenesDir string, width, height int) (tsPath, thumbPath string, err error) {
	os.MkdirAll(scenesDir, 0755)
	tsPath = filepath.Join(scenesDir, "builtin_default_slate.ts")
	thumbPath = filepath.Join(scenesDir, "builtin_default_thumb.jpg")

	// Generate clean dark slate via lavfi color source
	x265Params := "keyint=48:min-keyint=48:bframes=0:scenecut=0:repeat-headers=1:pools=1:frame-threads=1"
	cmd := exec.Command("ffmpeg",
		"-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-t", "1.6", "-i", fmt.Sprintf("color=c=0x0b101b:s=%dx%d:r=30", width, height),
		"-f", "lavfi", "-t", "1.6", "-i", "anullsrc=channel_layout=stereo:sample_rate=48000",
		"-map", "0:v:0", "-map", "1:a:0",
		"-frames:v", "48", "-pix_fmt", "yuv420p",
		"-c:v", "libx265", "-preset", "ultrafast",
		"-x265-params", x265Params,
		"-c:a", "aac", "-b:a", "128k", "-ar", "48000", "-ac", "2",
		"-mpegts_pmt_start_pid", "0x1000",
		"-mpegts_start_pid", "0x0100",
		"-pes_payload_size", "0",
		"-f", "mpegts", tsPath,
	)
	if out, runErr := cmd.CombinedOutput(); runErr != nil {
		return "", "", fmt.Errorf("generate built-in slate: %w (%s)", runErr, strings.TrimSpace(string(out)))
	}

	// Generate matching 320x180 thumbnail
	_ = exec.Command("ffmpeg",
		"-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "color=c=0x0b101b:s=320x180:r=1",
		"-frames:v", "1", "-q:v", "5", thumbPath,
	).Run()

	return tsPath, thumbPath, nil
}

// LoadSlateClipFromFile reads a compiled .ts file into RAM and slices it at video frame boundaries.
func LoadSlateClipFromFile(sceneID, tsPath string, width, height int) (*SlateClip, error) {
	raw, err := os.ReadFile(tsPath)
	if err != nil {
		return nil, err
	}
	frames := SliceTSIntoFrames(raw)
	if len(frames) == 0 {
		return nil, fmt.Errorf("no valid MPEG-TS packets found in %s", tsPath)
	}
	return &SlateClip{
		SceneID:  sceneID,
		Width:    width,
		Height:   height,
		RawBytes: len(raw),
		Frames:   frames,
	}, nil
}

// SliceTSIntoFrames splits raw MPEG-TS bytes into 1-video-frame chunks (33.33ms @ 30fps).
// Any PSI tables (SDT/PAT/PMT) immediately preceding a video PUSI packet stay attached to that frame,
// guaranteeing Frames[0] begins at byte 0 with PAT + PMT + IDR Keyframe.
func SliceTSIntoFrames(raw []byte) [][]byte {
	aligner := NewPacketAligner()
	aligner.Feed(raw)
	aligned := aligner.NextBatch()
	if len(aligned) < tsPacketSize {
		return nil
	}

	totalPkts := len(aligned) / tsPacketSize
	vidPID := uint16(defaultVideoPID)
	pmtPID := uint16(defaultPMTPID)

	// Detect custom PMT/Video PID in case a pre-existing .ts file used non-default PIDs
	for i := 0; i < totalPkts && i < 64; i++ {
		pkt := aligned[i*tsPacketSize : (i+1)*tsPacketSize]
		info, ok := ParseTSPacket(pkt)
		if !ok {
			continue
		}
		if info.PID == patPID && info.PUSI && info.HasPayload {
			if p := ParsePAT(pkt[info.PayloadOffset:]); p != 0 {
				pmtPID = p
			}
		}
		if info.PID == pmtPID && info.PUSI && info.HasPayload {
			if v, _ := ParsePMT(pkt[info.PayloadOffset:]); v != 0 {
				vidPID = v
				break
			}
		}
	}

	// Collect packet indices where each video frame starts (PUSI=1 on vidPID)
	var frameStarts []int
	for i := 0; i < totalPkts; i++ {
		pkt := aligned[i*tsPacketSize : (i+1)*tsPacketSize]
		info, ok := ParseTSPacket(pkt)
		if !ok {
			continue
		}
		if info.PID == vidPID && info.PUSI {
			startPkt := i
			if len(frameStarts) == 0 {
				startPkt = 0 // Frame 0 always starts at packet 0 to include initial SDT/PAT/PMT
			} else {
				prevStart := frameStarts[len(frameStarts)-1]
				// Attach preceding PSI packets (SDT/PAT/PMT) to this frame
				for startPkt > prevStart+1 {
					prevPkt := aligned[(startPkt-1)*tsPacketSize : startPkt*tsPacketSize]
					pInfo, pOk := ParseTSPacket(prevPkt)
					if !pOk || (pInfo.PID != patPID && pInfo.PID != sdtPID && pInfo.PID != pmtPID) {
						break
					}
					startPkt--
				}
			}
			frameStarts = append(frameStarts, startPkt)
		}
	}

	// Fallback if stream had no detectable video PUSI: split into ~7-packet chunks
	if len(frameStarts) == 0 {
		const pktsPerChunk = 7
		var chunks [][]byte
		for p := 0; p < totalPkts; p += pktsPerChunk {
			endP := p + pktsPerChunk
			if endP > totalPkts {
				endP = totalPkts
			}
			chunk := make([]byte, (endP-p)*tsPacketSize)
			copy(chunk, aligned[p*tsPacketSize:endP*tsPacketSize])
			chunks = append(chunks, chunk)
		}
		return chunks
	}

	frames := make([][]byte, 0, len(frameStarts))
	for idx, startPkt := range frameStarts {
		endPkt := totalPkts
		if idx+1 < len(frameStarts) {
			endPkt = frameStarts[idx+1]
		}
		if endPkt <= startPkt {
			continue
		}
		frameBytes := make([]byte, (endPkt-startPkt)*tsPacketSize)
		copy(frameBytes, aligned[startPkt*tsPacketSize:endPkt*tsPacketSize])
		frames = append(frames, frameBytes)
	}
	return frames
}
