package openid

import (
	"crypto/rsa"
	"fmt"
	"net/http"

	"github.com/dgrijalva/jwt-go"
)

const issuerClaimName = "iss"
const audiencesClaimName = "aud"
const subjectClaimName = "sub"
const keyIDJwtHeaderName = "kid"

type jwtTokenValidator interface {
	validate(r *http.Request, t string) (jt *jwt.Token, err error)
}

type jwtParserFunc func(string, jwt.Keyfunc) (*jwt.Token, error)
type pemToRSAPublicKeyParserFunc func(key []byte) (*rsa.PublicKey, error)

type idTokenValidator struct {
	provGetter GetProvidersFunc
	jwtParser  jwtParserFunc
	keyGetter  signingKeyGetter
	rsaParser  pemToRSAPublicKeyParserFunc
}

func newIDTokenValidator(pg GetProvidersFunc, jp jwtParserFunc, kg signingKeyGetter, kp pemToRSAPublicKeyParserFunc) *idTokenValidator {
	return &idTokenValidator{pg, jp, kg, kp}
}

func (tv *idTokenValidator) validate(r *http.Request, t string) (*jwt.Token, error) {
	jt, err := tv.jwtParser(t, func(tok *jwt.Token) (interface{}, error) {
		return tv.getSigningKey(r, tok)
	})
	if err != nil {

		if verr, ok := err.(*jwt.ValidationError); ok {
			// If the signing key did not match it may be because the in memory key is outdated.
			// Renew the cached signing key.
			if (verr.Errors & jwt.ValidationErrorSignatureInvalid) != 0 {
				jt, err = tv.jwtParser(t, func(tok *jwt.Token) (interface{}, error) {
					return tv.renewAndGetSigningKey(r, tok)
				})
			}
		}
	}

	if err != nil {
		return nil, jwtErrorToOpenIDError(err)
	}

	return jt, nil
}

func (tv *idTokenValidator) renewAndGetSigningKey(r *http.Request, jt *jwt.Token) (interface{}, error) {
	// Issuer is already validated when 'getSigningKey was called.
	iss := jt.Claims.(jwt.MapClaims)[issuerClaimName].(string)

	err := tv.keyGetter.flushCachedSigningKeys(iss)
	if err != nil {
		return nil, err
	}

	kid := getTokenKid(jt)

	var key []byte
	if key, err = tv.keyGetter.getSigningKey(r, iss, kid); err == nil {
		return tv.rsaParser(key)
	}

	return nil, err
}

func (tv *idTokenValidator) getSigningKey(r *http.Request, jt *jwt.Token) (interface{}, error) {
	provs, err := tv.provGetter()
	if err != nil {
		return nil, err
	}

	if err := providers(provs).validate(); err != nil {
		return nil, err
	}

	p, err := validateIssuer(jt, provs)
	if err != nil {
		return nil, err
	}

	_, err = validateAudiences(jt, p)
	if err != nil {
		return nil, err
	}
	_, err = validateSubject(jt)
	if err != nil {
		return nil, err
	}

	kid := getTokenKid(jt)

	var key []byte
	if key, err = tv.keyGetter.getSigningKey(r, p.Issuer, kid); err == nil {
		return tv.rsaParser(key)
	}

	return nil, err
}

func getTokenKid(jt *jwt.Token) string {
	kid, _ := jt.Header[keyIDJwtHeaderName].(string)
	return kid
}

func validateIssuer(jt *jwt.Token, ps []Provider) (*Provider, error) {
	issuerClaim := getIssuer(jt)
	var ti string

	if iss, ok := issuerClaim.(string); ok {
		ti = iss
	} else {
		return nil, &ValidationError{
			Code:       ValidationErrorInvalidIssuerType,
			Message:    fmt.Sprintf("Invalid Issuer type: %T", issuerClaim),
			HTTPStatus: http.StatusUnauthorized,
		}
	}

	if ti == "" {
		return nil, &ValidationError{
			Code:       ValidationErrorInvalidIssuer,
			Message:    "The token 'iss' claim was not found or was empty.",
			HTTPStatus: http.StatusUnauthorized,
		}
	}

	// Workaround for tokens issued by google
	gi := ti
	if gi == "accounts.google.com" {
		gi = "https://" + gi
	}

	for _, p := range ps {
		if ti == p.Issuer || gi == p.Issuer {
			return &p, nil
		}
	}

	return nil, &ValidationError{
		Code:       ValidationErrorIssuerNotFound,
		Message:    fmt.Sprintf("No provider was registered with issuer: %v", ti),
		HTTPStatus: http.StatusUnauthorized,
	}
}

func validateSubject(jt *jwt.Token) (string, error) {
	subjectClaim := getSubject(jt)

	var ts string
	if sub, ok := subjectClaim.(string); ok {
		ts = sub
	} else {
		return ts, &ValidationError{
			Code:       ValidationErrorInvalidSubjectType,
			Message:    fmt.Sprintf("Invalid subject type: %T", subjectClaim),
			HTTPStatus: http.StatusUnauthorized,
		}
	}

	if ts == "" {
		return ts, &ValidationError{
			Code:       ValidationErrorInvalidSubject,
			Message:    "The token 'sub' claim was not found or was empty.",
			HTTPStatus: http.StatusUnauthorized,
		}
	}

	return ts, nil
}

func validateAudiences(jt *jwt.Token, p *Provider) (string, error) {
	audiencesClaim, err := getAudiences(jt)

	if err != nil {
		return "", err
	}

	for _, aud := range p.ClientIDs {
		for _, audienceClaim := range audiencesClaim {
			ta, ok := audienceClaim.(string)
			if !ok {
				fmt.Printf("aud type %T \n", audienceClaim)
				return "", &ValidationError{
					Code:       ValidationErrorInvalidAudienceType,
					Message:    fmt.Sprintf("Invalid Audiences type: %T", audiencesClaim),
					HTTPStatus: http.StatusUnauthorized,
				}
			}

			if ta == "" {
				return "", &ValidationError{
					Code:       ValidationErrorInvalidAudience,
					Message:    "The token 'aud' claim was not found or was empty.",
					HTTPStatus: http.StatusUnauthorized,
				}
			}

			if ta == aud {
				return ta, nil
			}
		}
	}

	return "", &ValidationError{
		Code:       ValidationErrorAudienceNotFound,
		Message:    fmt.Sprintf("The provider %v does not have a client id matching any of the token audiences %+v", p.Issuer, audiencesClaim),
		HTTPStatus: http.StatusUnauthorized,
	}
}

func getAudiences(t *jwt.Token) ([]interface{}, error) {
	audiencesClaim := t.Claims.(jwt.MapClaims)[audiencesClaimName]
	if aud, ok := audiencesClaim.(string); ok {
		return []interface{}{aud}, nil
	} else if _, ok := audiencesClaim.([]interface{}); ok {
		return audiencesClaim.([]interface{}), nil
	}

	return nil, &ValidationError{
		Code:       ValidationErrorInvalidAudienceType,
		Message:    fmt.Sprintf("Invalid Audiences type: %T", audiencesClaim),
		HTTPStatus: http.StatusUnauthorized,
	}

}

func getIssuer(t *jwt.Token) interface{} {
	return t.Claims.(jwt.MapClaims)[issuerClaimName]
}

func getSubject(t *jwt.Token) interface{} {
	return t.Claims.(jwt.MapClaims)[subjectClaimName]
}
