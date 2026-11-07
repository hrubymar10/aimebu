package adapter

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestStderrTailBoundsAndRedactsCredentials(t *testing.T) {
	var tail StderrTail
	_, _ = tail.Write([]byte(strings.Repeat("x", 5000)))
	_, _ = tail.Write([]byte("\nAuthorization: Bearer aak_secret\nkey=aak_another\nDocker is not running\n"))
	got := tail.String()
	if len(got) > 4096 || strings.Contains(got, "aak_secret") || strings.Contains(got, "aak_another") || !strings.Contains(got, "Docker is not running") {
		t.Fatalf("unsafe stderr tail: %q", got)
	}
}

func TestRedactSecretsKeepsTheRestOfTheText(t *testing.T) {
	got := RedactSecrets("key=aak_abc-123_x done\nAuthorization: Bearer eyJ.tok.en and more\nbearer\tlower-token ok")
	if strings.Contains(got, "aak_abc") || strings.Contains(got, "eyJ.tok.en") || strings.Contains(got, "lower-token") {
		t.Fatalf("a secret survived: %q", got)
	}
	for _, kept := range []string{"key=[redacted] done", "Authorization: Bearer [redacted] and more", "ok"} {
		if !strings.Contains(got, kept) {
			t.Fatalf("%q was not kept in %q", kept, got)
		}
	}
	if RedactSecrets("a\xffb") != "a�b" {
		t.Fatalf("invalid UTF-8 was not replaced")
	}
}

// What a harness prints on a failure can carry any credential it holds, not only the agent's own key: aimebu's machine key and session
// token, a provider's API key, a header that holds one, and a cookie. All of it goes before the output is stored or shown, and the words
// around it stay, including ordinary identifiers that merely contain a short prefix.
func TestRedactSecretsCoversOtherKeysHeadersAndCookies(t *testing.T) {
	secrets := []string{
		"amk_Zm9vYmFyYmF6cXV4", "as_0123456789abcdefghij", "sk-ant-api03-AbCdEfGhIjKlMnOp", "sk-proj-AbCdEfGhIjKlMnOpQr",
		"live-key-value-1", "cookie-session-value", "second=pair", "tok-9f8e7d",
	}
	text := "machine amk_Zm9vYmFyYmF6cXV4 session as_0123456789abcdefghij\n" +
		"anthropic sk-ant-api03-AbCdEfGhIjKlMnOp openai sk-proj-AbCdEfGhIjKlMnOpQr\n" +
		"x-api-key: live-key-value-1\n" +
		"Cookie: aimebu_session=cookie-session-value; other=second=pair\n" +
		"Set-Cookie: sid=cookie-session-value; Path=/\n" +
		`config {"access_token": "tok-9f8e7d", "keep": 1}` + "\n" +
		"has_value=1 task-list sk-short as_short"
	got := RedactSecrets(text)
	for _, secret := range secrets {
		if strings.Contains(got, secret) {
			t.Fatalf("%q survived in %q", secret, got)
		}
	}
	for _, kept := range []string{"machine [redacted] session [redacted]", "x-api-key: [redacted]", "Cookie: [redacted]", "Set-Cookie: [redacted]", `"keep": 1`, "has_value=1 task-list sk-short as_short"} {
		if !strings.Contains(got, kept) {
			t.Fatalf("%q was not kept in %q", kept, got)
		}
	}
}

// The assignments a harness echoes from its environment and config: any name ending in key, token, secret or password (the underscore before it
// is no word boundary), the hosts' token prefixes, and the Basic scheme. The surrounding words stay, and ordinary prose and identifiers do not
// turn into [redacted].
func TestRedactSecretsCoversEnvironmentLinesAndTokenFamilies(t *testing.T) {
	secrets := []string{"sk-ant-env-0123456789abcdef", "sk-openai-env-0123456789", "ghp_0123456789abcdefghijABCDEF", "github_pat_11ABCDEFG0123456789_abcdef", "refresh-value-123",
		"client-secret-value", "plain-token-value", "dXNlcjpwYXNzd29yZA==", "hunter2hunter2", "xoxb-1234567890-abcdefghij", "AKIAABCDEFGHIJKLMNOP", "idtok-value-9"}
	text := "ANTHROPIC_API_KEY=sk-ant-env-0123456789abcdef\n" +
		"OPENAI_API_KEY=sk-openai-env-0123456789\n" +
		"GH_TOKEN=ghp_0123456789abcdefghijABCDEF GITHUB_PAT=github_pat_11ABCDEFG0123456789_abcdef\n" +
		`{"refresh_token":"refresh-value-123","client_secret":"client-secret-value","id_token":"idtok-value-9","keep":true}` + "\n" +
		"token: plain-token-value\n" +
		"Authorization: Basic dXNlcjpwYXNzd29yZA==\n" +
		"DB_PASSWORD=hunter2hunter2\n" +
		"slack xoxb-1234567890-abcdefghij aws AKIAABCDEFGHIJKLMNOP\n" +
		"Basic usage of the task-list-item-for-the-test and a has_token_count of 3 and tokens are counted: 4"
	got := RedactSecrets(text)
	for _, secret := range secrets {
		if strings.Contains(got, secret) {
			t.Fatalf("%q survived in %q", secret, got)
		}
	}
	for _, kept := range []string{"ANTHROPIC_API_KEY=[redacted]", "GH_TOKEN=[redacted]", `"keep":true`, "token: [redacted]", "Authorization: Basic [redacted]", "DB_PASSWORD=[redacted]",
		"Basic usage of the task-list-item-for-the-test and a has_token_count of 3 and tokens are counted: 4"} {
		if !strings.Contains(got, kept) {
			t.Fatalf("%q was not kept in %q", kept, got)
		}
	}
}

type failingWriter struct{ writes int }

func (f *failingWriter) Write(p []byte) (int, error) { f.writes++; return 0, errors.New("disk full") }

func TestJoinWritersNeverLetsOneFailureStopTheOthers(t *testing.T) {
	if JoinWriters() != nil || JoinWriters(nil, nil) != nil {
		t.Fatal("no writers is nil")
	}
	var kept bytes.Buffer
	failing := &failingWriter{}
	joined := JoinWriters(failing, nil, &kept)
	n, err := io.Copy(joined, strings.NewReader("one\ntwo\n"))
	if err != nil || n != 8 || kept.String() != "one\ntwo\n" || failing.writes == 0 {
		t.Fatalf("copy = %d, %v; kept %q; failing wrote %d times", n, err, kept.String(), failing.writes)
	}
	// A single failing writer still reports success to the copy that feeds it: the harness's stderr pipe must keep being read.
	if _, err := JoinWriters(&failingWriter{}).Write([]byte("x")); err != nil {
		t.Fatalf("a failing capture failed the copy: %v", err)
	}
}
