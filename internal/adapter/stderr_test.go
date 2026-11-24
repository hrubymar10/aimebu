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

// The shapes a secret has when nothing names it: a private key in PEM armour, across its lines, and the tokens of the common hosts and
// providers by their prefixes, wherever they stand in a line. Ordinary text that only looks a little like one is left as it is.
func TestRedactSecretsCoversPrivateKeysAndPrefixedTokens(t *testing.T) {
	body := "b3BlbnNzaC1rZXktdjEAAAAABG5vbmUAAAAEbm9uZQAAAAAAAAABAAABlwAAAAdzc2gtcn\nNhAAAAAwEAAQAAAYEAu1vPz0Zq8dQw0n3J5mXo2b7yq9sKf4hT6LrWc1pVgEaYxN2UoBdI\n"
	for _, tc := range []struct{ name, in, want string }{
		{"an OpenSSH key over several lines", "before\n-----BEGIN OPENSSH PRIVATE KEY-----\n" + body + "-----END OPENSSH PRIVATE KEY-----\nafter", "before\n[redacted private key]\nafter"},
		{"an RSA key", "-----BEGIN RSA PRIVATE KEY-----\n" + body + "-----END RSA PRIVATE KEY-----", "[redacted private key]"},
		{"a PKCS#8 key", "x -----BEGIN PRIVATE KEY-----\n" + body + "-----END PRIVATE KEY----- y", "x [redacted private key] y"},
		{"an encrypted key", "-----BEGIN ENCRYPTED PRIVATE KEY-----\n" + body + "-----END ENCRYPTED PRIVATE KEY-----", "[redacted private key]"},
		{"a key inside a JSON string", `{"content":"-----BEGIN EC PRIVATE KEY-----\nMHcCAQEEIIrYSSNQFaA2Hwf1duRSxKtLYX5CB04fSeQ6tF1aY\n-----END EC PRIVATE KEY-----\n","ok":true}`, `{"content":"[redacted private key]\n","ok":true}`},
		{"two keys keep what is between them", "-----BEGIN PRIVATE KEY-----\nAAAA\n-----END PRIVATE KEY-----\nmiddle\n-----BEGIN PRIVATE KEY-----\nBBBB\n-----END PRIVATE KEY-----", "[redacted private key]\nmiddle\n[redacted private key]"},
		{"a key cut before its end", "log line\n-----BEGIN OPENSSH PRIVATE KEY-----\n" + body, "log line\n[redacted private key]"},
		{"a key cut before its beginning", "kept text\n" + body + "-----END OPENSSH PRIVATE KEY-----\nafter", "kept text\n[redacted private key]\nafter"},
		{"a public key is not a secret", "-----BEGIN PUBLIC KEY-----\nMFkwEwYHKoZIzj0CAQYI\n-----END PUBLIC KEY-----", "-----BEGIN PUBLIC KEY-----\nMFkwEwYHKoZIzj0CAQYI\n-----END PUBLIC KEY-----"},
		{"a certificate is not a secret", "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----", "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----"},
		{"a classic host token in a longer line", "remote: pushing with ghp_0123456789abcdefghijABCDEFGHIJ0123456789 to origin", "remote: pushing with [redacted] to origin"},
		{"an OAuth host token", "(gho_0123456789abcdefghijABCDEFGHIJ0123456789)", "([redacted])"},
		{"a fine-grained host token", "url=https://x:github_pat_11ABCDEFG0123456789_abcdefghijklmnopqrstuvwxyz@example.test/r.git", "url=https://x:[redacted]@example.test/r.git"},
		{"a provider key", "using sk-ant-api03-0123456789abcdefghij-ABCDEFGHIJ for this", "using [redacted] for this"},
		{"a short provider key", "export K=sk-0123456789abcdefghij0123", "export K=[redacted]"},
		{"a chat bot token", "posting as xoxb-1234567890-abcdefghijkl now", "posting as [redacted] now"},
		{"a chat user token", "xoxp-1234567890-abcdefghijkl", "[redacted]"},
		{"a cloud access key id", "aws configure set id AKIAIOSFODNN7EXAMPLE ok", "aws configure set id [redacted] ok"},
		{"ordinary words with the same letters", "the task-list has_value asks: skip the sk-ip step, ghp is short, xoxo, AKIA is a prefix", "the task-list has_value asks: skip the sk-ip step, ghp is short, xoxo, AKIA is a prefix"},
		{"a path, a hash and a sentence", "internal/agentlog/agentlog.go:533 at 8a5c3ce9767e8126b9883d5ad0c9dd34699b232c: BEGIN and END are words", "internal/agentlog/agentlog.go:533 at 8a5c3ce9767e8126b9883d5ad0c9dd34699b232c: BEGIN and END are words"},
		{"a dashed rule", "----- BEGIN the report -----\nbody\n----- END -----", "----- BEGIN the report -----\nbody\n----- END -----"},
	} {
		if got := RedactSecrets(tc.in); got != tc.want {
			t.Errorf("%s:\n got %q\nwant %q", tc.name, got, tc.want)
		}
		// The conversation log uses the same rules, and so does the tail kept as exit evidence.
		if got := CleanConversationText(tc.in, ConversationToolLimit); got != tc.want {
			t.Errorf("%s in a conversation record:\n got %q\nwant %q", tc.name, got, tc.want)
		}
	}
	if !OpensPrivateKey("x -----BEGIN RSA PRIVATE KEY-----") || OpensPrivateKey("-----BEGIN PRIVATE KEY-----AAAA-----END PRIVATE KEY-----") ||
		OpensPrivateKey("-----BEGIN PUBLIC KEY-----") || !ClosesPrivateKey("-----END EC PRIVATE KEY-----") || ClosesPrivateKey("-----END CERTIFICATE-----") {
		t.Error("the line-by-line reader's view of a private key is wrong")
	}
}

// PrivateKeyLines drops the lines of a key after its BEGIN line, END included; with a limit it gives up after that many lines, without
// one it drops to the end; an END that a new BEGIN follows on the same line starts the next key.
func TestPrivateKeyLinesFollowsAKeyAcrossLines(t *testing.T) {
	skipped := func(k *PrivateKeyLines, lines ...string) string {
		var out []string
		for _, line := range lines {
			if !k.Skip(line) {
				out = append(out, line)
			}
		}
		return strings.Join(out, "|")
	}
	if got := skipped(&PrivateKeyLines{}, "a", "-----BEGIN PRIVATE KEY-----", "AAAA", "BBBB", "-----END PRIVATE KEY-----", "b"); got != "a|-----BEGIN PRIVATE KEY-----|b" {
		t.Fatalf("one key: %q", got)
	}
	unbounded := &PrivateKeyLines{}
	if got := skipped(unbounded, "a", "-----BEGIN PRIVATE KEY-----", "AAAA", "x", "y"); got != "a|-----BEGIN PRIVATE KEY-----" || !unbounded.Open() {
		t.Fatalf("unterminated, no limit: %q open=%t", got, unbounded.Open())
	}
	if got := skipped(&PrivateKeyLines{Limit: 2}, "-----BEGIN PRIVATE KEY-----", "AAAA", "BBBB", "shown"); got != "-----BEGIN PRIVATE KEY-----|shown" {
		t.Fatalf("unterminated, limit 2: %q", got)
	}
	if got := skipped(&PrivateKeyLines{}, "-----BEGIN PRIVATE KEY-----", "AAAA", "-----END PRIVATE KEY----- -----BEGIN RSA PRIVATE KEY-----", "CCCC", "-----END RSA PRIVATE KEY-----", "after"); got != "-----BEGIN PRIVATE KEY-----|after" {
		t.Fatalf("END then BEGIN on one line: %q", got)
	}
	if got := skipped(&PrivateKeyLines{}, "-----BEGIN PRIVATE KEY-----AAAA-----END PRIVATE KEY-----", "after"); got != "-----BEGIN PRIVATE KEY-----AAAA-----END PRIVATE KEY-----|after" {
		t.Fatalf("one-line key: %q", got)
	}
}
