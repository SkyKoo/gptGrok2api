package register

import "time"

// Only egress-dependent fields change. Stable TLS/browser/device identity is
// reused across login attempts; offsets are calculated for the current instant.
func (p *BrowserProfile) setTimezone(loc *time.Location, at time.Time) {
	name, seconds := at.In(loc).Zone()
	p.TimezoneIANA = loc.String()
	p.TimezoneName = name
	if p.TimezoneIANA == "UTC" {
		p.TimezoneName = "Coordinated Universal Time"
	}
	p.TimezoneOffsetMinutes = -seconds / 60 // JavaScript getTimezoneOffset sign.
}

func (r *WebRegistrar) browserDate(at time.Time) string {
	profile := r.profile
	loc := r.timezone
	if loc == nil {
		loc, _ = time.LoadLocation(profile.TimezoneIANA)
		if loc == nil {
			loc = time.UTC
		}
	}
	profile.setTimezone(loc, at)
	return at.In(loc).Format("Mon Jan 02 2006 15:04:05 GMT-0700") + " (" + profile.TimezoneName + ")"
}
