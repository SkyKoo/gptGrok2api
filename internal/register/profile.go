package register

import (
	"crypto/rand"
	"math/big"
	"strings"
	"time"
)

// profileForEmail derives the protocol-only profile submitted when OpenAI
// requires the about-you step. The UI never exposes these fields: the name is
// derived from the generated mailbox local-part and the birthday represents a
// random age from 20 through 50.
func profileForEmail(email string) (string, string) {
	local := email
	if at := strings.LastIndexByte(local, '@'); at >= 0 {
		local = local[:at]
	}
	words := make([]string, 0, 3)
	current := strings.Builder{}
	flush := func() {
		if current.Len() == 0 || len(words) >= 3 {
			current.Reset()
			return
		}
		word := current.String()
		words = append(words, strings.ToUpper(word[:1])+word[1:])
		current.Reset()
	}
	for _, r := range strings.ToLower(local) {
		if r >= 'a' && r <= 'z' {
			current.WriteRune(r)
			continue
		}
		flush()
	}
	flush()
	if len(words) == 0 {
		words = append(words, "Openai")
	}
	if len(words) == 1 {
		words = append(words, "User")
	}
	name := strings.Join(words, " ")

	// Pick an inclusive age in [20, 50] with a cryptographically secure source
	// so repeated registrations do not follow a predictable sequence.
	ageOffset, err := rand.Int(rand.Reader, big.NewInt(31))
	if err != nil {
		// crypto/rand failure is exceptionally unlikely; retain a valid age and
		// avoid turning a usable registration into an empty profile submission.
		ageOffset = big.NewInt(0)
	}
	age := 20 + int(ageOffset.Int64())
	birthday := time.Now().UTC().AddDate(-age, 0, 0).Format("2006-01-02")
	return name, birthday
}
