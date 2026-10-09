package redaction

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestPaymentCardForms(t *testing.T) {
	// Public synthetic test-card numbers; never use a user's payment data here.
	for _, tt := range []struct{ name, input, want string }{
		{"compact", "Card 4111111111111111", "Card " + Marker},
		{"spaced", "- Number: 4111 1111 1111 1111", "- Number: " + Marker},
		{"hyphenated", "5555-5555-5555-4444", Marker},
		{"padded groups", "4111  1111 - 1111  1111", Marker},
		{"read line number", "     2\t4111 1111 1111 1111", "     2\t" + Marker},
		{"amex", "3782 822463 10005", Marker},
		{"thirteen digits", "4222222222222", Marker},
		{"nineteen digits", "4000000000000000006", Marker},
		{"labelled invalid checksum", "Credit card number: 4111 1111 1111 1112", "Credit card number: " + Marker},
		{"labelled json", `"card_number": "4111 1111 1111 1112"`, `"card_number": "` + Marker + `"`},
		{"markdown", "**CVV:** `123`", "**CVV:** `" + Marker + "`"},
		{"table", "| CVC | 123 |", "| CVC | " + Marker + " |"},
		{"security code", "Card security code is 1234.", "Card security code is " + Marker + "."},
		{"cvv2", "CVV2=123", "CVV2=" + Marker},
		{"prose", "CVV is 123, expiration date is 04/29.", "CVV is " + Marker + ", expiration date is " + Marker + "."},
		{"thru", "- Thru: 04/29", "- Thru: " + Marker},
		{"valid thru", "Valid Thru: 04/2029", "Valid Thru: " + Marker},
		{"validate date", "Validate Date: 04 / 29", "Validate Date: " + Marker},
		{"iso expiry", "Expiry: 2029-04", "Expiry: " + Marker},
		{"full expiry", "Expiry Date: 04/30/2029", "Expiry Date: " + Marker},
		{"named month", "Expires: April 2029", "Expires: " + Marker},
		{"snake case", "card_expiry: 04/29", "card_expiry: " + Marker},
		{"separate expiry parts", "exp_month=04 exp_year=2029", "exp_month=" + Marker + " exp_year=" + Marker},
		{"camel case", "cardNumber: 4111111111111112", "cardNumber: " + Marker},
		{"invalid unlabelled", "4111111111111112", "4111111111111112"},
		{"all zeros", "0000000000000000", "0000000000000000"},
		{"long counter", "411111111111111111111", "411111111111111111111"},
		{"short numbers and dates", "ticket 123 on 04/29; 1234 records", "ticket 123 on 04/29; 1234 records"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var p *Policy // nil selects the same built-in policy used by default.
			for _, sink := range []Sink{Terminal, Persistence, Exports, JSONEvents, Remote} {
				got := p.Text(sink, tt.input)
				if got != tt.want {
					t.Fatalf("sink %d: got %q, want %q", sink, got, tt.want)
				}
				if p.Text(sink, got) != got {
					t.Fatalf("sink %d: repeated filtering changed text", sink)
				}
			}
		})
	}
}

func TestPaymentJSONFieldsAndNumbers(t *testing.T) {
	var p *Policy
	input := []byte(`{"cardNumber":"4111 1111 1111 1112","CVV":123,"validThru":"04/29","expiration_month":4,"expiration_year":2029,"nested":{"number":4111111111111111,"text":"4111\u00201111\u00201111\u00201111"},"count":123,"date":"04/29"}`)
	got := p.JSON(Persistence, input)
	var fields map[string]any
	if err := json.Unmarshal(got, &fields); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"cardNumber", "CVV", "validThru", "expiration_month", "expiration_year"} {
		if fields[name] != Marker {
			t.Fatalf("payment field %s leaked", name)
		}
	}
	for _, value := range fields["nested"].(map[string]any) {
		if value != Marker {
			t.Fatal("JSON escaping or numeric encoding hid a card number")
		}
	}
	if fields["count"] != float64(123) || fields["date"] != "04/29" {
		t.Fatal("ordinary counter or date changed")
	}
	if string(p.JSON(Persistence, got)) != string(got) {
		t.Fatal("JSON filtering is not idempotent")
	}
	// Routing IDs and typed numeric counters retain their original representation.
	type transport struct {
		ID      string `json:"id"`
		Count   int64  `json:"count"`
		Content string `json:"content"`
	}
	original := transport{ID: "4111111111111111", Count: 4111111111111111, Content: "CVV: 123"}
	copy := Copy(p, Remote, original)
	if copy.ID != original.ID || copy.Count != original.Count || copy.Content != "CVV: "+Marker {
		t.Fatal("transport metadata changed or content leaked")
	}
}

func TestPaymentStreamingEveryBoundaryAndDisabledSinks(t *testing.T) {
	const input = "Credit card:\r\n- Number: 4111 1111 1111 1111\r\n- CVV: 123\r\n- Thru: 04/29"
	const want = "Credit card:\n- Number: " + Marker + "\n- CVV: " + Marker + "\n- Thru: " + Marker
	var p *Policy
	for _, sink := range []Sink{Terminal, Persistence, Exports, JSONEvents, Remote} {
		for split := 0; split <= len(input); split++ {
			stream := p.Stream(sink)
			got := stream.Feed(input[:split]) + stream.Feed(input[split:]) + stream.Flush()
			if got != want {
				t.Fatalf("sink %d, split %d: %q", sink, split, got)
			}
		}
		stream := p.Stream(sink)
		var got strings.Builder
		for _, b := range []byte(input) {
			got.WriteString(stream.Feed(string(b)))
		}
		got.WriteString(stream.Flush())
		if got.String() != want {
			t.Fatalf("sink %d: single-byte chunks leaked", sink)
		}
	}
	disabled := false
	optOut, _ := New(Config{Terminal: &disabled, Persistence: &disabled, Exports: &disabled, JSONEvents: &disabled, Remote: &disabled}, nil)
	for _, sink := range []Sink{Terminal, Persistence, Exports, JSONEvents, Remote} {
		if optOut.Text(sink, input) != input {
			t.Fatalf("sink %d: opt-out ignored", sink)
		}
	}
	if !optOut.ContainsSecret(input) {
		t.Fatal("learning detection depends on display switches")
	}
}
