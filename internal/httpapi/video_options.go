package httpapi

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/auucoder/gptgrok2api-go/internal/model"
	"github.com/auucoder/gptgrok2api-go/internal/protocol"
)

type videoOptions struct {
	Seconds                          int
	Size, Aspect, Resolution, Preset string
}

// Parse the supported Web/SSO contract before queuing or spending quota.
// Size selects an aspect ratio; resolution is a requested tier, not exact pixels.
func parseVideoOptions(r *http.Request) (videoOptions, error) {
	var options videoOptions
	if m, ok := model.AdobeModel(r.FormValue("model")); ok && m.Kind == "video" {
		return parseAdobeVideoOptions(r, m.ID)
	}
	for key, values := range r.Form {
		switch key {
		case "model", "prompt", "seconds", "size", "aspect_ratio", "resolution_name", "resolution", "preset", "quality", "client_task_id":
			if len(values) != 1 {
				return options, fmt.Errorf("%s must be provided once", key)
			}
		default:
			return options, fmt.Errorf("unsupported video parameter %q for the current Grok Web channel", key)
		}
	}
	options.Seconds = 6
	if raw := strings.TrimSpace(r.FormValue("seconds")); raw != "" {
		var err error
		options.Seconds, err = strconv.Atoi(raw)
		if err != nil {
			return options, fmt.Errorf("当前 Grok Web 视频通道支持 1～6 秒；具体账号权限由上游返回")
		}
	}
	if options.Seconds < 1 || options.Seconds > 6 {
		return options, fmt.Errorf("当前 Grok Web 视频通道支持 1～6 秒；具体账号权限由上游返回")
	}
	sizes := map[string]string{"9:16": "720x1280", "16:9": "1280x720", "1:1": "1024x1024", "2:3": "1024x1792", "3:2": "1792x1024"}
	options.Size = strings.TrimSpace(r.FormValue("size"))
	requestedAspect := strings.TrimSpace(r.FormValue("aspect_ratio"))
	if requestedAspect != "" && sizes[requestedAspect] == "" {
		return options, fmt.Errorf("aspect_ratio must be one of [9:16, 16:9, 1:1, 2:3, 3:2]")
	}
	if options.Size == "" {
		options.Size = sizes[requestedAspect]
		if options.Size == "" {
			options.Size = sizes["9:16"]
		}
	}
	var ok bool
	options.Aspect, ok = protocol.AspectRatio(options.Size)
	if !ok || sizes[options.Aspect] != options.Size {
		return options, fmt.Errorf("size must be one of [720x1280, 1280x720, 1024x1024, 1024x1792, 1792x1024]; it selects aspect ratio, not output pixels")
	}
	if requestedAspect != "" && requestedAspect != options.Aspect {
		return options, fmt.Errorf("size and aspect_ratio must select the same aspect ratio")
	}
	options.Resolution = strings.ToLower(strings.TrimSpace(r.FormValue("resolution_name")))
	alias := strings.ToLower(strings.TrimSpace(r.FormValue("resolution")))
	if options.Resolution != "" && alias != "" && options.Resolution != alias {
		return options, fmt.Errorf("resolution and resolution_name must match")
	}
	if options.Resolution == "" {
		options.Resolution = alias
	}
	if options.Resolution == "" {
		options.Resolution = "720p"
	}
	if options.Resolution != "480p" && options.Resolution != "720p" {
		return options, fmt.Errorf("resolution_name must be 480p or 720p; other resolutions are not supported by this CFM Web route")
	}
	options.Preset = strings.ToLower(strings.TrimSpace(r.FormValue("preset")))
	if options.Preset == "" {
		options.Preset = "custom"
	}
	switch options.Preset {
	case "custom", "normal", "fun", "spicy":
	default:
		return options, fmt.Errorf("preset must be one of [custom, normal, fun, spicy]")
	}
	// Retain the legacy compatibility value, without pretending it controls quality.
	if quality := strings.ToLower(strings.TrimSpace(r.FormValue("quality"))); quality != "" && quality != "standard" {
		return options, fmt.Errorf("Grok Web video has no separate quality setting; use resolution_name (480p or 720p), and omit quality or use standard")
	}
	return options, nil
}

func parseAdobeVideoOptions(r *http.Request, id string) (videoOptions, error) {
	m, _ := model.AdobeModel(id)
	v := videoOptions{Seconds: m.Duration, Size: m.Size, Aspect: m.AspectRatio, Resolution: m.Resolution}
	for key, values := range r.Form {
		switch key {
		case "model", "prompt", "seconds", "size", "aspect_ratio", "resolution_name", "resolution", "generate_audio", "client_task_id":
			if len(values) != 1 {
				return v, fmt.Errorf("%s must be provided once", key)
			}
		default:
			return v, fmt.Errorf("unsupported parameter %q for this model", key)
		}
	}
	if value := r.FormValue("seconds"); value != "" && value != strconv.Itoa(m.Duration) {
		return v, fmt.Errorf("verified Free duration for this model is %d seconds", m.Duration)
	}
	for _, field := range []struct{ key, value string }{{"size", m.Size}, {"aspect_ratio", m.AspectRatio}, {"resolution", m.Resolution}, {"resolution_name", m.Resolution}} {
		if value := strings.TrimSpace(r.FormValue(field.key)); value != "" && value != field.value {
			return v, fmt.Errorf("unsupported %s for the verified Free model tier", field.key)
		}
	}
	if audio := strings.TrimSpace(r.FormValue("generate_audio")); audio != "" && audio != "false" && audio != "0" {
		return v, fmt.Errorf("audio is not enabled for the verified Free model tier")
	}
	return v, nil
}
