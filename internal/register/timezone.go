package register

import "time"

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
