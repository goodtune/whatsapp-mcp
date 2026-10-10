// Package audioconv converts arbitrary audio files to Opus/Ogg (via ffmpeg)
// for WhatsApp voice messages, and extracts duration/waveform metadata from
// Ogg Opus files so voice messages render correctly in the WhatsApp UI.
package audioconv

import (
	"encoding/binary"
	"fmt"
	"math"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
)

// ConvertToOpusOggTemp converts an audio file to Opus/Ogg using ffmpeg and
// returns the path to a new temporary file. The caller is responsible for
// removing the temporary file once it's no longer needed.
func ConvertToOpusOggTemp(inputPath string) (string, error) {
	if _, err := os.Stat(inputPath); err != nil {
		return "", fmt.Errorf("input file not found: %s", inputPath)
	}

	tmp, err := os.CreateTemp("", "whatsapp-audio-*.ogg")
	if err != nil {
		return "", fmt.Errorf("failed to create temp file: %w", err)
	}
	outputPath := tmp.Name()
	tmp.Close()

	cmd := exec.Command("ffmpeg",
		"-i", inputPath,
		"-c:a", "libopus",
		"-b:a", "32k",
		"-ar", "24000",
		"-application", "voip",
		"-vbr", "on",
		"-compression_level", "10",
		"-frame_duration", "60",
		"-y",
		outputPath,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		os.Remove(outputPath)
		return "", fmt.Errorf("failed to convert audio, you likely need to install ffmpeg: %w: %s", err, string(out))
	}

	return outputPath, nil
}

// AnalyzeOggOpus extracts the duration (seconds) and a synthetic waveform
// from an Ogg Opus file, for use in WhatsApp's AudioMessage.Seconds and
// AudioMessage.Waveform fields.
func AnalyzeOggOpus(data []byte) (duration uint32, waveform []byte, err error) {
	if len(data) < 4 || string(data[0:4]) != "OggS" {
		return 0, nil, fmt.Errorf("not a valid Ogg file (missing OggS signature)")
	}

	var lastGranule uint64
	var sampleRate uint32 = 48000
	var preSkip uint16
	var foundOpusHead bool

	for i := 0; i+27 < len(data); {
		if string(data[i:i+4]) != "OggS" {
			i++
			continue
		}

		granulePos := binary.LittleEndian.Uint64(data[i+6 : i+14])
		pageSeqNum := binary.LittleEndian.Uint32(data[i+18 : i+22])
		numSegments := int(data[i+26])

		if i+27+numSegments >= len(data) {
			break
		}
		segmentTable := data[i+27 : i+27+numSegments]

		pageSize := 27 + numSegments
		for _, segLen := range segmentTable {
			pageSize += int(segLen)
		}
		if i+pageSize > len(data) {
			break
		}

		if !foundOpusHead && pageSeqNum <= 1 {
			pageData := data[i : i+pageSize]
			if headPos := indexOf(pageData, []byte("OpusHead")); headPos >= 0 {
				headPos += 8
				if headPos+12 <= len(pageData) {
					preSkip = binary.LittleEndian.Uint16(pageData[headPos+10 : headPos+12])
					sampleRate = binary.LittleEndian.Uint32(pageData[headPos+12 : headPos+16])
					foundOpusHead = true
				}
			}
		}

		if granulePos != 0 {
			lastGranule = granulePos
		}

		i += pageSize
	}

	if lastGranule > 0 {
		durationSeconds := float64(lastGranule-uint64(preSkip)) / float64(sampleRate)
		duration = uint32(math.Ceil(durationSeconds))
	} else {
		duration = uint32(float64(len(data)) / 2000.0)
	}

	if duration < 1 {
		duration = 1
	} else if duration > 300 {
		duration = 300
	}

	return duration, placeholderWaveform(duration), nil
}

func indexOf(haystack, needle []byte) int {
	n := len(needle)
	for i := 0; i+n <= len(haystack); i++ {
		if string(haystack[i:i+n]) == string(needle) {
			return i
		}
	}
	return -1
}

// placeholderWaveform generates a synthetic waveform for WhatsApp voice
// messages, the same shape WhatsApp clients expect (64 bytes, 0-100).
func placeholderWaveform(duration uint32) []byte {
	const waveformLength = 64
	waveform := make([]byte, waveformLength)

	rng := rand.New(rand.NewSource(int64(duration)))

	baseAmplitude := 35.0
	frequencyFactor := float64(minInt(int(duration), 120)) / 30.0

	for i := range waveform {
		pos := float64(i) / float64(waveformLength)

		val := baseAmplitude * math.Sin(pos*math.Pi*frequencyFactor*8)
		val += (baseAmplitude / 2) * math.Sin(pos*math.Pi*frequencyFactor*16)
		val += (rng.Float64() - 0.5) * 15

		fadeInOut := math.Sin(pos * math.Pi)
		val = val * (0.7 + 0.3*fadeInOut)
		val += 50

		if val < 0 {
			val = 0
		} else if val > 100 {
			val = 100
		}
		waveform[i] = byte(val)
	}

	return waveform
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// SanitizeFilename returns a filesystem-safe base filename, stripping any
// directory components.
func SanitizeFilename(name string) string {
	return filepath.Base(name)
}
