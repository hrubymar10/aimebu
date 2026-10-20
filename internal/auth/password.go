package auth

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"

	"github.com/goccy/go-json"
)

const passwordAlgorithm = "pbkdf2-hmac-sha256"

// OWASP's Password Storage Cheat Sheet recommends 600,000 iterations for
// PBKDF2-HMAC-SHA256. A fresh 16-byte salt gives 128 random bits, comfortably
// above NIST SP 800-63B's 32-bit minimum while staying compact. The policy is
// stored per hash so a future increase can rehash on successful login.
// https://cheatsheetseries.owasp.org/cheatsheets/Password_Storage_Cheat_Sheet.html
// https://pages.nist.gov/800-63-4/sp800-63b.html
var DefaultPasswordPolicy = PasswordPolicy{Iterations: 600_000, SaltBytes: 16}

type PasswordPolicy struct {
	Iterations int
	SaltBytes  int
}

type passwordParams struct {
	Algorithm  string `json:"algo"`
	Iterations int    `json:"iterations"`
	SaltLength int    `json:"salt_length"`
	Salt       string `json:"salt"`
}

func (policy PasswordPolicy) valid() bool {
	return policy.Iterations > 0 && policy.Iterations <= 10_000_000 &&
		policy.SaltBytes >= 16 && policy.SaltBytes <= 64
}

func HashPassword(password string, policy PasswordPolicy) (hash, params string, err error) {
	if !policy.valid() {
		return "", "", errors.New("invalid password policy")
	}
	salt := make([]byte, policy.SaltBytes)
	if _, err := rand.Read(salt); err != nil {
		return "", "", err
	}
	derived, err := pbkdf2.Key(sha256.New, password, salt, policy.Iterations, sha256.Size)
	if err != nil {
		return "", "", err
	}
	encoded, err := json.Marshal(passwordParams{passwordAlgorithm, policy.Iterations,
		len(salt), base64.RawStdEncoding.EncodeToString(salt)})
	if err != nil {
		return "", "", err
	}
	return base64.RawStdEncoding.EncodeToString(derived), string(encoded), nil
}

// VerifyPassword returns whether the password matches and whether its policy
// needs an upgrade. Callers give unknown users equivalent PBKDF2 work.
func VerifyPassword(password, hash, params string, policy PasswordPolicy) (matched, rehash bool, err error) {
	return verifyPasswordAtCost(password, hash, params, policy, policy.Iterations)
}

func verifyPasswordAtCost(password, hash, params string, policy PasswordPolicy, floorIterations int) (matched, rehash bool, err error) {
	var stored passwordParams
	if err := json.Unmarshal([]byte(params), &stored); err != nil {
		return false, false, err
	}
	if stored.Algorithm != passwordAlgorithm || stored.Iterations < 1 ||
		stored.Iterations > 10_000_000 || stored.SaltLength < 16 || stored.SaltLength > 64 {
		return false, false, errors.New("unsupported password parameters")
	}
	salt, err := base64.RawStdEncoding.DecodeString(stored.Salt)
	if err != nil || len(salt) != stored.SaltLength {
		return false, false, errors.New("invalid stored salt")
	}
	want, err := base64.RawStdEncoding.DecodeString(hash)
	if err != nil || len(want) != sha256.Size {
		return false, false, errors.New("invalid stored password hash")
	}
	got, err := pbkdf2.Key(sha256.New, password, salt, stored.Iterations, sha256.Size)
	if err != nil {
		return false, false, err
	}
	// Equalize old and current hash policies, including the strongest stored
	// user policy. Unknown logins run the same aggregate PBKDF2 iteration count.
	if floorIterations > stored.Iterations {
		_, _ = pbkdf2.Key(sha256.New, password, make([]byte, policy.SaltBytes), floorIterations-stored.Iterations, sha256.Size)
	}
	matched = subtle.ConstantTimeCompare(got, want) == 1
	return matched, matched && (stored.Iterations < policy.Iterations || stored.SaltLength < policy.SaltBytes), nil
}

func dummyPasswordWork(password string, policy PasswordPolicy) {
	_, _ = pbkdf2.Key(sha256.New, password, make([]byte, policy.SaltBytes), policy.Iterations, sha256.Size)
}
