package local

import (
	"errors"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/credentials"
)

// TestCredentialSpellsAbsenceOnceAndKeepsErrorsForFailures: a nil resolver, an
// *ErrMissing and a Secret that is not Present all mean "no key, and that is
// fine" on loopback, and only a real failure is an error. Merging the first
// three into one answer is what keeps the Header builder from sending an empty
// bearer.
func TestCredentialSpellsAbsenceOnceAndKeepsErrorsForFailures(t *testing.T) {
	boom := errors.New("keychain is locked")
	cases := []struct {
		name        string
		resolve     func() (credentials.Secret, error)
		wantPresent bool
		wantErr     error
	}{
		{"nil resolver", nil, false, nil},
		{"missing", func() (credentials.Secret, error) { return credentials.Secret{}, &credentials.ErrMissing{} }, false, nil},
		{"empty secret", func() (credentials.Secret, error) { return credentials.Secret{}, nil }, false, nil},
		{"failure", func() (credentials.Secret, error) { return credentials.Secret{}, boom }, false, boom},
		{"present", func() (credentials.Secret, error) { return credentials.NewSecret("k", "test"), nil }, true, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			secret, present, err := credential(tc.resolve)
			if present != tc.wantPresent || !errors.Is(err, tc.wantErr) || (err == nil) != (tc.wantErr == nil) {
				t.Fatalf("credential() present=%v err=%v, want present=%v err=%v", present, err, tc.wantPresent, tc.wantErr)
			}
			if present != secret.Present() {
				t.Fatalf("present=%v disagrees with the Secret it returned", present)
			}
		})
	}
}
