package register

import "strings"

const adobeCountryCodes = " AD AE AF AG AI AL AM AO AQ AR AS AT AU AW AX AZ BA BB BD BE BF BG BH BI BJ BL BM BN BO BQ BR BS BT BV BW BY BZ CA CC CD CF CG CH CI CK CL CM CN CO CR CU CV CW CX CY CZ DE DJ DK DM DO DZ EC EE EG EH ER ES ET FI FJ FK FM FO FR GA GB GD GE GF GG GH GI GL GM GN GP GQ GR GS GT GU GW GY HK HM HN HR HT HU ID IE IL IM IN IO IQ IR IS IT JE JM JO JP KE KG KH KI KM KN KP KR KW KY KZ LA LB LC LI LK LR LS LT LU LV LY MA MC MD ME MF MG MH MK ML MM MN MO MP MQ MR MS MT MU MV MW MX MY MZ NA NC NE NF NG NI NL NO NP NR NU NZ OM PA PE PF PG PH PK PL PM PN PR PS PT PW PY QA RE RO RS RU RW SA SB SC SD SE SG SH SI SJ SK SL SM SN SO SR SS ST SV SX SY SZ TC TD TF TG TH TJ TK TL TM TN TO TR TT TV TW TZ UA UG UM US UY UZ VA VC VE VG VI VN VU WF WS YE YT ZA ZM ZW "

type adobeCountrySelection struct {
	Country string `json:"country"`
	Source  string `json:"source"`
}

func (c adobeCountrySelection) valid() bool {
	if len(c.Country) != 2 || !strings.Contains(adobeCountryCodes, " "+c.Country+" ") {
		return false
	}
	switch c.Source {
	case "ipwho_is", "preserved":
		return true
	case "default_us":
		return c.Country == "US"
	default:
		return false
	}
}

// The Worker persists the selection before returning it; Go commits the same
// country before any command that could create the upstream account.
func (e *AdobeEngine) recordRegistrationCountry(id string, selection adobeCountrySelection) error {
	if !selection.valid() {
		return fail("adobe", "registration_country_invalid")
	}
	var rejected error
	err := e.change(id, func(job *RegistrationJob) {
		p := job.AdobeProfile
		if p == nil || (p.CountrySource != "" || job.AdobeRegistration != nil && job.AdobeRegistration.SubmissionStarted) && p.Country != selection.Country {
			rejected = fail("adobe", "saved_registration_country_changed")
			return
		}
		if p.CountrySource != "" {
			return
		}
		p.Country, p.CountrySource = selection.Country, selection.Source
		detail := "沿用本次注册已保存的国家/地区：" + selection.Country
		if selection.Source == "ipwho_is" {
			detail = "按浏览器 Worker 的本次出口设置国家/地区：" + selection.Country + "（来源 ipwho.is）"
		}
		if selection.Source == "default_us" {
			detail = "浏览器出口国家定位失败，沿用默认国家/地区 US；未切换出口"
		}
		appendRegistrationLog(job, "adobe_registration_country", detail, "info")
	})
	if err != nil {
		return err
	}
	return rejected
}
