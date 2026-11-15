package register

import (
	"encoding/json"
	"fmt"
	"strings"
)

const adobeCountryCodes = " AD AE AF AG AI AL AM AO AQ AR AS AT AU AW AX AZ BA BB BD BE BF BG BH BI BJ BL BM BN BO BQ BR BS BT BV BW BY BZ CA CC CD CF CG CH CI CK CL CM CN CO CR CU CV CW CX CY CZ DE DJ DK DM DO DZ EC EE EG EH ER ES ET FI FJ FK FM FO FR GA GB GD GE GF GG GH GI GL GM GN GP GQ GR GS GT GU GW GY HK HM HN HR HT HU ID IE IL IM IN IO IQ IR IS IT JE JM JO JP KE KG KH KI KM KN KP KR KW KY KZ LA LB LC LI LK LR LS LT LU LV LY MA MC MD ME MF MG MH MK ML MM MN MO MP MQ MR MS MT MU MV MW MX MY MZ NA NC NE NF NG NI NL NO NP NR NU NZ OM PA PE PF PG PH PK PL PM PN PR PS PT PW PY QA RE RO RS RU RW SA SB SC SD SE SG SH SI SJ SK SL SM SN SO SR SS ST SV SX SY SZ TC TD TF TG TH TJ TK TL TM TN TO TR TT TV TW TZ UA UG UM US UY UZ VA VC VE VG VI VN VU WF WS YE YT ZA ZM ZW "

// Only fixed categories and numeric transport facts cross the Worker boundary.
// Raw stderr, response JSON, exit IP and proxy credentials are never retained.
type adobeCountryDiagnostics struct {
	FailureKind   string `json:"failure_kind"`
	CurlExitCode  *int   `json:"curl_exit_code,omitempty"`
	HTTPStatus    int    `json:"http_status"`
	ConnectStatus int    `json:"connect_status"`
	ElapsedMS     int    `json:"elapsed_ms"`
}

func (d *adobeCountryDiagnostics) valid(source string) bool {
	if d == nil {
		return true
	} // Historical profiles did not record a reason.
	if !((d.HTTPStatus == 0 || d.HTTPStatus >= 100 && d.HTTPStatus <= 599) &&
		(d.ConnectStatus == 0 || d.ConnectStatus >= 100 && d.ConnectStatus <= 599)) ||
		d.ElapsedMS < 0 || d.ElapsedMS > 60000 || d.CurlExitCode != nil && (*d.CurlExitCode < 0 || *d.CurlExitCode > 255) {
		return false
	}
	if _, ok := adobeCountryFailureLabels[d.FailureKind]; !ok {
		return false
	}
	return source == "default_us" && d.FailureKind != "none" || source != "default_us" && d.FailureKind == "none"
}

var adobeCountryFailureLabels = map[string]string{
	"none": "定位成功", "timeout": "定位请求超时", "proxy_auth": "代理认证失败",
	"proxy_tunnel": "代理隧道失败", "proxy_connection": "代理连接失败", "connection_failed": "网络连接失败",
	"dns": "DNS解析失败", "tls": "TLS握手或证书失败", "http_error": "定位服务HTTP错误",
	"http_redirect": "定位服务返回重定向", "lookup_rejected": "定位服务拒绝查询",
	"invalid_response": "定位响应格式不符", "response_too_large": "定位响应超限",
	"spawn_failed": "定位进程启动失败", "transport_failed": "定位请求传输失败",
}

func (d *adobeCountryDiagnostics) detail() string {
	if d == nil {
		return "；历史定位原因未记录"
	}
	detail := "；原因：" + adobeCountryFailureLabels[d.FailureKind]
	if d.CurlExitCode != nil {
		detail += fmt.Sprintf("；curl=%d", *d.CurlExitCode)
	}
	return detail + fmt.Sprintf("；HTTP=%d；CONNECT=%d；耗时%dms", d.HTTPStatus, d.ConnectStatus, d.ElapsedMS)
}

func cloneAdobeCountryDiagnostics(d *adobeCountryDiagnostics) *adobeCountryDiagnostics {
	if d == nil {
		return nil
	}
	copied := *d
	if d.CurlExitCode != nil {
		value := *d.CurlExitCode
		copied.CurlExitCode = &value
	}
	return &copied
}

// Decode optional diagnostics independently so a malformed observation cannot
// discard the surrounding country, creation result or persisted identity.
func parseAdobeCountryDiagnostics(raw json.RawMessage, source string) *adobeCountryDiagnostics {
	if len(raw) == 0 || len(raw) > 1024 {
		return nil
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return nil
	}
	for key, value := range fields {
		switch key {
		case "failure_kind", "curl_exit_code", "http_status", "connect_status", "elapsed_ms":
			if strings.TrimSpace(string(value)) == "null" {
				return nil
			}
		default:
			return nil
		}
	}
	for _, key := range []string{"failure_kind", "http_status", "connect_status", "elapsed_ms"} {
		if _, ok := fields[key]; !ok {
			return nil
		}
	}
	var diagnostics adobeCountryDiagnostics
	if json.Unmarshal(raw, &diagnostics) != nil || !diagnostics.valid(source) {
		return nil
	}
	return &diagnostics
}

type adobeCountrySelection struct {
	Country     string                   `json:"country"`
	Source      string                   `json:"source"`
	Diagnostics *adobeCountryDiagnostics `json:"diagnostics,omitempty"`
}

func (c *adobeCountrySelection) UnmarshalJSON(raw []byte) error {
	type selection adobeCountrySelection
	var wire struct {
		selection
		Diagnostics json.RawMessage `json:"diagnostics"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		return err
	}
	*c = adobeCountrySelection(wire.selection)
	c.Diagnostics = parseAdobeCountryDiagnostics(wire.Diagnostics, c.Source)
	return nil
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
	if !selection.Diagnostics.valid(selection.Source) {
		selection.Diagnostics = nil
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
		p.CountryDiagnostics = cloneAdobeCountryDiagnostics(selection.Diagnostics)
		detail := "沿用本次注册已保存的国家/地区：" + selection.Country
		if selection.Source == "ipwho_is" {
			detail = "按浏览器 Worker 的本次出口设置国家/地区：" + selection.Country + "（来源 ipwho.is）"
		}
		if selection.Source == "default_us" {
			detail = "浏览器出口国家定位失败，实际出口国家未确认；沿用默认国家/地区 US；未切换出口"
		}
		if selection.Diagnostics != nil || selection.Source == "default_us" {
			detail += selection.Diagnostics.detail()
		}
		appendRegistrationLog(job, "adobe_registration_country", detail, "info")
	})
	if err != nil {
		return err
	}
	return rejected
}
