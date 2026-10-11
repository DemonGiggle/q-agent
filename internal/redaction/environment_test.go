package redaction

import (
	"reflect"
	"testing"
)

func TestCredentialEnvironmentFilter(t *testing.T) {
	environment := []string{"PATH=/bin", "HOME=/home/service", "LANG=C.UTF-8", "VISIBLE=keep=equals", "CUSTOM_AUTH=configured-value", "bot_auth=bot-value", "AWS_ACCESS_KEY_ID=listed-value", "OPENAI_API_KEY=provider-value", "SERVICE_tOkEn=token-value", "my_Secret=secret-value", "PASSWORD=password-value", "service_passwd=passwd-value", "Credentials=credential-value", "vendor_Api_Key=key-value", "vendor_APIKEY=apikey-value", "EMPTY_SECRET="}
	safe, removed := FilterEnvironment(environment, "CUSTOM_AUTH", "BOT_AUTH")
	want := environment[:4]
	if !reflect.DeepEqual(safe, want) || len(removed) != 11 {
		t.Fatalf("safe=%v removed count=%d", safe, len(removed))
	}
	policy, _ := New(Config{CustomPatterns: []string{"custom-pattern"}}, []string{"existing-value"})
	extended := policy.WithValues(removed)
	for _, value := range append(removed, "existing-value", "custom-pattern") {
		for _, sink := range []Sink{Terminal, Persistence, Remote} {
			if got := extended.Text(sink, value); got != Marker {
				t.Errorf("value not redacted at sink %v", sink)
			}
		}
	}
	if policy.Text(Remote, "token-value") != "token-value" {
		t.Fatal("extending a policy mutated it")
	}
	safe[0] = "changed"
	if environment[0] != "PATH=/bin" {
		t.Fatal("filter aliases service environment")
	}
}

func TestEnvironmentValuesIncludesPatternCredentials(t *testing.T) {
	t.Setenv("EXTRA_tOkEn", "environment-test-value")
	found := false
	for _, value := range EnvironmentValues() {
		found = found || value == "environment-test-value"
	}
	if !found {
		t.Fatal("pattern credential missing from startup redaction")
	}
}
