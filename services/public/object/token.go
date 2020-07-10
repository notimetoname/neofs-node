package object

import (
	"context"
	"crypto/ecdsa"

	"github.com/nspcc-dev/neofs-api-go/service"
	crypto "github.com/nspcc-dev/neofs-crypto"
	"github.com/nspcc-dev/neofs-node/internal"
	"github.com/nspcc-dev/neofs-node/lib/core"
)

type sessionTokenVerifier interface {
	verifySessionToken(context.Context, service.SessionToken) error
}

type complexTokenVerifier struct {
	verifiers []sessionTokenVerifier
}

type tokenSignatureVerifier struct {
	ownerKeys []*ecdsa.PublicKey
}

type tokenEpochsVerifier struct {
	epochRecv EpochReceiver
}

type tokenPreProcessor struct {
	keyVerifier core.OwnerKeyVerifier

	staticVerifier sessionTokenVerifier
}

const errCreatedAfterExpiration = internal.Error("creation epoch number is greater than expired one")

const errTokenExpired = internal.Error("token is expired")

const errForbiddenSpawn = internal.Error("request spawn is forbidden")

func (s tokenPreProcessor) preProcess(ctx context.Context, req serviceRequest) error {
	token := req.GetSessionToken()
	if token == nil {
		return nil
	}

	if !allowedSpawn(token.GetVerb(), req.Type()) {
		return errForbiddenSpawn
	}

	if err := s.keyVerifier.VerifyKey(ctx, token); err != nil {
		return err
	}

	ownerKeyBytes := token.GetOwnerKey()

	verifier := newComplexTokenVerifier(
		s.staticVerifier,
		&tokenSignatureVerifier{
			ownerKeys: []*ecdsa.PublicKey{
				crypto.UnmarshalPublicKey(ownerKeyBytes),
			},
		},
	)

	return verifier.verifySessionToken(ctx, token)
}

func newComplexTokenVerifier(verifiers ...sessionTokenVerifier) sessionTokenVerifier {
	return &complexTokenVerifier{
		verifiers: verifiers,
	}
}

func (s complexTokenVerifier) verifySessionToken(ctx context.Context, token service.SessionToken) error {
	for i := range s.verifiers {
		if s.verifiers[i] == nil {
			continue
		} else if err := s.verifiers[i].verifySessionToken(ctx, token); err != nil {
			return err
		}
	}

	return nil
}

func (s tokenSignatureVerifier) verifySessionToken(ctx context.Context, token service.SessionToken) error {
	verifiedToken := service.NewVerifiedSessionToken(token)

	for i := range s.ownerKeys {
		if err := service.VerifySignatureWithKey(s.ownerKeys[i], verifiedToken); err != nil {
			return err
		}
	}

	return nil
}

func (s tokenEpochsVerifier) verifySessionToken(ctx context.Context, token service.SessionToken) error {
	if expired := token.ExpirationEpoch(); token.CreationEpoch() > expired {
		return errCreatedAfterExpiration
	} else if s.epochRecv.Epoch() > expired {
		return errTokenExpired
	}

	return nil
}
