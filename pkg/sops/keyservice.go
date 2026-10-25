package sops

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"

	"filippo.io/age"
	"filippo.io/age/armor"
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

// ageKeyService is picolet's keyservice: it unwraps age stanzas with the
// identities it holds and nothing else. It never calls SOPS's own age
// decryption, which falls back to SOPS_AGE_KEY*, keys.txt and ~/.ssh when it
// holds no identity, and it answers every other key type with an error
// instead of delegating to SOPS's default keyservice.
type ageKeyService struct {
	ids []age.Identity
}

var _ keyservice.KeyServiceClient = ageKeyService{}

// Encrypt is never needed: picolet only decrypts.
func (ageKeyService) Encrypt(context.Context, *keyservice.EncryptRequest, ...grpc.CallOption) (*keyservice.EncryptResponse, error) {
	return nil, errors.New("picolet never encrypts")
}

// Decrypt unwraps an age-wrapped data key.
func (s ageKeyService) Decrypt(_ context.Context, req *keyservice.DecryptRequest, _ ...grpc.CallOption) (*keyservice.DecryptResponse, error) {
	if req.GetKey().GetAgeKey() == nil {
		return nil, errKeyTypeUnsupported
	}
	if len(s.ids) == 0 {
		return nil, ErrNoIdentity
	}
	r, err := age.Decrypt(armor.NewReader(bytes.NewReader(req.GetCiphertext())), s.ids...)
	if err != nil {
		return nil, errors.New("age stanza does not open with these identities")
	}
	key, err := io.ReadAll(io.LimitReader(r, maxKeyPartLen+1))
	if err != nil {
		return nil, errors.New("age stanza does not open with these identities")
	}
	if len(key) == 0 || len(key) > maxKeyPartLen {
		return nil, fmt.Errorf("age stanza holds %d bytes, not a data key", len(key))
	}
	return &keyservice.DecryptResponse{Plaintext: key}, nil
}
