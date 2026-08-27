// secrets.go — secret persistant local + chiffrement AES-GCM des données sensibles
// au repos (mots de passe des instances distantes). AUCUN changement de compose :
// le secret est auto-généré dans /config/secret.key (0600) au premier lancement.
// Surchargeable par SB_SECRET si tu préfères le gérer toi-même.
package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"io"
	"os"
	"path/filepath"
	"strings"
)

var secretKey []byte

// loadSecret : SB_SECRET prioritaire (dérivé SHA-256), sinon /config/secret.key
// (généré au premier lancement, 32 octets aléatoires).
func loadSecret() {
	if s := os.Getenv("SB_SECRET"); s != "" {
		h := sha256.Sum256([]byte(s))
		secretKey = h[:]
		return
	}
	p := filepath.Join(dataDir, "secret.key")
	if b, err := os.ReadFile(p); err == nil && len(b) >= 32 {
		secretKey = b[:32]
		return
	}
	b := make([]byte, 32)
	rand.Read(b)
	if err := os.WriteFile(p, b, 0600); err == nil {
		own(p)
	}
	secretKey = b
}

const encPrefix = "enc:v1:"

// encrypt : "enc:v1:<base64(nonce|ciphertext)>". Best-effort : renvoie le clair si
// le chiffrement échoue (jamais de perte de donnée), mais ça n'arrive pas en pratique.
func encrypt(plain string) string {
	if plain == "" || secretKey == nil {
		return plain
	}
	gcm := gcmCipher()
	if gcm == nil {
		return plain
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return plain
	}
	ct := gcm.Seal(nonce, nonce, []byte(plain), nil)
	return encPrefix + base64.StdEncoding.EncodeToString(ct)
}

// decrypt : inverse d'encrypt. Sans le préfixe -> renvoie tel quel (rétrocompat avec
// les valeurs stockées en clair avant l'ajout du chiffrement).
func decrypt(s string) string {
	if !strings.HasPrefix(s, encPrefix) || secretKey == nil {
		return s
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(s, encPrefix))
	if err != nil {
		return s
	}
	gcm := gcmCipher()
	if gcm == nil || len(raw) < gcm.NonceSize() {
		return s
	}
	nonce, ct := raw[:gcm.NonceSize()], raw[gcm.NonceSize():]
	pt, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return s
	}
	return string(pt)
}

func gcmCipher() cipher.AEAD {
	block, err := aes.NewCipher(secretKey)
	if err != nil {
		return nil
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil
	}
	return gcm
}
