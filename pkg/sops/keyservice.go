package sops

import (
	"context"
	"errors"
	"fmt"

	"filippo.io/age"
	sopsage "github.com/getsops/sops/v3/age"
	"github.com/getsops/sops/v3/keyservice"
	"google.golang.org/grpc"
)

// maxKeyPartLen bounds what an age stanza may unwrap to: a 32-byte data key,
// or a Shamir share of one (33 bytes) in a key-group file.
const maxKeyPartLen = 64

// errKeyTypeUnsupported is what the keyservice answers for every master key
// that is not age (KMS, PGP, Vault, ...): picolet never contacts a key
// backend.
var errKeyTypeUnsupported = errors.New("picolet decrypts age keys only")

// ageKeyService is picolet's keyservice: it unwraps age stanzas with
// upstream's age MasterKey, given the identities it holds and nothing else.
// The MasterKey falls back to SOPS_AGE_KEY*, keys.txt and ~/.ssh only when it
// holds no identity, so Decrypt refuses an empty set before reaching it and
// that fallback is unreachable. It answers every other key type with an
// error instead of delegating to SOPS's default keyservice.
type ageKeyService struct {
	ids []age.Identity
}

var _ keyservice.KeyServiceClient = ageKeyService{}

// Encrypt is never needed: picolet only decrypts.
func (ageKeyService) Encrypt(context.Context, *keyservice.EncryptRequest, ...grpc.CallOption) (*keyservice.EncryptResponse, error) {
	return nil, errors.New("picolet never encrypts")
}

// Decrypt unwraps an age-wrapped data key. Upstream's error text is never
// passed on.
func (s ageKeyService) Decrypt(_ context.Context, req *keyservice.DecryptRequest, _ ...grpc.CallOption) (*keyservice.DecryptResponse, error) {
	ageKey := req.GetKey().GetAgeKey()
	if ageKey == nil {
		return nil, errKeyTypeUnsupported
	}
	if len(s.ids) == 0 {
		// Never reach the MasterKey without identities: it would fall back
		// to the environment, keys.txt and ~/.ssh.
		return nil, ErrNoIdentity
	}
	mk := &sopsage.MasterKey{Recipient: ageKey.GetRecipient(), EncryptedKey: string(req.GetCiphertext())}
	sopsage.ParsedIdentities(s.ids).ApplyToMasterKey(mk)
	key, err := mk.Decrypt()
	if err != nil {
		return nil, errors.New("age stanza does not open with these identities")
	}
	if len(key) == 0 || len(key) > maxKeyPartLen {
		return nil, fmt.Errorf("age stanza holds %d bytes, not a data key", len(key))
	}
	return &keyservice.DecryptResponse{Plaintext: key}, nil
}
