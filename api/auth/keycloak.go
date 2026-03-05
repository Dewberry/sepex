package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt"
	"github.com/labstack/echo/v4"
	log "github.com/sirupsen/logrus"
)

type PublicKey struct {
	Kid  string   `json:"kid"`
	Kty  string   `json:"kty"`
	Alg  string   `json:"alg"`
	Use  string   `json:"use"`
	N    string   `json:"n"`
	E    string   `json:"e"`
	X5C  []string `json:"x5c"`
	X5T  string   `json:"x5t"`
	S256 string   `json:"x5t#S256"`
}

// KeycloakAuthStrategy implements AuthStrategy for Keycloak authentication.
type KeycloakAuthStrategy struct {
	PublicKeys      map[string]PublicKey
	Mutex           sync.RWMutex
	ServiceRoleName string
}

// NewKeycloakAuthStrategy creates a new instance of KeycloakAuthStrategy and
// starts a background process to refresh the public keys periodically.
func NewKeycloakAuthStrategy() (*KeycloakAuthStrategy, error) {
	strategy := &KeycloakAuthStrategy{
		PublicKeys:      make(map[string]PublicKey),
		ServiceRoleName: os.Getenv("AUTH_SERVICE_ROLE"),
	}

	kcUrl, exist := os.LookupEnv("KEYCLOAK_PUBLIC_KEYS_URL")
	if !exist || kcUrl == "" {
		return nil, errors.New("env variable KEYCLOAK_PUBLIC_KEYS_URL not set")
	}

	err := strategy.LoadPublicKeys()
	if err != nil {
		return nil, err
	}
	go strategy.refreshKeysPeriodically(24 * time.Hour)
	return strategy, nil
}

// refreshKeysPeriodically runs in a goroutine and periodically refreshes
// the public keys used for token validation.
func (kas *KeycloakAuthStrategy) refreshKeysPeriodically(duration time.Duration) {
	for {
		err := kas.LoadPublicKeys()
		if err != nil {
			log.Errorf("Error refreshing public keys: %v\n", err)
			time.Sleep(10 * time.Minute) // Retry after a delay in case of failure
			continue
		}
		time.Sleep(duration)
	}
}

// LoadPublicKeys fetches the public keys from the Keycloak server.
// This method is thread-safe and can be called concurrently.
func (kas *KeycloakAuthStrategy) LoadPublicKeys() error {
	kas.Mutex.Lock()
	defer kas.Mutex.Unlock()

	r, err := http.Get(os.Getenv("KEYCLOAK_PUBLIC_KEYS_URL"))
	if err != nil {
		return err
	}
	defer r.Body.Close()

	var target map[string][]PublicKey
	if err = json.NewDecoder(r.Body).Decode(&target); err != nil {
		return err
	}

	newKeys := make(map[string]PublicKey)
	for _, key := range target["keys"] {
		newKeys[key.Kid] = key
	}
	kas.PublicKeys = newKeys
	return nil
}

// getPublicKeyStr retrieves the public key string for a given 'kid'.
// It returns an empty string if the key is not found.
func (kas *KeycloakAuthStrategy) getPublicKeyStr(kid string) string {
	kas.Mutex.RLock()
	defer kas.Mutex.RUnlock()

	key, ok := kas.PublicKeys[kid]
	if !ok {
		return ""
	}
	return "-----BEGIN CERTIFICATE-----\n" + key.X5C[0] + "\n-----END CERTIFICATE-----"
}

func (kas *KeycloakAuthStrategy) ValidateToken(tokenString string) (*Claims, error) {
	var claims Claims
	token, err := jwt.ParseWithClaims(tokenString, &claims, func(token *jwt.Token) (interface{}, error) {
		if _, ok := token.Method.(*jwt.SigningMethodRSA); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", token.Header["alg"])
		}

		publicKeyStr := kas.getPublicKeyStr(token.Header["kid"].(string))
		if publicKeyStr == "" {
			return nil, fmt.Errorf("public key not found")
		}

		return jwt.ParseRSAPublicKeyFromPEM([]byte(publicKeyStr))
	})

	if err != nil {
		return nil, fmt.Errorf("failed to parse JWT: %v", err)
	}
	if !token.Valid {
		return nil, fmt.Errorf("invalid JWT")
	}

	return &claims, nil
}

// Validate X-SEPEX-User-Email header against user from claims
func (kas *KeycloakAuthStrategy) ValidateUser(c echo.Context, claims *Claims) (err error) {
	roles := claims.RealmAccess["roles"]

	if kas.ServiceRoleName != "" && overlap(roles, []string{kas.ServiceRoleName}) {
		// assume provided header is correct
	} else if claims.Email == "" || !(c.Request().Header.Get("X-SEPEX-User-Email") == claims.Email) {
		return fmt.Errorf("invalid X-SEPEX-User-Email header")
	}

	return nil
}

// Set user roles to API Header
func (kas *KeycloakAuthStrategy) SetUserRolesHeader(c echo.Context, claims *Claims) (err error) {
	roles, exists := claims.RealmAccess["roles"]
	if exists {
		rolesString := strings.Join(roles, ",")
		c.Request().Header.Set("X-SEPEX-User-Roles", rolesString)
	} else {
		return nil
	}

	return nil
}
