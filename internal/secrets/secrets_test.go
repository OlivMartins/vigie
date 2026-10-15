package secrets

import (
	"encoding/json"
	"strings"
	"testing"
)

// Sample credentials are assembled at run time so that the repository itself
// never contains anything a secret scanner would flag.
func sample(parts ...string) string { return strings.Join(parts, "") }

var samples = map[string]string{
	"private_key":       sample("-----BEGIN ", "OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXktdjE=\n-----END OPENSSH PRIVATE KEY-----"),
	"aws_access_key_id": sample("AKIA", "Z7QJ4XWVN2L5KT3M"),
	"github_token":      sample("ghp_", strings.Repeat("aB3dE5fG7h", 3), "IjKlMn"),
	"gitlab_token":      sample("glpat-", "x8Yz2Wq4Rt6Uv1Sa3Bc5"),
	"slack_token":       sample("xoxb-", "1234567890-0987654321-AbCdEf"),
	"stripe_secret_key": sample("sk_", "live_", "4eC39HqLyjWDarjtT1zdp7dc"),
	"anthropic_api_key": sample("sk-ant-", "api03-", strings.Repeat("Ab3_-x9Kq2", 9)),
	"openai_api_key":    sample("sk-proj-", strings.Repeat("Ab3x9Kq2Zw", 3), "T3Blbk", "FJ", strings.Repeat("Qp7Lm2Xc9V", 3)),
	"google_api_key":    sample("AIza", "SyD8f7Gh6Jk5Lm4Np3Qr2St1Uv0Wx9Yz8Ab"),
	"jwt":               sample("eyJ", "hbGciOiJIUzI1NiJ9", ".eyJ", "zdWIiOiIxMjM0NTY3ODkwIn0", ".", "dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U"),
	"url_credentials":   sample("postgres://", "app:", "s3cr3t-pass", "@db.internal:5432/billing"),
}

func TestEveryRuleHasASample(t *testing.T) {
	for _, id := range NewScanner().Rules() {
		if _, ok := samples[id]; !ok {
			t.Errorf("rule %s has no sample", id)
		}
	}
}

func TestRulesDetectTheirSample(t *testing.T) {
	s := NewScanner()
	for id, value := range samples {
		raw, _ := json.Marshal(map[string]any{"field": "prefix " + value + " suffix"})
		found := s.ScanJSON("arguments", raw)
		ok := false
		for _, f := range found {
			if f.Rule == id && f.Location == "arguments.field" {
				ok = true
			}
		}
		if !ok {
			t.Errorf("%s not detected in %q: %+v", id, value, found)
		}
	}
}

func TestOrdinaryTextIsQuiet(t *testing.T) {
	s := NewScanner()
	for _, text := range []string{
		"Fetch invoice F-2026-0042 for ACME and send a reminder to billing@example.com",
		"AKIAISNOTAKEY", // too short
		"The task asks for a sk-learn model",
		"https://example.com:8443/path?q=1",
		"ghp_short",
		"-----BEGIN CERTIFICATE-----",
		"eyJhbGciOiJIUzI1NiJ9 alone is a header, not a token",
	} {
		raw, _ := json.Marshal(text)
		if found := s.ScanJSON("x", raw); len(found) > 0 {
			t.Errorf("false positive on %q: %+v", text, found)
		}
	}
}

func TestLocations(t *testing.T) {
	s := NewScanner()
	key := samples["aws_access_key_id"]
	raw, _ := json.Marshal(map[string]any{
		"items":       []any{"ok", map[string]any{"note": key}},
		"odd key":     key,
		"count":       3,
		"nested":      map[string]any{"deep": map[string]any{"value": key}},
		"not_flagged": true,
	})
	var locs []string
	for _, f := range s.ScanJSON("arguments", raw) {
		locs = append(locs, f.Location)
	}
	want := `arguments.items[1].note|arguments.nested.deep.value|arguments["odd key"]`
	if strings.Join(locs, "|") != want {
		t.Errorf("locations = %v, want %s", locs, want)
	}
}

// Member names and duplicate members are scanned too: the raw text is what
// reaches the server, whatever its decoder keeps.
func TestKeysAndDuplicatesAreScanned(t *testing.T) {
	key := samples["aws_access_key_id"]
	s := NewScanner()
	found := s.ScanJSON("arguments", json.RawMessage(`{"meta":{"`+key+`":{"note":"`+samples["slack_token"]+`"}}}`))
	if len(found) != 2 || found[0].Location != "arguments.meta (member name)" || found[1].Location != "arguments.meta[REDACTED].note" {
		t.Errorf("found %+v", found)
	}
	for _, f := range found {
		if strings.Contains(f.Location, key) {
			t.Errorf("location leaks the key: %s", f.Location)
		}
	}
	dup := s.ScanJSON("arguments", json.RawMessage(`{"body":"`+key+`","body":"hello"}`))
	if len(dup) != 1 || dup[0].Location != "arguments.body" {
		t.Errorf("duplicate member hid the key: %+v", dup)
	}
	out := string(s.Redact(json.RawMessage(`{"meta":{"` + key + `":1}}`)))
	if strings.Contains(out, key) || !strings.Contains(out, "[REDACTED:aws_access_key_id]") {
		t.Errorf("member name not redacted: %s", out)
	}
	if !s.Contains("x "+key) || s.Contains("hello") {
		t.Error("Contains")
	}
	if got := s.RedactText("postgres://u:p4ss@db/x"); strings.Contains(got, "p4ss") {
		t.Errorf("RedactText = %s", got)
	}
}

func TestFindingsAreBounded(t *testing.T) {
	values := make([]string, 100)
	for i := range values {
		values[i] = samples["github_token"]
	}
	raw, _ := json.Marshal(values)
	if n := len(NewScanner().ScanJSON("arguments", raw)); n != MaxFindings {
		t.Errorf("%d findings, want %d", n, MaxFindings)
	}
}

// A credential spelled with JSON escapes is still a credential.
func TestEscapedCredentialsAreFound(t *testing.T) {
	key := samples["aws_access_key_id"]
	escaped := `{"note":"\u0041` + key[1:] + `"}` // "A" written as \u0041
	found := NewScanner().ScanJSON("arguments", json.RawMessage(escaped))
	if len(found) != 1 || found[0].Rule != "aws_access_key_id" {
		t.Errorf("escaped key not found: %+v", found)
	}
	clean := json.RawMessage(`{"note":"nothing to see here"}`)
	if out := NewScanner().Redact(clean); string(out) != string(clean) {
		t.Errorf("clean value altered by Redact: %s", out)
	}
}

func TestNonJSONIsScannedAsText(t *testing.T) {
	found := NewScanner().ScanJSON("arguments", json.RawMessage("not json "+samples["slack_token"]))
	if len(found) != 1 || found[0].Rule != "slack_token" {
		t.Errorf("found %+v", found)
	}
}

func TestRedact(t *testing.T) {
	s := NewScanner()
	raw, _ := json.Marshal(map[string]any{
		"body":  "key " + samples["aws_access_key_id"] + " and <html>",
		"count": json.Number("12.50"),
	})
	out := string(s.Redact(raw))
	if strings.Contains(out, samples["aws_access_key_id"]) {
		t.Fatalf("secret survived redaction: %s", out)
	}
	if !strings.Contains(out, "[REDACTED:aws_access_key_id]") || !strings.Contains(out, "<html>") || !strings.Contains(out, "12.50") {
		t.Errorf("redacted = %s", out)
	}
}

func FuzzScanJSON(f *testing.F) {
	f.Add([]byte(`{"a":"b"}`))
	f.Add([]byte(`["` + samples["jwt"] + `"]`))
	f.Fuzz(func(t *testing.T, data []byte) {
		s := NewScanner()
		_ = s.ScanJSON("x", data)
		_ = s.Redact(data)
	})
}
