package register

import "github.com/auucoder/gptgrok2api-go/internal/browserprofile"

func browserProfileBase(w, h, c int, heap int64) BrowserProfile {
	return BrowserProfile(browserprofile.Base(w, h, c, heap))
}

var browserProfilePool = func() []BrowserProfile {
	var out []BrowserProfile
	for _, p := range browserprofile.Candidates() {
		out = append(out, BrowserProfile(p))
	}
	return out
}()
