package provider

import (
	"bytes"
	"encoding/binary"
	"errors"
	"image/gif"
	"strings"
)

func (o ImageOutputOptions) effectiveFrameCount() int {
	if o.FrameCount != nil {
		return *o.FrameCount
	}
	return 9
}
func (o ImageOutputOptions) effectiveFrameDuration() int {
	if o.FrameDurationMS != nil {
		return ((*o.FrameDurationMS + 5) / 10) * 10
	}
	return 200
}
func (o ImageOutputOptions) validateAnimation() error {
	bad := func(param, message string) error { return &ImageOptionError{Param: param, Message: message} }
	if o.OutputFormat != "gif" {
		if o.FrameCount != nil {
			return bad("frame_count", "frame_count requires output_format=gif")
		}
		if o.FrameDurationMS != nil {
			return bad("frame_duration_ms", "frame_duration_ms requires output_format=gif")
		}
		return nil
	}
	// Use the overlap of the published limits until larger Web requests are verified.
	if n := o.effectiveFrameCount(); n < 2 || n > 16 {
		return bad("frame_count", "frame_count must be between 2 and 16")
	}
	if o.FrameDurationMS != nil && (*o.FrameDurationMS < 20 || *o.FrameDurationMS > 2000) {
		return bad("frame_duration_ms", "frame_duration_ms must be between 20 and 2000")
	}
	if o.Background != "" && o.Background != "auto" {
		return bad("background", "GIF output currently supports background=auto only")
	}
	if o.Stream {
		return bad("stream", "GIF output does not support streaming")
	}
	if o.PartialImages != nil {
		return bad("partial_images", "GIF output does not support partial_images")
	}
	if o.OutputCompression != nil {
		return bad("output_compression", "GIF output does not support output_compression")
	}
	return nil
}

// Inspect the container before DecodeAll allocates frame buffers. A small LZW
// payload can describe many large frames, so a compressed-byte limit is insufficient.
func decodeBoundedGIF(raw []byte) (*gif.GIF, error) {
	invalid := errors.New("invalid GIF or animation exceeds frame/pixel limits")
	if len(raw) < 14 || len(raw) > 64<<20 || (string(raw[:6]) != "GIF87a" && string(raw[:6]) != "GIF89a") {
		return nil, invalid
	}
	width, height := uint64(binary.LittleEndian.Uint16(raw[6:8])), uint64(binary.LittleEndian.Uint16(raw[8:10]))
	if width == 0 || height == 0 || width*height > 64_000_000 {
		return nil, invalid
	}
	pos := 13
	if raw[10]&0x80 != 0 {
		pos += 3 * (1 << ((raw[10] & 7) + 1))
	}
	frames := 0
	blocks := func() bool {
		for pos < len(raw) {
			n := int(raw[pos])
			pos++
			if n == 0 {
				return true
			}
			if n > len(raw)-pos {
				return false
			}
			pos += n
		}
		return false
	}
	for pos < len(raw) {
		marker := raw[pos]
		pos++
		switch marker {
		case 0x21:
			if pos >= len(raw) {
				return nil, invalid
			}
			pos++
			if !blocks() {
				return nil, invalid
			}
		case 0x2c:
			if len(raw)-pos < 9 {
				return nil, invalid
			}
			left, top := uint64(binary.LittleEndian.Uint16(raw[pos:pos+2])), uint64(binary.LittleEndian.Uint16(raw[pos+2:pos+4]))
			w, h := uint64(binary.LittleEndian.Uint16(raw[pos+4:pos+6])), uint64(binary.LittleEndian.Uint16(raw[pos+6:pos+8]))
			flags := raw[pos+8]
			pos += 9
			frames++
			if frames > 64 || width*height*uint64(frames) > 64_000_000 || w == 0 || h == 0 || left+w > width || top+h > height {
				return nil, invalid
			}
			if flags&0x80 != 0 {
				pos += 3 * (1 << ((flags & 7) + 1))
			}
			if pos >= len(raw) {
				return nil, invalid
			}
			pos++ // LZW minimum code size; the decoder verifies it.
			if !blocks() {
				return nil, invalid
			}
		case 0x3b:
			if frames == 0 || pos != len(raw) {
				return nil, invalid
			}
			return gif.DecodeAll(bytes.NewReader(raw))
		default:
			return nil, invalid
		}
	}
	return nil, invalid
}

// Animation metadata embeds the source spritesheet and speed variants. Only the
// asset's primary pointer is the delivered animation; nested assets are not outputs.
func primaryOpenAIAnimationRef(value map[string]any) string {
	metadata, _ := value["metadata"].(map[string]any)
	generation, _ := metadata["generation"].(map[string]any)
	animation, _ := generation["animation"].(map[string]any)
	if animation == nil {
		return ""
	}
	pointer := stringValue(value["asset_pointer"])
	if strings.HasPrefix(pointer, "sediment://") {
		return pointer
	}
	if strings.HasPrefix(pointer, "file-service://") {
		return strings.TrimPrefix(pointer, "file-service://")
	}
	return ""
}
