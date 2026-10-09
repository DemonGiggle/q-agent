package redaction

import (
	"regexp"
	"strings"
)

// A checksum avoids treating every long timestamp or counter as a card number.
// Explicit payment-card labels also protect test/invalid numbers without one.
// Tabs delimit columns, including the read tool's line-number column. Keeping
// them out of PAN grouping prevents a line number from hiding the card number.
const cardDigits = `[0-9](?:[ -]*[0-9]){12,18}`

var cardCandidate = regexp.MustCompile(cardDigits)

const paymentGap = `[ \t_-]*`
const cardLabel = `(?:credit` + paymentGap + `card(?:` + paymentGap + `(?:number|no\.?))?|debit` + paymentGap + `card(?:` + paymentGap + `(?:number|no\.?))?|payment` + paymentGap + `card(?:` + paymentGap + `number)?|card` + paymentGap + `(?:number|no\.?|num)|cc` + paymentGap + `(?:number|num)|pan)`
const securityLabel = `(?:cvv2?|cvc2?|cid|card` + paymentGap + `(?:security` + paymentGap + `code|verification` + paymentGap + `(?:code|value))|security` + paymentGap + `code)`
const expiryLabel = `(?:(?:card` + paymentGap + `)?(?:expir(?:y|ation|es|e)|exp\.)(?:` + paymentGap + `date)?|(?:valid|validate|validation|validity)` + paymentGap + `(?:date|thru|through|until)|thru)`

// Labels may be Markdown headings/table cells or natural-language mentions.
// Capture only the value so formatting and useful labels survive filtering.
const paymentSeparator = `["'*_` + "`" + ` \t]*(?:(?::|=|\||-|\bis\b|\bof\b|\bwas\b)[ \t]*)?["'*_` + "`" + ` \t]*`

var labelledCard = regexp.MustCompile(`(?i)\b` + cardLabel + paymentSeparator + `(` + cardDigits + `)`)
var labelledSecurity = regexp.MustCompile(`(?i)\b` + securityLabel + paymentSeparator + `([0-9]{3,4})\b`)
var labelledExpiry = regexp.MustCompile(`(?i)\b` + expiryLabel + paymentSeparator + `((?:[0-9]{4}[-/.](?:0?[1-9]|1[0-2])(?:[-/.][0-9]{1,2})?|(?:0?[1-9]|1[0-2])[-/.][0-9]{1,2}[-/.][0-9]{2,4}|(?:0?[1-9]|1[0-2])[ \t]*[-/.][ \t]*[0-9]{2,4}|(?:Jan(?:uary)?|Feb(?:ruary)?|Mar(?:ch)?|Apr(?:il)?|May|Jun(?:e)?|Jul(?:y)?|Aug(?:ust)?|Sep(?:tember)?|Oct(?:ober)?|Nov(?:ember)?|Dec(?:ember)?)[ \t]+[0-9]{4}))\b`)
var labelledExpiryPart = regexp.MustCompile(`(?i)\b(?:card` + paymentGap + `)?(?:exp|expiry|expiration)` + paymentGap + `(?:month|year)` + paymentSeparator + `([0-9]{1,4})\b`)

var paymentFields = []string{
	"card_number", "credit_card", "credit_card_number", "debit_card_number", "payment_card_number", "cc_number", "cc_num", "pan",
	"cvv", "cvv2", "cvc", "cvc2", "cid", "security_code", "card_security_code", "card_verification_code", "card_verification_value",
	"expiry", "expiry_date", "expiration", "expiration_date", "expires", "expire_date", "card_expiry", "card_expiry_date", "card_expiration", "card_expiration_date", "valid_thru", "valid_through", "valid_until", "valid_date", "validate_date", "validation_date", "thru", "exp_month", "exp_year", "expiry_month", "expiry_year", "expiration_month", "expiration_year",
}

func redactPayments(line string) string {
	// Match labelled grouped numbers before generic assignments, which would
	// otherwise remove just their first digit group.
	for _, pattern := range []*regexp.Regexp{labelledCard, labelledSecurity, labelledExpiry, labelledExpiryPart} {
		line = protect(line, func(piece string) string { return replacePaymentValue(piece, pattern) })
	}
	return protect(line, func(piece string) string {
		matches := cardCandidate.FindAllStringIndex(piece, -1)
		var out strings.Builder
		cursor := 0
		for _, match := range matches {
			if !cardBoundary(piece, match[0], match[1]) || !luhn(piece[match[0]:match[1]]) {
				continue
			}
			out.WriteString(piece[cursor:match[0]])
			out.WriteString(Marker)
			cursor = match[1]
		}
		out.WriteString(piece[cursor:])
		return out.String()
	})
}
func replacePaymentValue(text string, pattern *regexp.Regexp) string {
	matches := pattern.FindAllStringSubmatchIndex(text, -1)
	var out strings.Builder
	cursor := 0
	for _, match := range matches {
		start, end := match[2], match[3]
		if pattern == labelledCard && !cardBoundary(text, start, end) {
			continue
		}
		out.WriteString(text[cursor:start])
		out.WriteString(Marker)
		cursor = end
	}
	out.WriteString(text[cursor:])
	return out.String()
}
func cardBoundary(text string, start, end int) bool {
	// A candidate must not be a substring of a longer digit sequence, including
	// longer sequences written with the same space/hyphen grouping.
	for i := start - 1; i >= 0; i-- {
		if text[i] == ' ' || text[i] == '-' {
			continue
		}
		if text[i] >= '0' && text[i] <= '9' {
			return false
		}
		break
	}
	for i := end; i < len(text); i++ {
		if text[i] == ' ' || text[i] == '-' {
			continue
		}
		if text[i] >= '0' && text[i] <= '9' {
			return false
		}
		break
	}
	return true
}
func luhn(value string) bool {
	sum, count := 0, 0
	nonzero := false
	for i := len(value) - 1; i >= 0; i-- {
		digit := value[i]
		if digit < '0' || digit > '9' {
			continue
		}
		number := int(digit - '0')
		nonzero = nonzero || number != 0
		if count%2 == 1 {
			number *= 2
			if number > 9 {
				number -= 9
			}
		}
		sum += number
		count++
	}
	return count >= 13 && count <= 19 && nonzero && sum%10 == 0
}
